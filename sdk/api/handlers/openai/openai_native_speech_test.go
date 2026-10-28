package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

func setupNativeSpeechTestEnv(t *testing.T, upstreamHandler http.HandlerFunc) (*gin.Engine, *usageRecordCollector, *httptest.Server) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	upstreamServer := httptest.NewServer(upstreamHandler)
	t.Cleanup(upstreamServer.Close)

	cfg := &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name:    "test-tts-provider",
				BaseURL: upstreamServer.URL,
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{
					{
						APIKey:   "upstream-tts-key-xyz",
						ProxyURL: "http://10.0.0.1:8888",
					},
				},
				Routes: map[string]string{
					"tts": "/custom/audio/speech",
				},
				Models: []config.OpenAICompatibilityModel{
					{
						Alias:      "tts-custom-alias",
						Name:       "real-tts-custom",
						Capability: "tts",
					},
					{
						Alias:      "tts-default-alias",
						Name:       "real-tts-default",
						Capability: "speech",
					},
					{
						Alias:      "embed-m",
						Name:       "real-embed",
						Capability: "embedding",
					},
				},
			},
			{
				Name:    "test-default-tts-provider",
				BaseURL: upstreamServer.URL,
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{
					{APIKey: "upstream-default-tts-key"},
				},
				Models: []config.OpenAICompatibilityModel{
					{
						Alias:      "tts-default-route",
						Name:       "real-tts-default-route",
						Capability: "tts",
					},
					{
						Alias:      "vid-default-route",
						Name:       "real-vid-default",
						Capability: "video",
					},
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
				regModels = append(regModels, &registry.ModelInfo{ID: m.Name})
				if m.Alias != "" {
					regModels = append(regModels, &registry.ModelInfo{ID: m.Alias})
				}
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
	coreusage.RegisterNamedPlugin("test-speech-collector-"+t.Name(), collector)

	router := gin.New()
	v1 := router.Group("/v1")
	v1.POST("/audio/speech", apiHandler.AudioSpeech)
	v1.POST("/video/submit", apiHandler.VideoSubmit)
	v1.POST("/video/status", apiHandler.VideoStatus)

	return router, collector, upstreamServer
}

// 1. 正常 audio/speech 转发与自定义路由
func TestNativeSpeech_ForwardingAndCustomRoute(t *testing.T) {
	var called atomic.Bool
	router, collector, _ := setupNativeSpeechTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		if r.URL.Path != "/custom/audio/speech" {
			t.Errorf("upstream path = %q, want /custom/audio/speech", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer upstream-tts-key-xyz" {
			t.Errorf("upstream auth = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		w.WriteHeader(http.StatusOK)
		// Binary audio bytes
		_, _ = w.Write([]byte{0xFF, 0xFB, 0x90, 0x64, 0x00, 0x00})
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(`{"model":"tts-custom-alias","input":"Hello world","voice":"alloy"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if !called.Load() {
		t.Fatal("upstream was not called")
	}
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body.String())
	}
	if resp.Header().Get("Content-Type") != "audio/mpeg" {
		t.Errorf("Content-Type = %q, want audio/mpeg", resp.Header().Get("Content-Type"))
	}
	if resp.Body.Len() != 6 || resp.Body.Bytes()[0] != 0xFF {
		t.Fatalf("binary audio bytes corrupted: %v", resp.Body.Bytes())
	}

	rec := collector.WaitLast(t, time.Second)
	if rec.Capability != "tts" {
		t.Fatalf("capability = %q, want tts", rec.Capability)
	}
	if rec.BillingUnit != "characters" {
		t.Fatalf("billing_unit = %q, want characters", rec.BillingUnit)
	}
	if rec.Quantity != float64(len("Hello world")) {
		t.Fatalf("quantity = %v, want %d", rec.Quantity, len("Hello world"))
	}
	if rec.Detail.TotalTokens != 0 {
		t.Fatalf("tokens must not be fabricated for TTS: %d", rec.Detail.TotalTokens)
	}
}

// 2. 默认 audio/speech 路由 (/audio/speech)
func TestNativeSpeech_DefaultRoute(t *testing.T) {
	var called atomic.Bool
	router, _, _ := setupNativeSpeechTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		if r.URL.Path != "/audio/speech" {
			t.Errorf("upstream path = %q, want /audio/speech", r.URL.Path)
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("RIFF1234WAVEfmt "))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(`{"model":"tts-default-route","input":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if !called.Load() {
		t.Fatal("upstream was not called")
	}
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body.String())
	}
	if resp.Header().Get("Content-Type") != "audio/wav" {
		t.Errorf("Content-Type = %q, want audio/wav", resp.Header().Get("Content-Type"))
	}
}

// 3. audio/speech 能力不匹配
func TestNativeSpeech_CapabilityMismatch(t *testing.T) {
	var called atomic.Bool
	router, _, _ := setupNativeSpeechTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(`{"model":"embed-m","input":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if called.Load() {
		t.Fatal("upstream should not be called for capability mismatch")
	}
	if resp.Code != http.StatusBadRequest && resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 400 or 404: %s", resp.Code, resp.Body.String())
	}
}

// 4. audio/speech 未知模型
func TestNativeSpeech_UnknownModel(t *testing.T) {
	var called atomic.Bool
	router, _, _ := setupNativeSpeechTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(`{"model":"unknown-tts-model","input":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if called.Load() {
		t.Fatal("upstream should not be called for unknown model")
	}
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", resp.Code, resp.Body.String())
	}
}

// 5. audio/speech 上游错误脱敏
func TestNativeSpeech_ErrorDesensitized(t *testing.T) {
	router, _, upstreamServer := setupNativeSpeechTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"tts failed on ` + r.Host + ` with key upstream-tts-key-xyz at /app/tts/engine.py on test-tts-provider via proxy http://10.0.0.1:8888"}}`))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(`{"model":"tts-custom-alias","input":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	body := resp.Body.String()
	forbidden := []string{
		upstreamServer.URL,
		"upstream-tts-key-xyz",
		"/app/tts/engine.py",
		"test-tts-provider",
		"10.0.0.1:8888",
	}
	for _, f := range forbidden {
		if strings.Contains(body, f) {
			t.Fatalf("leak detected in tts error response: contains %q: %s", f, body)
		}
	}
}

// 6. 视频 submit 与 status 不重复计 video_tasks
func TestNativeVideo_StatusDoesNotDuplicateVideoTaskBilling(t *testing.T) {
	var subCalled, statCalled atomic.Bool
	router, collector, _ := setupNativeSpeechTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/video/submit" {
			subCalled.Store(true)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"requestId":"task-billing-001"}`))
			return
		}
		if r.URL.Path == "/video/status" {
			statCalled.Store(true)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"requestId":"task-billing-001","status":"processing"}`))
			return
		}
	})

	// 1. Submit
	subReq := httptest.NewRequest(http.MethodPost, "/v1/video/submit", strings.NewReader(`{"model":"vid-default-route","prompt":"waves"}`))
	subReq.Header.Set("Content-Type", "application/json")
	subResp := httptest.NewRecorder()
	router.ServeHTTP(subResp, subReq)

	if subResp.Code != http.StatusOK {
		t.Fatalf("submit status = %d: %s", subResp.Code, subResp.Body.String())
	}
	subRecord := collector.WaitLast(t, time.Second)
	if subRecord.BillingUnit != "video_tasks" || subRecord.Quantity != 1 {
		t.Fatalf("submit must bill 1 video_task: got unit=%q qty=%v", subRecord.BillingUnit, subRecord.Quantity)
	}

	// 2. Query status multiple times
	for i := 0; i < 3; i++ {
		statReq := httptest.NewRequest(http.MethodPost, "/v1/video/status", strings.NewReader(`{"requestId":"task-billing-001"}`))
		statReq.Header.Set("Content-Type", "application/json")
		statResp := httptest.NewRecorder()
		router.ServeHTTP(statResp, statReq)
		if statResp.Code != http.StatusOK {
			t.Fatalf("status %d error = %d: %s", i, statResp.Code, statResp.Body.String())
		}
		// Inspect all records in collector: total billed video_tasks must remain exactly 1!
		collector.mu.Lock()
		var totalBilledTasks float64
		for _, rec := range collector.records {
			if rec.BillingUnit == "video_tasks" {
				totalBilledTasks += rec.Quantity
			}
		}
		collector.mu.Unlock()
		if totalBilledTasks != 1 {
			t.Fatalf("after %d status checks, total billed video tasks = %v, want 1 (status must NOT duplicate video_tasks)", i+1, totalBilledTasks)
		}
	}
}
