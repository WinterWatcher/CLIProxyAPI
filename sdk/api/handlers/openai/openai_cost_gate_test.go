package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cost"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type gateTestFixture struct {
	router         *gin.Engine
	upstreamServer *httptest.Server
	upstreamCalls  *int64
	fallbackServer *httptest.Server
	fallbackCalls  *int64
	auditSink      *cost.MemoryCostAuditSink
}

func setupGateTestEnv(t *testing.T, cfg *config.Config) *gateTestFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)

	var upstreamCalls int64
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&upstreamCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"mock-upstream-resp","choices":[{"message":{"role":"assistant","content":"hello"}}]}`)
	}))
	t.Cleanup(upstreamServer.Close)

	var fallbackCalls int64
	fallbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&fallbackCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"mock-fallback-resp"}`)
	}))
	t.Cleanup(fallbackServer.Close)

	// Inject upstream URLs into config
	for i := range cfg.OpenAICompatibility {
		if cfg.OpenAICompatibility[i].Name == "fallback-prov" {
			cfg.OpenAICompatibility[i].BaseURL = fallbackServer.URL
		} else {
			cfg.OpenAICompatibility[i].BaseURL = upstreamServer.URL
		}
	}

	authMgr := coreauth.NewManager(nil, nil, nil)
	modelRegistry := registry.GetGlobalRegistry()

	for _, p := range cfg.OpenAICompatibility {
		pKey := util.OpenAICompatibleProviderKey(p.Name)
		exec := runtimeexecutor.NewOpenAICompatExecutor(pKey, cfg)
		authMgr.RegisterExecutor(exec)
		for idx, keyEntry := range p.APIKeyEntries {
			authID := fmt.Sprintf("%s-auth-%d", p.Name, idx)
			auth := &coreauth.Auth{
				ID:       authID,
				Provider: pKey,
				Attributes: map[string]string{
					"api_key":  keyEntry.APIKey,
					"base_url": p.BaseURL,
				},
			}
			_, _ = authMgr.Register(context.Background(), auth)

			var regModels []*registry.ModelInfo
			for _, m := range p.Models {
				publicModel := m.Alias
				if publicModel == "" {
					publicModel = m.Name
				}
				regModels = append(regModels, &registry.ModelInfo{ID: publicModel})
			}
			modelRegistry.RegisterClient(authID, pKey, regModels)
			t.Cleanup(func() {
				modelRegistry.UnregisterClient(authID)
			})
		}
	}

	baseHandler := &handlers.BaseAPIHandler{
		AuthManager: authMgr,
		FullConfig:  cfg,
	}
	apiHandler := &OpenAIAPIHandler{
		BaseAPIHandler: baseHandler,
	}

	router := gin.New()
	v1 := router.Group("/v1")
	v1.POST("/chat/completions", apiHandler.ChatCompletions)
	v1.POST("/embeddings", apiHandler.Embeddings)
	v1.POST("/rerank", apiHandler.Rerank)
	v1.POST("/audio/transcriptions", apiHandler.AudioTranscriptions)
	v1.POST("/audio/speech", apiHandler.AudioSpeech)
	v1.POST("/images/generations", apiHandler.ImagesGenerations)
	v1.POST("/video/submit", apiHandler.VideoSubmit)
	v1.POST("/video/status", apiHandler.VideoStatus)

	auditSink := cost.NewMemoryCostAuditSink(100)
	cost.SetGlobalAuditSink(auditSink)

	return &gateTestFixture{
		router:         router,
		upstreamServer: upstreamServer,
		upstreamCalls:  &upstreamCalls,
		fallbackServer: fallbackServer,
		fallbackCalls:  &fallbackCalls,
		auditSink:      auditSink,
	}
}

func TestCostGateChatCompletionsUnknownPriceDenied(t *testing.T) {
	cfg := &config.Config{
		CostGuard: config.CostGuardConfig{
			Enabled:            true,
			UnknownPriceAction: config.CostActionDeny,
		},
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name: "prov-chat",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{
					{APIKey: "sk-super-secret-key-111"},
				},
				Models: []config.OpenAICompatibilityModel{
					{Name: "unpriced-model", Alias: "unpriced-model"}, // no cost policy => unknown_price
				},
			},
		},
	}

	fix := setupGateTestEnv(t, cfg)

	reqBody := `{"model":"unpriced-model","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	fix.router.ServeHTTP(w, req)

	// Must be blocked with 402 Payment Required
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("expected HTTP 402, got %d with body: %s", w.Code, w.Body.String())
	}

	// Upstream must have received ZERO requests!
	if calls := atomic.LoadInt64(fix.upstreamCalls); calls != 0 {
		t.Fatalf("expected 0 upstream calls, got %d", calls)
	}

	// Verify error response format and no secret leakage
	respStr := w.Body.String()
	if !strings.Contains(respStr, "price_policy_violation") || !strings.Contains(respStr, "unknown_price") {
		t.Fatalf("unexpected error response content: %s", respStr)
	}
	if strings.Contains(respStr, "sk-super-secret") {
		t.Fatalf("secret key leaked in error response: %s", respStr)
	}
}

func TestCostGateChatCompletionsFreeConfirmedAllowed(t *testing.T) {
	cfg := &config.Config{
		CostGuard: config.CostGuardConfig{
			Enabled: true,
		},
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name: "prov-chat-free",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{
					{APIKey: "sk-free-key"},
				},
				Models: []config.OpenAICompatibilityModel{
					{
						Name:  "free-chat-model",
						Alias: "free-chat-model",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus: config.PriceStatusFreeConfirmed,
							BillingUnit: config.BillingUnitTokens,
						},
					},
				},
			},
		},
	}

	fix := setupGateTestEnv(t, cfg)

	reqBody := `{"model":"free-chat-model","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	fix.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d with body: %s", w.Code, w.Body.String())
	}
	// Upstream must have received exactly 1 call
	if calls := atomic.LoadInt64(fix.upstreamCalls); calls != 1 {
		t.Fatalf("expected 1 upstream call, got %d", calls)
	}
}

func TestCostGateChatStreamingDenied(t *testing.T) {
	cfg := &config.Config{
		CostGuard: config.CostGuardConfig{
			Enabled:            true,
			UnknownPriceAction: config.CostActionDeny,
		},
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name: "prov-chat-stream",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{
					{APIKey: "sk-stream-secret-key"},
				},
				Models: []config.OpenAICompatibilityModel{
					{Name: "unpriced-stream-model", Alias: "unpriced-stream-model"},
				},
			},
		},
	}

	fix := setupGateTestEnv(t, cfg)

	reqBody := `{"model":"unpriced-stream-model","messages":[{"role":"user","content":"stream test"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	fix.router.ServeHTTP(w, req)

	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("expected HTTP 402 for streaming deny, got %d with body: %s", w.Code, w.Body.String())
	}
	if calls := atomic.LoadInt64(fix.upstreamCalls); calls != 0 {
		t.Fatalf("expected 0 upstream calls on streaming deny, got %d", calls)
	}
}

func TestCostGateDeniedDoesNotFallback(t *testing.T) {
	cfg := &config.Config{
		CostGuard: config.CostGuardConfig{
			Enabled:            true,
			UnknownPriceAction: config.CostActionDeny,
		},
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name: "primary-prov",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{
					{APIKey: "key-primary"},
				},
				Models: []config.OpenAICompatibilityModel{
					{Name: "shared-model", Alias: "shared-model"}, // unknown price
				},
			},
			{
				Name: "fallback-prov",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{
					{APIKey: "key-fallback"},
				},
				Models: []config.OpenAICompatibilityModel{
					{Name: "shared-model", Alias: "shared-model"},
				},
			},
		},
	}

	fix := setupGateTestEnv(t, cfg)

	reqBody := `{"model":"shared-model","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	fix.router.ServeHTTP(w, req)

	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("expected HTTP 402, got %d with body: %s", w.Code, w.Body.String())
	}
	// Neither primary nor fallback upstream should have received any calls!
	if calls := atomic.LoadInt64(fix.upstreamCalls); calls != 0 {
		t.Fatalf("expected 0 primary upstream calls, got %d", calls)
	}
	if calls := atomic.LoadInt64(fix.fallbackCalls); calls != 0 {
		t.Fatalf("expected 0 fallback upstream calls, got %d", calls)
	}
}

func TestCostGateLegacyConfigCostGuardDisabledPassesThrough(t *testing.T) {
	cfg := &config.Config{
		CostGuard: config.CostGuardConfig{
			Enabled: false, // explicitly disabled / legacy default
		},
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name: "legacy-prov",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{
					{APIKey: "key-legacy"},
				},
				Models: []config.OpenAICompatibilityModel{
					{Name: "legacy-model", Alias: "legacy-model"}, // unconfigured
				},
			},
		},
	}

	fix := setupGateTestEnv(t, cfg)

	reqBody := `{"model":"legacy-model","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	fix.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 for legacy request, got %d with body: %s", w.Code, w.Body.String())
	}
	if calls := atomic.LoadInt64(fix.upstreamCalls); calls != 1 {
		t.Fatalf("expected 1 upstream call, got %d", calls)
	}
}

func TestCostGateEmbeddingsPaidWithoutTokensDenied(t *testing.T) {
	cfg := &config.Config{
		CostGuard: config.CostGuardConfig{
			Enabled: true,
		},
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name: "embed-prov",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{
					{APIKey: "key-embed"},
				},
				Models: []config.OpenAICompatibilityModel{
					{
						Name:       "embed-paid",
						Alias:      "embed-paid",
						Capability: "embedding",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus:          config.PriceStatusPaidConfirmed,
							BillingUnit:          config.BillingUnitTokens,
							PromptPer1M:          0.20,
							PromptConfigured:     true,
							CompletionPer1M:      0,
							CompletionConfigured: true,
						},
					},
				},
			},
		},
	}

	fix := setupGateTestEnv(t, cfg)

	// String input tokens cannot be reliably known before upstream tokenization => missing_quantity => deny
	reqBody := `{"model":"embed-paid","input":"hello world embedding text"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	fix.router.ServeHTTP(w, req)

	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("expected HTTP 402, got %d with body: %s", w.Code, w.Body.String())
	}
	if calls := atomic.LoadInt64(fix.upstreamCalls); calls != 0 {
		t.Fatalf("expected 0 upstream calls on embeddings deny, got %d", calls)
	}
}

func TestCostGateEmbeddingsFreeConfirmedAllowed(t *testing.T) {
	cfg := &config.Config{
		CostGuard: config.CostGuardConfig{
			Enabled: true,
		},
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name: "embed-prov",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{
					{APIKey: "key-embed"},
				},
				Models: []config.OpenAICompatibilityModel{
					{
						Name:       "embed-free",
						Alias:      "embed-free",
						Capability: "embedding",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus: config.PriceStatusFreeConfirmed,
							BillingUnit: config.BillingUnitTokens,
						},
					},
				},
			},
		},
	}

	fix := setupGateTestEnv(t, cfg)

	reqBody := `{"model":"embed-free","input":"hello world"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	fix.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d with body: %s", w.Code, w.Body.String())
	}
	if calls := atomic.LoadInt64(fix.upstreamCalls); calls != 1 {
		t.Fatalf("expected 1 upstream call, got %d", calls)
	}
}

func TestCostGateRerankUnknownPriceDenied(t *testing.T) {
	cfg := &config.Config{
		CostGuard: config.CostGuardConfig{
			Enabled: true,
		},
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name: "rerank-prov",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{
					{APIKey: "key-rerank"},
				},
				Models: []config.OpenAICompatibilityModel{
					{Name: "rerank-m", Alias: "rerank-m", Capability: "rerank"}, // unknown price
				},
			},
		},
	}

	fix := setupGateTestEnv(t, cfg)

	reqBody := `{"model":"rerank-m","query":"query","documents":["doc1","doc2"]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/rerank", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	fix.router.ServeHTTP(w, req)

	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("expected HTTP 402, got %d with body: %s", w.Code, w.Body.String())
	}
	if calls := atomic.LoadInt64(fix.upstreamCalls); calls != 0 {
		t.Fatalf("expected 0 upstream calls on rerank deny, got %d", calls)
	}
}

func TestCostGateASRMissingSecondsDeniedAndNoAudioSent(t *testing.T) {
	cfg := &config.Config{
		CostGuard: config.CostGuardConfig{
			Enabled: true,
		},
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name: "asr-prov",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{
					{APIKey: "key-asr"},
				},
				Models: []config.OpenAICompatibilityModel{
					{
						Name:       "whisper-paid",
						Alias:      "whisper-paid",
						Capability: "asr",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus:         config.PriceStatusPaidConfirmed,
							BillingUnit:         config.BillingUnitAudioSeconds,
							UnitPrice:           0.006,
							UnitPriceConfigured: true,
						},
					},
				},
			},
		},
	}

	fix := setupGateTestEnv(t, cfg)

	// Create multipart audio upload
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	_ = mw.WriteField("model", "whisper-paid")
	fw, _ := mw.CreateFormFile("file", "test.wav")
	_, _ = fw.Write([]byte("RAW_AUDIO_BYTE_STREAM_THAT_MUST_NOT_BE_SENT"))
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &b)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()

	fix.router.ServeHTTP(w, req)

	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("expected HTTP 402, got %d with body: %s", w.Code, w.Body.String())
	}
	// Upstream must have received 0 requests! Audio bytes were NOT uploaded!
	if calls := atomic.LoadInt64(fix.upstreamCalls); calls != 0 {
		t.Fatalf("expected 0 upstream calls, got %d", calls)
	}
}

func TestCostGateTTSCostExceedsMaxRequestCostDenied(t *testing.T) {
	cfg := &config.Config{
		CostGuard: config.CostGuardConfig{
			Enabled: true,
		},
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name: "tts-prov",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{
					{APIKey: "key-tts"},
				},
				Models: []config.OpenAICompatibilityModel{
					{
						Name:       "tts-voice",
						Alias:      "tts-voice",
						Capability: "tts",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus:         config.PriceStatusPaidConfirmed,
							BillingUnit:         config.BillingUnitCharacters,
							UnitPrice:           0.01, // $0.01 per char
							UnitPriceConfigured: true,
							MaxRequestCost:      0.05, // max $0.05 (5 chars max)
						},
					},
				},
			},
		},
	}

	fix := setupGateTestEnv(t, cfg)

	// 10 chars -> $0.10 > $0.05 limit -> deny!
	reqBody := `{"model":"tts-voice","input":"1234567890"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	fix.router.ServeHTTP(w, req)

	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("expected HTTP 402, got %d with body: %s", w.Code, w.Body.String())
	}
	if calls := atomic.LoadInt64(fix.upstreamCalls); calls != 0 {
		t.Fatalf("expected 0 upstream calls on TTS exceed, got %d", calls)
	}
	if !strings.Contains(w.Body.String(), "max_request_cost_exceeded") {
		t.Fatalf("expected max_request_cost_exceeded in body: %s", w.Body.String())
	}
}

func TestCostGateImageGenerationsDeniedAndAllowed(t *testing.T) {
	cfg := &config.Config{
		CostGuard: config.CostGuardConfig{
			Enabled: true,
		},
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name: "img-prov",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{
					{APIKey: "key-img"},
				},
				Models: []config.OpenAICompatibilityModel{
					{
						Name:       "img-gen",
						Alias:      "img-gen",
						Capability: "image",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus:         config.PriceStatusPaidConfirmed,
							BillingUnit:         config.BillingUnitImages,
							UnitPrice:           0.10, // $0.10 per image
							UnitPriceConfigured: true,
							MaxRequestCost:      0.25, // max $0.25 (up to 2 images)
						},
					},
				},
			},
		},
	}

	fix := setupGateTestEnv(t, cfg)

	// Test A: 3 images -> $0.30 > $0.25 -> deny!
	reqBodyDenied := `{"model":"img-gen","prompt":"a cat","n":3}`
	reqD := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(reqBodyDenied))
	reqD.Header.Set("Content-Type", "application/json")
	wD := httptest.NewRecorder()
	fix.router.ServeHTTP(wD, reqD)

	if wD.Code != http.StatusPaymentRequired {
		t.Fatalf("expected HTTP 402 for 3 images, got %d with body: %s", wD.Code, wD.Body.String())
	}
	if calls := atomic.LoadInt64(fix.upstreamCalls); calls != 0 {
		t.Fatalf("expected 0 upstream calls on image deny, got %d", calls)
	}

	// Test B: 2 images -> $0.20 <= $0.25 -> allow!
	reqBodyAllowed := `{"model":"img-gen","prompt":"a cat","n":2}`
	reqA := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(reqBodyAllowed))
	reqA.Header.Set("Content-Type", "application/json")
	wA := httptest.NewRecorder()
	fix.router.ServeHTTP(wA, reqA)

	if wA.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 for 2 images, got %d with body: %s", wA.Code, wA.Body.String())
	}
	if calls := atomic.LoadInt64(fix.upstreamCalls); calls != 1 {
		t.Fatalf("expected 1 upstream call on image allow, got %d", calls)
	}
}

func TestCostGateVideoSubmitDeniedAndStatusAllowed(t *testing.T) {
	cfg := &config.Config{
		CostGuard: config.CostGuardConfig{
			Enabled: true,
		},
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name: "vid-prov",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{
					{APIKey: "key-vid"},
				},
				Models: []config.OpenAICompatibilityModel{
					{
						Name:       "vid-model",
						Alias:      "vid-model",
						Capability: "video",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus:         config.PriceStatusPaidConfirmed,
							BillingUnit:         config.BillingUnitVideoTasks,
							UnitPrice:           5.00, // $5 per task
							UnitPriceConfigured: true,
							MaxRequestCost:      1.00, // max $1 -> submit will be denied!
						},
					},
				},
			},
		},
	}

	fix := setupGateTestEnv(t, cfg)

	// 1. Submit exceeds budget -> deny!
	reqSubmit := `{"model":"vid-model","prompt":"sunset"}`
	reqS := httptest.NewRequest(http.MethodPost, "/v1/video/submit", strings.NewReader(reqSubmit))
	reqS.Header.Set("Content-Type", "application/json")
	wS := httptest.NewRecorder()
	fix.router.ServeHTTP(wS, reqS)

	if wS.Code != http.StatusPaymentRequired {
		t.Fatalf("expected HTTP 402 for video submit, got %d with body: %s", wS.Code, wS.Body.String())
	}
	if calls := atomic.LoadInt64(fix.upstreamCalls); calls != 0 {
		t.Fatalf("expected 0 upstream calls on video submit deny, got %d", calls)
	}

	// 2. Status query for existing task -> allowed!
	reqStatus := `{"requestId":"task-123","model":"vid-model"}`
	reqQ := httptest.NewRequest(http.MethodPost, "/v1/video/status", strings.NewReader(reqStatus))
	reqQ.Header.Set("Content-Type", "application/json")
	wQ := httptest.NewRecorder()
	fix.router.ServeHTTP(wQ, reqQ)

	if wQ.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 for video status, got %d with body: %s", wQ.Code, wQ.Body.String())
	}
	if calls := atomic.LoadInt64(fix.upstreamCalls); calls != 1 {
		t.Fatalf("expected 1 upstream call for video status query, got %d", calls)
	}
}

func TestCostGateSanitizedErrorResponseFormat(t *testing.T) {
	dec := cost.GateDecision{
		Allowed:     false,
		Decision:    cost.DecisionDeny,
		Code:        cost.CodePricePolicyViolation,
		Reason:      cost.ReasonUnknownPrice,
		Provider:    "safe-provider",
		Model:       "safe-model",
		BillingUnit: "tokens",
		EstimatedCost: cost.CostEstimate{
			AmountKnown: false,
		},
	}

	body, status := cost.BuildGateErrorResponse(dec)
	if status != http.StatusPaymentRequired {
		t.Fatalf("expected 402 status, got %d", status)
	}

	var obj struct {
		Error struct {
			Code        string `json:"code"`
			Message     string `json:"message"`
			Reason      string `json:"reason"`
			Provider    string `json:"provider"`
			Model       string `json:"model"`
			BillingUnit string `json:"billing_unit"`
			AmountKnown bool   `json:"amount_known"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("json parse error: %v", err)
	}
	if obj.Error.Code != cost.CodePricePolicyViolation {
		t.Errorf("expected code %s, got %s", cost.CodePricePolicyViolation, obj.Error.Code)
	}
	if obj.Error.Reason != cost.ReasonUnknownPrice {
		t.Errorf("expected reason %s, got %s", cost.ReasonUnknownPrice, obj.Error.Reason)
	}
	if obj.Error.AmountKnown {
		t.Errorf("expected amount_known to be false")
	}
}
