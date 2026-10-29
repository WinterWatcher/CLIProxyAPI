package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cost"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const nativeRouteMetadataKey = "openai_compat_native_route"
const nativeContentTypeMetadataKey = "openai_compat_native_content_type"
const nativeBodyLimit int64 = 64 << 20

type nativeRouteError interface {
	error
	StatusCode() int
	ResponseHeaders() http.Header
	ResponseBody() []byte
}

func (h *OpenAIAPIHandler) getFullConfig() *config.Config {
	if h == nil || h.BaseAPIHandler == nil {
		return nil
	}
	return h.FullConfig
}

func (h *OpenAIAPIHandler) NativeOpenAICompat(c *gin.Context, capability string) {
	cfg := h.getFullConfig()
	if cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "configuration unavailable"})
		return
	}
	contentType := c.GetHeader("Content-Type")
	var body []byte
	var requested string
	if strings.HasPrefix(strings.ToLower(contentType), "multipart/") {
		var ok bool
		body, requested, ok = readNativeMultipart(c)
		if !ok {
			return
		}
	} else {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, nativeBodyLimit)
		var err error
		body, err = io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request body too large"})
			return
		}
		var payload map[string]any
		if err = json.Unmarshal(body, &payload); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid request: %v", err)})
			return
		}
		requested, _ = payload["model"].(string)
		provider, upstream, ok := nativeProviderModel(cfg, requested, capability)
		if !ok {
			if found, otherCap := nativeModelCapability(cfg, requested); found && otherCap != capability {
				c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("model %q does not support capability %q (has %q)", requested, capability, otherCap)})
				return
			}
			c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("model %q not found", requested)})
			return
		}
		payload["model"] = upstream
		body, _ = json.Marshal(payload)
		h.executeNative(c, cfg, provider, requested, upstream, capability, body, contentType)
		return
	}
	provider, upstream, ok := nativeProviderModel(cfg, requested, capability)
	if !ok {
		if found, otherCap := nativeModelCapability(cfg, requested); found && otherCap != capability {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("model %q does not support capability %q (has %q)", requested, capability, otherCap)})
			return
		}
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("model %q not found", requested)})
		return
	}
	body, rewriteErr := rewriteMultipartModel(body, contentType, upstream)
	if rewriteErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid multipart body"})
		return
	}
	h.executeNative(c, cfg, provider, requested, upstream, capability, body, contentType)
}

func (h *OpenAIAPIHandler) executeNative(c *gin.Context, cfg *config.Config, provider, requested, model, capability string, body []byte, contentType string) {
	var reservationKey string
	bStore := h.GetBudgetStore()
	var filterRes cost.ProviderFilterResult
	execCtx := context.WithValue(c.Request.Context(), nativeCapabilityContextKey{}, capability)

	if cfg != nil && cfg.CostGuard.Enabled {
		measurement := cost.ExtractNativeMeasurement(capability, body, contentType)
		filterRes = cost.FilterCandidateProviders(cost.ProviderFilterRequest{
			Config:      cfg,
			Providers:   []string{provider},
			Model:       requested,
			Capability:  capability,
			Measurement: measurement,
			RawPayload:  body,
			ContentType: contentType,
			BudgetStore: bStore,
		})
		if len(filterRes.AllowedProviders) == 0 {
			primaryReason := filterRes.PrimaryDecision.Reason
			if primaryReason == "" {
				primaryReason = cost.ReasonUnknownPrice
			}
			errBody, status := cost.BuildGateErrorResponse(cost.GateDecision{
				Allowed:             false,
				Decision:            cost.DecisionDeny,
				Code:                cost.CodePricePolicyViolation,
				Reason:              primaryReason,
				Provider:            filterRes.PrimaryDecision.Provider,
				Model:               requested,
				BillingUnit:         filterRes.PrimaryDecision.BillingUnit,
				EstimatedCost:       filterRes.PrimaryDecision.EstimatedCost,
				MaxRequestCostNanos: filterRes.PrimaryDecision.MaxRequestCostNanos,
			})
			c.Data(status, "application/json", errBody)
			return
		}

		provider = filterRes.AllowedProviders[0]
		provPolicy, primaryModelPol, _, _ := cost.ResolveModelPolicy(cfg, provider, requested, capability)
		guard := cost.NewFallbackGuard(
			cfg,
			provider,
			requested,
			capability,
			filterRes.PrimaryDecision.EstimatedCost,
			primaryModelPol,
			filterRes.AllowedProviders,
			measurement,
			"",
		)
		execCtx = cost.WithFallbackGuard(execCtx, guard)

		if bStore != nil && filterRes.PrimaryDecision.EstimatedCost.AmountNanos > 0 {
			reservationKey = fmt.Sprintf("native-%s-%d", requested, time.Now().UnixNano())
			var dailyLimit, monthlyLimit int64
			if provPolicy != nil {
				dailyLimit = int64(math.Round(provPolicy.ProviderBudget.DailyUSD * float64(cost.NanosPerUSD)))
				monthlyLimit = int64(math.Round(provPolicy.ProviderBudget.MonthlyUSD * float64(cost.NanosPerUSD)))
			}
			_, errReserve := bStore.Reserve(execCtx, cost.ReservationRequest{
				ReservationKey:    reservationKey,
				Provider:          provider,
				Model:             requested,
				BillingUnit:       filterRes.PrimaryDecision.BillingUnit,
				AmountNanos:       filterRes.PrimaryDecision.EstimatedCost.AmountNanos,
				DailyLimitNanos:   dailyLimit,
				MonthlyLimitNanos: monthlyLimit,
			})
			if errReserve != nil {
				errBody, status := cost.BuildGateErrorResponse(cost.GateDecision{
					Allowed:     false,
					Decision:    cost.DecisionDeny,
					Code:        cost.CodePricePolicyViolation,
					Reason:      cost.ReasonProviderBudgetExceeded,
					Provider:    provider,
					Model:       requested,
					BillingUnit: filterRes.PrimaryDecision.BillingUnit,
				})
				c.Data(status, "application/json", errBody)
				return
			}
		}
	}

	compat := nativeCompat(cfg, provider)
	route := nativeRoute(compat, capability)
	providerKey := util.OpenAICompatibleProviderKey(strings.ToLower(strings.TrimSpace(provider)))
	headers := c.Request.Header.Clone()
	headers.Del("Authorization")
	headers.Del("Proxy-Authorization")
	opts := coreexecutor.Options{
		SourceFormat:    "openai",
		Headers:         headers,
		OriginalRequest: body,
		Metadata: map[string]any{
			nativeRouteMetadataKey:       route,
			nativeContentTypeMetadataKey: contentType,
		},
	}
	start := time.Now()
	// Select credentials with the public model ID registered in the auth pool.
	// The payload already contains the provider's upstream model ID.
	resp, err := h.AuthManager.Execute(execCtx, []string{providerKey}, coreexecutor.Request{Model: requested, Payload: body, Format: "openai"}, opts)
	emitNativeUsageRecord(c, provider, requested, model, capability, body, resp.Payload, start, err)
	if err != nil {
		if bStore != nil && reservationKey != "" {
			_ = bStore.Release(execCtx, reservationKey, "request_failed")
		}
		if re, ok := err.(nativeRouteError); ok {
			cleanBody := sanitizeNativeErrorBody(re.ResponseBody(), compat, re.StatusCode())
			writeNativeResponse(c, re.StatusCode(), re.ResponseHeaders(), cleanBody)
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": "upstream request failed"})
		return
	}
	if bStore != nil && reservationKey != "" {
		_ = bStore.Settle(execCtx, reservationKey, filterRes.PrimaryDecision.EstimatedCost.AmountNanos, 0, 0)
	}
	cleanPayload := sanitizeNativeErrorBody(resp.Payload, compat, http.StatusOK)
	writeNativeResponse(c, http.StatusOK, resp.Headers, cleanPayload)
}

func emitNativeUsageRecord(c *gin.Context, provider, requestedModel, model, capability string, reqBody, respBody []byte, start time.Time, execErr error) {
	latency := time.Since(start)
	billingUnit := "tokens"
	var quantity, inQty, outQty float64
	var detail coreusage.Detail

	switch capability {
	case "embedding", "rerank":
		billingUnit = "tokens"
		inTokens := gjson.GetBytes(respBody, "usage.prompt_tokens").Int()
		if inTokens == 0 {
			inTokens = gjson.GetBytes(respBody, "usage.total_tokens").Int()
		}
		if inTokens == 0 {
			inTokens = gjson.GetBytes(respBody, "meta.tokens.input_tokens").Int()
		}
		if inTokens > 0 {
			quantity = float64(inTokens)
			inQty = float64(inTokens)
			detail.InputTokens = inTokens
			detail.TotalTokens = inTokens
		}
	case "asr":
		billingUnit = "audio_seconds"
		duration := gjson.GetBytes(respBody, "duration").Float()
		if duration == 0 {
			duration = gjson.GetBytes(respBody, "usage.seconds").Float()
		}
		if duration > 0 {
			quantity = duration
			inQty = duration
		}
	case "tts", "speech":
		billingUnit = "characters"
		chars := float64(utf8.RuneCountInString(gjson.GetBytes(reqBody, "input").String()))
		if chars > 0 {
			quantity = chars
			inQty = chars
		}
	case "image":
		billingUnit = "images"
		imgCount := gjson.GetBytes(respBody, "data.#").Int()
		if imgCount == 0 {
			imgCount = gjson.GetBytes(reqBody, "n").Int()
		}
		if imgCount == 0 {
			imgCount = 1
		}
		quantity = float64(imgCount)
		outQty = float64(imgCount)
	case "video":
		billingUnit = "video_tasks"
		quantity = 1
		outQty = 1
	case "video_status":
		billingUnit = "video_status"
		quantity = 0
		outQty = 0
	}

	record := coreusage.Record{
		RequestID:      uuid.NewString(),
		TraceID:        c.GetString("requestId"),
		Provider:       provider,
		Model:          model,
		Alias:          requestedModel,
		Capability:     capability,
		BillingUnit:    billingUnit,
		Quantity:       quantity,
		InputQuantity:  inQty,
		OutputQuantity: outQty,
		RequestedAt:    start,
		Latency:        latency,
		Failed:         execErr != nil,
		Detail:         detail,
	}
	if execErr != nil {
		if re, ok := execErr.(nativeRouteError); ok {
			record.Fail = coreusage.Failure{StatusCode: re.StatusCode(), Body: string(re.ResponseBody())}
		} else {
			record.Fail = coreusage.Failure{StatusCode: http.StatusBadGateway, Body: execErr.Error()}
		}
	}
	coreusage.PublishRecord(c.Request.Context(), record)
}

type nativeCapabilityContextKey struct{}

func (h *OpenAIAPIHandler) Embeddings(c *gin.Context)          { h.NativeOpenAICompat(c, "embedding") }
func (h *OpenAIAPIHandler) Rerank(c *gin.Context)              { h.NativeOpenAICompat(c, "rerank") }
func (h *OpenAIAPIHandler) AudioTranscriptions(c *gin.Context) { h.NativeOpenAICompat(c, "asr") }

type nativeVideoTask struct {
	Provider      string
	PublicModel   string
	UpstreamModel string
	AuthID        string
	CreatedAt     time.Time
	ExpiresAt     time.Time
}

type nativeVideoTaskStore struct {
	mu    sync.RWMutex
	tasks map[string]nativeVideoTask
}

var globalNativeVideoTasks = &nativeVideoTaskStore{
	tasks: make(map[string]nativeVideoTask),
}

func (s *nativeVideoTaskStore) Set(requestID, provider, publicModel, upstreamModel, authID string) {
	if s == nil || strings.TrimSpace(requestID) == "" {
		return
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[strings.TrimSpace(requestID)] = nativeVideoTask{
		Provider:      provider,
		PublicModel:   publicModel,
		UpstreamModel: upstreamModel,
		AuthID:        authID,
		CreatedAt:     now,
		ExpiresAt:     now.Add(24 * time.Hour),
	}
}

func (s *nativeVideoTaskStore) Get(requestID string) (nativeVideoTask, bool) {
	if s == nil {
		return nativeVideoTask{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.tasks[strings.TrimSpace(requestID)]
	if !ok || time.Now().After(t.ExpiresAt) {
		return nativeVideoTask{}, false
	}
	return t, true
}

func (h *OpenAIAPIHandler) VideoSubmit(c *gin.Context) {
	cfg := h.getFullConfig()
	if cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "configuration unavailable"})
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, nativeBodyLimit)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request body too large"})
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid request: %v", err)})
		return
	}
	model, _ := payload["model"].(string)
	model = strings.TrimSpace(model)
	if model == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing model"})
		return
	}
	provider, upstream, ok := nativeProviderModel(cfg, model, "video")
	if !ok {
		if found, actualCap := nativeModelCapability(cfg, model); found && actualCap != "video" {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("model %q does not support capability video (has %q)", model, actualCap)})
			return
		}
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("model %q not found", model)})
		return
	}
	payload["model"] = upstream
	body, _ = json.Marshal(payload)

	var reservationKey string
	bStore := h.GetBudgetStore()
	var filterRes cost.ProviderFilterResult
	execCtx := context.WithValue(c.Request.Context(), nativeCapabilityContextKey{}, "video")

	if cfg != nil && cfg.CostGuard.Enabled {
		filterRes = cost.FilterCandidateProviders(cost.ProviderFilterRequest{
			Config:      cfg,
			Providers:   []string{provider},
			Model:       model,
			Capability:  "video",
			Measurement: cost.FromVideoSubmit(1),
			RawPayload:  body,
			BudgetStore: bStore,
		})
		if len(filterRes.AllowedProviders) == 0 {
			primaryReason := filterRes.PrimaryDecision.Reason
			if primaryReason == "" {
				primaryReason = cost.ReasonUnknownPrice
			}
			errBody, status := cost.BuildGateErrorResponse(cost.GateDecision{
				Allowed:             false,
				Decision:            cost.DecisionDeny,
				Code:                cost.CodePricePolicyViolation,
				Reason:              primaryReason,
				Provider:            filterRes.PrimaryDecision.Provider,
				Model:               model,
				BillingUnit:         filterRes.PrimaryDecision.BillingUnit,
				EstimatedCost:       filterRes.PrimaryDecision.EstimatedCost,
				MaxRequestCostNanos: filterRes.PrimaryDecision.MaxRequestCostNanos,
			})
			c.Data(status, "application/json", errBody)
			return
		}

		provider = filterRes.AllowedProviders[0]
		provPolicy, primaryModelPol, _, _ := cost.ResolveModelPolicy(cfg, provider, model, "video")
		guard := cost.NewFallbackGuard(
			cfg,
			provider,
			model,
			"video",
			filterRes.PrimaryDecision.EstimatedCost,
			primaryModelPol,
			filterRes.AllowedProviders,
			cost.FromVideoSubmit(1),
			"",
		)
		execCtx = cost.WithFallbackGuard(execCtx, guard)

		if bStore != nil && filterRes.PrimaryDecision.EstimatedCost.AmountNanos > 0 {
			reservationKey = fmt.Sprintf("native-video-%s-%d", model, time.Now().UnixNano())
			var dailyLimit, monthlyLimit int64
			if provPolicy != nil {
				dailyLimit = int64(math.Round(provPolicy.ProviderBudget.DailyUSD * float64(cost.NanosPerUSD)))
				monthlyLimit = int64(math.Round(provPolicy.ProviderBudget.MonthlyUSD * float64(cost.NanosPerUSD)))
			}
			_, errReserve := bStore.Reserve(execCtx, cost.ReservationRequest{
				ReservationKey:    reservationKey,
				Provider:          provider,
				Model:             model,
				BillingUnit:       filterRes.PrimaryDecision.BillingUnit,
				AmountNanos:       filterRes.PrimaryDecision.EstimatedCost.AmountNanos,
				DailyLimitNanos:   dailyLimit,
				MonthlyLimitNanos: monthlyLimit,
			})
			if errReserve != nil {
				errBody, status := cost.BuildGateErrorResponse(cost.GateDecision{
					Allowed:     false,
					Decision:    cost.DecisionDeny,
					Code:        cost.CodePricePolicyViolation,
					Reason:      cost.ReasonProviderBudgetExceeded,
					Provider:    provider,
					Model:       model,
					BillingUnit: filterRes.PrimaryDecision.BillingUnit,
				})
				c.Data(status, "application/json", errBody)
				return
			}
		}
	}

	compat := nativeCompat(cfg, provider)
	route := nativeRoute(compat, "video_submit")
	providerKey := util.OpenAICompatibleProviderKey(strings.ToLower(strings.TrimSpace(provider)))
	headers := c.Request.Header.Clone()
	headers.Del("Authorization")
	headers.Del("Proxy-Authorization")
	opts := coreexecutor.Options{
		SourceFormat:    "openai",
		Headers:         headers,
		OriginalRequest: body,
		Metadata: map[string]any{
			nativeRouteMetadataKey:       route,
			nativeContentTypeMetadataKey: "application/json",
		},
	}
	start := time.Now()
	resp, err := h.AuthManager.Execute(execCtx, []string{providerKey}, coreexecutor.Request{Model: model, Payload: body, Format: "openai"}, opts)
	emitNativeUsageRecord(c, provider, model, upstream, "video", body, resp.Payload, start, err)
	if err != nil {
		if bStore != nil && reservationKey != "" {
			_ = bStore.Release(execCtx, reservationKey, "request_failed")
		}
		if re, ok := err.(nativeRouteError); ok {
			cleanBody := sanitizeNativeErrorBody(re.ResponseBody(), compat, re.StatusCode())
			writeNativeResponse(c, re.StatusCode(), re.ResponseHeaders(), cleanBody)
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": "upstream request failed"})
		return
	}
	if bStore != nil && reservationKey != "" {
		_ = bStore.Settle(execCtx, reservationKey, filterRes.PrimaryDecision.EstimatedCost.AmountNanos, 0, 0)
	}

	reqID := strings.TrimSpace(gjson.GetBytes(resp.Payload, "requestId").String())
	if reqID == "" {
		reqID = strings.TrimSpace(gjson.GetBytes(resp.Payload, "request_id").String())
	}
	if reqID == "" {
		reqID = strings.TrimSpace(gjson.GetBytes(resp.Payload, "task_id").String())
	}
	if reqID == "" {
		reqID = strings.TrimSpace(gjson.GetBytes(resp.Payload, "id").String())
	}

	outPayload := sanitizeNativeErrorBody(resp.Payload, compat, http.StatusOK)
	if reqID != "" {
		if !gjson.GetBytes(outPayload, "requestId").Exists() {
			outPayload, _ = sjson.SetBytes(outPayload, "requestId", reqID)
		}
		authID := ""
		if resp.Metadata != nil {
			authID, _ = resp.Metadata[coreexecutor.SelectedAuthMetadataKey].(string)
		}
		globalNativeVideoTasks.Set(reqID, provider, model, upstream, authID)
	}
	writeNativeResponse(c, http.StatusOK, resp.Headers, outPayload)
}

func (h *OpenAIAPIHandler) VideoStatus(c *gin.Context) {
	cfg := h.getFullConfig()
	if cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "configuration unavailable"})
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, nativeBodyLimit)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request body too large"})
		return
	}
	reqID := strings.TrimSpace(gjson.GetBytes(body, "requestId").String())
	if reqID == "" {
		reqID = strings.TrimSpace(gjson.GetBytes(body, "request_id").String())
	}
	if reqID == "" {
		reqID = strings.TrimSpace(gjson.GetBytes(body, "task_id").String())
	}
	if reqID == "" {
		reqID = strings.TrimSpace(gjson.GetBytes(body, "id").String())
	}
	if reqID == "" {
		reqID = strings.TrimSpace(c.Query("requestId"))
	}
	if reqID == "" {
		reqID = strings.TrimSpace(c.Query("request_id"))
	}
	if reqID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing requestId"})
		return
	}
	task, found := globalNativeVideoTasks.Get(reqID)
	if !found {
		var ok bool
		task, ok = nativeVideoTaskFromStatusRequest(cfg, body)
		if !ok {
			c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("video task %q not found; include model when multiple video providers are configured", reqID)})
			return
		}
	}
	compat := nativeCompat(cfg, task.Provider)
	if compat == nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "provider not found"})
		return
	}
	route := nativeRoute(compat, "video_status")
	route = strings.ReplaceAll(route, ":request_id", reqID)
	route = strings.ReplaceAll(route, ":requestId", reqID)
	route = strings.ReplaceAll(route, "{requestId}", reqID)
	route = strings.ReplaceAll(route, "{id}", reqID)

	if len(body) == 0 || !gjson.GetBytes(body, "requestId").Exists() {
		body, _ = sjson.SetBytes(body, "requestId", reqID)
	}

	providerKey := util.OpenAICompatibleProviderKey(strings.ToLower(strings.TrimSpace(task.Provider)))
	headers := c.Request.Header.Clone()
	headers.Del("Authorization")
	headers.Del("Proxy-Authorization")
	opts := coreexecutor.Options{
		SourceFormat:    "openai",
		Headers:         headers,
		OriginalRequest: body,
		Metadata: map[string]any{
			nativeRouteMetadataKey:       route,
			nativeContentTypeMetadataKey: "application/json",
		},
	}
	if cfg != nil && cfg.CostGuard.Enabled {
		dec := cost.EvaluatePreflight(cost.GateRequest{
			Config:      cfg,
			Provider:    task.Provider,
			Model:       task.PublicModel,
			Capability:  "video_status",
			Measurement: cost.FromVideoStatus(),
			RawPayload:  body,
		})
		if !dec.Allowed {
			errBody, status := cost.BuildGateErrorResponse(dec)
			c.Data(status, "application/json", errBody)
			return
		}
	}

	start := time.Now()
	resp, err := h.AuthManager.Execute(context.WithValue(c.Request.Context(), nativeCapabilityContextKey{}, "video"), []string{providerKey}, coreexecutor.Request{Model: task.PublicModel, Payload: body, Format: "openai"}, opts)
	emitNativeUsageRecord(c, task.Provider, task.PublicModel, task.UpstreamModel, "video_status", body, resp.Payload, start, err)
	if err != nil {
		if re, ok := err.(nativeRouteError); ok {
			cleanBody := sanitizeNativeErrorBody(re.ResponseBody(), compat, re.StatusCode())
			writeNativeResponse(c, re.StatusCode(), re.ResponseHeaders(), cleanBody)
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": "upstream request failed"})
		return
	}
	cleanPayload := sanitizeNativeErrorBody(resp.Payload, compat, http.StatusOK)
	writeNativeResponse(c, http.StatusOK, resp.Headers, cleanPayload)
}

func nativeVideoTaskFromStatusRequest(cfg *config.Config, body []byte) (nativeVideoTask, bool) {
	requested := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if requested == "" {
		return nativeVideoTask{}, false
	}
	provider, upstream, ok := nativeProviderModel(cfg, requested, "video")
	if !ok {
		return nativeVideoTask{}, false
	}
	return nativeVideoTask{Provider: provider, PublicModel: requested, UpstreamModel: upstream}, true
}

func sanitizeNativeErrorBody(body []byte, compat *config.OpenAICompatibility, status int) []byte {
	if status < 400 {
		return body
	}
	text := string(body)
	if compat != nil {
		if b := strings.TrimSpace(compat.BaseURL); b != "" {
			text = strings.ReplaceAll(text, b, "[REDACTED_URL]")
			if u, err := url.Parse(b); err == nil && u.Host != "" {
				text = strings.ReplaceAll(text, u.Host, "[REDACTED_HOST]")
			}
		}
		for _, e := range compat.APIKeyEntries {
			if k := strings.TrimSpace(e.APIKey); k != "" {
				text = strings.ReplaceAll(text, k, "[REDACTED_KEY]")
			}
			if p := strings.TrimSpace(e.ProxyURL); p != "" {
				text = strings.ReplaceAll(text, p, "[REDACTED_PROXY]")
				if u, err := url.Parse(p); err == nil && u.Host != "" {
					text = strings.ReplaceAll(text, u.Host, "[REDACTED_PROXY]")
				}
			}
		}
		if n := strings.TrimSpace(compat.Name); n != "" {
			text = strings.ReplaceAll(text, n, "[REDACTED_PROVIDER]")
		}
	}
	text = regexp.MustCompile(`(/[a-zA-Z0-9_\-\.]+)+(/[a-zA-Z0-9_\-\.]+\.[a-zA-Z0-9]+|/[a-zA-Z0-9_\-\.]+)`).ReplaceAllString(text, "[REDACTED_PATH]")
	text = regexp.MustCompile(`[a-zA-Z]:\\[a-zA-Z0-9_\\\-\.]+\.[a-zA-Z0-9]+|[a-zA-Z]:\\[a-zA-Z0-9_\\\-\.]+`).ReplaceAllString(text, "[REDACTED_PATH]")
	text = regexp.MustCompile(`https?://(10\.\d+\.\d+\.\d+|192\.168\.\d+\.\d+|172\.(1[6-9]|2\d|3[01])\.\d+\.\d+|127\.0\.0\.1|localhost)(:\d+)?`).ReplaceAllString(text, "[REDACTED_IP]")

	if status >= 400 && !json.Valid([]byte(text)) {
		res, _ := json.Marshal(gin.H{
			"error": gin.H{
				"message": strings.TrimSpace(text),
				"type":    "upstream_error",
				"code":    status,
			},
		})
		return res
	}
	return []byte(text)
}

func nativeCompat(cfg *config.Config, provider string) *config.OpenAICompatibility {
	for i := range cfg.OpenAICompatibility {
		if strings.EqualFold(cfg.OpenAICompatibility[i].Name, provider) {
			return &cfg.OpenAICompatibility[i]
		}
	}
	return nil
}

func nativeModelCapability(cfg *config.Config, requested string) (found bool, cap string) {
	if cfg == nil {
		return false, ""
	}
	requested = strings.TrimSpace(requested)
	for _, p := range cfg.OpenAICompatibility {
		if p.Disabled {
			continue
		}
		for _, m := range p.Models {
			if requested == m.Alias || requested == m.Name {
				c := strings.ToLower(strings.TrimSpace(m.Capability))
				if c == "" {
					c = strings.ToLower(strings.TrimSpace(m.Type))
				}
				if c == "" && m.Image {
					c = "image"
				}
				if c == "" {
					c = "chat"
				}
				return true, c
			}
		}
	}
	return false, ""
}

func nativeProviderModel(cfg *config.Config, requested, capability string) (string, string, bool) {
	if cfg == nil {
		return "", "", false
	}
	requested = strings.TrimSpace(requested)
	for _, p := range cfg.OpenAICompatibility {
		if p.Disabled {
			continue
		}
		for _, m := range p.Models {
			cap := strings.ToLower(strings.TrimSpace(m.Capability))
			if cap == "" {
				cap = strings.ToLower(strings.TrimSpace(m.Type))
			}
			if cap == "" && m.Image && capability == "image" {
				cap = "image"
			}
			if (capability == "tts" || capability == "speech") && (cap == "tts" || cap == "speech") {
				cap = capability
			}
			if cap != capability || (requested != m.Alias && requested != m.Name) {
				continue
			}
			return p.Name, m.Name, true
		}
	}
	return "", "", false
}

func nativeRoute(p *config.OpenAICompatibility, capability string) string {
	defaults := map[string]string{
		"embedding":    "/embeddings",
		"rerank":       "/rerank",
		"asr":          "/audio/transcriptions",
		"tts":          "/audio/speech",
		"speech":       "/audio/speech",
		"image":        "/images/generations",
		"video_submit": "/video/submit",
		"video_status": "/video/status",
	}
	if p != nil && p.Routes != nil {
		if v := strings.TrimSpace(p.Routes[capability]); v != "" {
			return v
		}
		switch capability {
		case "asr":
			if v := strings.TrimSpace(p.Routes["audio/transcriptions"]); v != "" {
				return v
			}
		case "embedding":
			if v := strings.TrimSpace(p.Routes["embeddings"]); v != "" {
				return v
			}
		case "tts", "speech":
			if v := strings.TrimSpace(p.Routes["audio/speech"]); v != "" {
				return v
			}
			if v := strings.TrimSpace(p.Routes["audio_speech"]); v != "" {
				return v
			}
			if v := strings.TrimSpace(p.Routes["speech"]); v != "" {
				return v
			}
			if v := strings.TrimSpace(p.Routes["tts"]); v != "" {
				return v
			}
		case "image":
			if v := strings.TrimSpace(p.Routes["images/generations"]); v != "" {
				return v
			}
			if v := strings.TrimSpace(p.Routes["images"]); v != "" {
				return v
			}
		case "video_submit":
			if v := strings.TrimSpace(p.Routes["video/submit"]); v != "" {
				return v
			}
			if v := strings.TrimSpace(p.Routes["video"]); v != "" {
				return v
			}
		case "video_status":
			if v := strings.TrimSpace(p.Routes["video/status"]); v != "" {
				return v
			}
		}
	}
	return defaults[capability]
}

func writeNativeResponse(c *gin.Context, status int, headers http.Header, body []byte) {
	for _, k := range []string{"Content-Type", "Content-Encoding", "Retry-After"} {
		if v := headers.Get(k); v != "" {
			c.Header(k, v)
		}
	}
	c.Status(status)
	_, _ = c.Writer.Write(body)
}

func readNativeMultipart(c *gin.Context) ([]byte, string, bool) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, nativeBodyLimit)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request body too large"})
		return nil, "", false
	}
	ct, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid multipart content type"})
		return nil, "", false
	}
	_, params, _ := mime.ParseMediaType(c.GetHeader("Content-Type"))
	r := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	for {
		part, e := r.NextPart()
		if e == io.EOF {
			break
		}
		if e != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid multipart body"})
			return nil, "", false
		}
		if part.FormName() == "model" {
			b, _ := io.ReadAll(part)
			return body, string(b), true
		}
		_ = ct
	}
	c.JSON(http.StatusBadRequest, gin.H{"error": "missing model"})
	return nil, "", false
}

func rewriteMultipartModel(body []byte, contentType, model string) ([]byte, error) {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return body, err
	}
	r := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	var out bytes.Buffer
	w := multipart.NewWriter(&out)
	_ = w.SetBoundary(params["boundary"])
	for {
		part, e := r.NextPart()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, e
		}
		data, e := io.ReadAll(io.LimitReader(part, nativeBodyLimit))
		if e != nil {
			return nil, e
		}
		if part.FormName() == "model" {
			data = []byte(model)
		}
		h := make(textproto.MIMEHeader, len(part.Header))
		for k, values := range part.Header {
			h[k] = append([]string(nil), values...)
		}
		dst, e := w.CreatePart(h)
		if e != nil {
			return nil, e
		}
		_, _ = dst.Write(data)
	}
	if err = w.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
