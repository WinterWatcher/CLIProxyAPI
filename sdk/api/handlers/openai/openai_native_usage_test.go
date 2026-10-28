package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

type usageRecordCollector struct {
	mu      sync.Mutex
	records []coreusage.Record
}

func (c *usageRecordCollector) HandleUsage(ctx context.Context, r coreusage.Record) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r)
}

func (c *usageRecordCollector) WaitLast(t *testing.T, timeout time.Duration) coreusage.Record {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		if len(c.records) > 0 {
			rec := c.records[len(c.records)-1]
			c.mu.Unlock()
			return rec
		}
		c.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for usage record")
	return coreusage.Record{}
}

func setupUsageTestEnv(t *testing.T, upstreamHandler http.HandlerFunc) (*gin.Engine, *usageRecordCollector) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	upstreamServer := httptest.NewServer(upstreamHandler)
	t.Cleanup(upstreamServer.Close)

	cfg := &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name:    "test-usage-provider",
				BaseURL: upstreamServer.URL,
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{
					{APIKey: "upstream-key"},
				},
				Models: []config.OpenAICompatibilityModel{
					{Alias: "embed-m", Name: "real-embed", Capability: "embedding"},
					{Alias: "rerank-m", Name: "real-rerank", Capability: "rerank"},
					{Alias: "asr-m", Name: "real-asr", Capability: "asr"},
					{Alias: "img-m", Name: "real-img", Capability: "image"},
					{Alias: "vid-m", Name: "real-vid", Capability: "video"},
				},
			},
		},
	}

	authMgr := coreauth.NewManager(nil, nil, nil)
	modelRegistry := registry.GetGlobalRegistry()
	for _, p := range cfg.OpenAICompatibility {
		pKey := util.OpenAICompatibleProviderKey(p.Name)
		exec := runtimeexecutor.NewOpenAICompatExecutor(pKey, cfg)
		authMgr.RegisterExecutor(exec)
		for idx, keyEntry := range p.APIKeyEntries {
			authID := p.Name + "-auth-" + string(rune('0'+idx))
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

	apiHandler := &OpenAIAPIHandler{
		BaseAPIHandler: &handlers.BaseAPIHandler{
			AuthManager: authMgr,
			FullConfig:  cfg,
		},
	}

	collector := &usageRecordCollector{}
	coreusage.DefaultManager().Start(context.Background())
	coreusage.RegisterNamedPlugin("test-collector-"+t.Name(), collector)

	router := gin.New()
	v1 := router.Group("/v1")
	v1.POST("/embeddings", apiHandler.Embeddings)
	v1.POST("/rerank", apiHandler.Rerank)
	v1.POST("/audio/transcriptions", apiHandler.AudioTranscriptions)
	v1.POST("/images/generations", apiHandler.ImagesGenerations)
	v1.POST("/video/submit", apiHandler.VideoSubmit)

	return router, collector
}

func TestUsageEvent_Embedding(t *testing.T) {
	router, collector := setupUsageTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[{"embedding":[0.1,0.2]}],"usage":{"prompt_tokens":42,"total_tokens":42}}`))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"model":"embed-m","input":"hello world"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.Code, resp.Body.String())
	}

	r := collector.WaitLast(t, time.Second)
	if r.Capability != "embedding" {
		t.Fatalf("capability = %q, want embedding", r.Capability)
	}
	if r.BillingUnit != "tokens" {
		t.Fatalf("billing_unit = %q, want tokens", r.BillingUnit)
	}
	if r.Quantity != 42 || r.InputQuantity != 42 {
		t.Fatalf("quantity = %v, inQty = %v, want 42", r.Quantity, r.InputQuantity)
	}
}

func TestUsageEvent_ASR_NoFabricatedTokens(t *testing.T) {
	router, collector := setupUsageTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"text":"hello transcription","duration":15.5}`))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", strings.NewReader(`{"model":"asr-m"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.Code, resp.Body.String())
	}

	r := collector.WaitLast(t, time.Second)
	if r.Capability != "asr" {
		t.Fatalf("capability = %q, want asr", r.Capability)
	}
	if r.BillingUnit != "audio_seconds" {
		t.Fatalf("billing_unit = %q, want audio_seconds", r.BillingUnit)
	}
	if r.Quantity != 15.5 || r.InputQuantity != 15.5 {
		t.Fatalf("quantity = %v, inQty = %v, want 15.5", r.Quantity, r.InputQuantity)
	}
	if r.Detail.TotalTokens != 0 || r.Detail.InputTokens != 0 {
		t.Fatalf("tokens must not be fabricated for ASR: got total=%d in=%d", r.Detail.TotalTokens, r.Detail.InputTokens)
	}
}

func TestUsageEvent_Image(t *testing.T) {
	router, collector := setupUsageTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"created":123,"data":[{"url":"https://a.png"},{"url":"https://b.png"}]}`))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"img-m","prompt":"drawing","n":2}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.Code, resp.Body.String())
	}

	r := collector.WaitLast(t, time.Second)
	if r.Capability != "image" {
		t.Fatalf("capability = %q, want image", r.Capability)
	}
	if r.BillingUnit != "images" {
		t.Fatalf("billing_unit = %q, want images", r.BillingUnit)
	}
	if r.Quantity != 2 || r.OutputQuantity != 2 {
		t.Fatalf("quantity = %v, outQty = %v, want 2", r.Quantity, r.OutputQuantity)
	}
	if r.Detail.TotalTokens != 0 {
		t.Fatalf("tokens must not be fabricated for image: got %d", r.Detail.TotalTokens)
	}
}

func TestUsageEvent_Video(t *testing.T) {
	router, collector := setupUsageTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"requestId":"v-123","status":"submitted"}`))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/video/submit", strings.NewReader(`{"model":"vid-m","prompt":"ocean waves"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.Code, resp.Body.String())
	}

	r := collector.WaitLast(t, time.Second)
	if r.Capability != "video" {
		t.Fatalf("capability = %q, want video", r.Capability)
	}
	if r.BillingUnit != "video_tasks" {
		t.Fatalf("billing_unit = %q, want video_tasks", r.BillingUnit)
	}
	if r.Quantity != 1 || r.OutputQuantity != 1 {
		t.Fatalf("quantity = %v, outQty = %v, want 1", r.Quantity, r.OutputQuantity)
	}
	if r.Detail.TotalTokens != 0 {
		t.Fatalf("tokens must not be fabricated for video: got %d", r.Detail.TotalTokens)
	}
	// Verify no secrets in record
	if strings.Contains(r.APIKey, "upstream-key") {
		t.Fatalf("upstream key leaked into record: %s", r.APIKey)
	}
}
