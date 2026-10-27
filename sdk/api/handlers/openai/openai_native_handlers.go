package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
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

func (h *OpenAIAPIHandler) NativeOpenAICompat(c *gin.Context, capability string) {
	cfg := h.FullConfig
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
			c.JSON(http.StatusNotFound, gin.H{"error": "model not found or capability mismatch"})
			return
		}
		payload["model"] = upstream
		body, _ = json.Marshal(payload)
		h.executeNative(c, cfg, provider, upstream, capability, body, contentType)
		return
	}
	provider, upstream, ok := nativeProviderModel(cfg, requested, capability)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "model not found or capability mismatch"})
		return
	}
	body, rewriteErr := rewriteMultipartModel(body, contentType, upstream)
	if rewriteErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid multipart body"})
		return
	}
	h.executeNative(c, cfg, provider, upstream, capability, body, contentType)
}

func (h *OpenAIAPIHandler) executeNative(c *gin.Context, cfg *config.Config, provider, model, capability string, body []byte, contentType string) {
	compat := nativeCompat(cfg, provider)
	route := nativeRoute(compat, capability)
	providerKey := util.OpenAICompatibleProviderKey(strings.ToLower(strings.TrimSpace(provider)))
	opts := coreexecutor.Options{SourceFormat: "openai", Headers: c.Request.Header.Clone(), OriginalRequest: body, Metadata: map[string]any{nativeRouteMetadataKey: route, nativeContentTypeMetadataKey: contentType}}
	resp, err := h.AuthManager.Execute(context.WithValue(c.Request.Context(), nativeCapabilityContextKey{}, capability), []string{providerKey}, coreexecutor.Request{Model: model, Payload: body, Format: "openai"}, opts)
	if err != nil {
		if re, ok := err.(nativeRouteError); ok {
			writeNativeResponse(c, re.StatusCode(), re.ResponseHeaders(), re.ResponseBody())
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": "upstream request failed"})
		return
	}
	writeNativeResponse(c, http.StatusOK, resp.Headers, resp.Payload)
}

type nativeCapabilityContextKey struct{}

func (h *OpenAIAPIHandler) Embeddings(c *gin.Context)          { h.NativeOpenAICompat(c, "embedding") }
func (h *OpenAIAPIHandler) Rerank(c *gin.Context)              { h.NativeOpenAICompat(c, "rerank") }
func (h *OpenAIAPIHandler) AudioTranscriptions(c *gin.Context) { h.NativeOpenAICompat(c, "asr") }

func nativeCompat(cfg *config.Config, provider string) *config.OpenAICompatibility {
	for i := range cfg.OpenAICompatibility {
		if strings.EqualFold(cfg.OpenAICompatibility[i].Name, provider) {
			return &cfg.OpenAICompatibility[i]
		}
	}
	return nil
}
func nativeProviderModel(cfg *config.Config, requested, capability string) (string, string, bool) {
	for _, p := range cfg.OpenAICompatibility {
		if p.Disabled {
			continue
		}
		for _, m := range p.Models {
			cap := strings.ToLower(strings.TrimSpace(m.Capability))
			if cap == "" {
				cap = strings.ToLower(strings.TrimSpace(m.Type))
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
	defaults := map[string]string{"embedding": "/embeddings", "rerank": "/rerank", "asr": "/audio/transcriptions"}
	if p != nil && p.Routes != nil {
		if v := strings.TrimSpace(p.Routes[capability]); v != "" {
			return v
		}
		if capability == "asr" {
			if v := strings.TrimSpace(p.Routes["audio/transcriptions"]); v != "" {
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
