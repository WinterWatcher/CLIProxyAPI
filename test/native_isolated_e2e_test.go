package test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/openai"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
)

type isolatedUsageCollector struct {
	mu      sync.Mutex
	records []coreusage.Record
}

func (c *isolatedUsageCollector) HandleUsage(ctx context.Context, r coreusage.Record) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r)
}

func (c *isolatedUsageCollector) WaitCount(t *testing.T, count int, timeout time.Duration) []coreusage.Record {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		if len(c.records) >= count {
			out := append([]coreusage.Record(nil), c.records...)
			c.mu.Unlock()
			return out
		}
		c.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t.Fatalf("timed out waiting for %d records (got %d)", count, len(c.records))
	return nil
}

type clientTokenAuth struct {
	token string
}

func (a *clientTokenAuth) Identifier() string { return "test-auth" }
func (a *clientTokenAuth) Authenticate(ctx context.Context, r *http.Request) (*access.Result, *access.AuthError) {
	authHeader := r.Header.Get("Authorization")
	if authHeader != "Bearer "+a.token {
		return nil, access.NewInvalidCredentialError()
	}
	return &access.Result{Principal: a.token, Provider: "config-inline"}, nil
}

func TestIsolatedEndToEndAllSixCapabilities(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const clientSecretKey = "client-isolated-super-secret-key-12345"
	const upstreamApiKey = "upstream-isolated-provider-key-67890"

	var capturedUpstreamHeaders []http.Header
	var upstreamHeadersMu sync.Mutex
	var leakTestActive atomic.Bool

	// 1. Mock upstream server simulating all 6 non-chat endpoints
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHeadersMu.Lock()
		capturedUpstreamHeaders = append(capturedUpstreamHeaders, r.Header.Clone())
		upstreamHeadersMu.Unlock()

		// Verify client secret token is NEVER forwarded
		for hName, hVals := range r.Header {
			for _, v := range hVals {
				if strings.Contains(v, clientSecretKey) {
					t.Errorf("SECURITY FAULT: client secret token leaked in upstream header %s: %s", hName, v)
				}
			}
		}

		// If leak test active, return 500 with sensitive internals
		if leakTestActive.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"internal backend failure connecting to ` + r.Host + ` with key ` + upstreamApiKey + ` at /var/app/internal/code.py via proxy http://10.0.0.1:8888 on provider isolated-upstream-prov"}}`))
			return
		}

		switch r.URL.Path {
		case "/embeddings":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"object":"list","data":[{"embedding":[0.01,0.02,0.03]}],"usage":{"prompt_tokens":12,"total_tokens":12}}`))
		case "/rerank":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"results":[{"index":0,"relevance_score":0.95}],"usage":{"prompt_tokens":8,"total_tokens":8}}`))
		case "/audio/transcriptions":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"text":"hello isolated audio world","duration":4.5}`))
		case "/audio/speech":
			w.Header().Set("Content-Type", "audio/mpeg")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte{0x49, 0x44, 0x33, 0x03, 0x00, 0x00, 0x00}) // Binary ID3 MP3 frame
		case "/images/generations":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"created":1700000000,"data":[{"url":"https://storage.local/img001.png"}]}`))
		case "/video/submit":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"requestId":"iso-video-task-999","status":"submitted"}`))
		case "/video/status":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"requestId":"iso-video-task-999","status":"succeeded","video_url":"https://storage.local/vid001.mp4"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mockUpstream.Close()

	// 2. Isolated configuration without production paths or keys
	cfg := &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name:    "isolated-upstream-prov",
				BaseURL: mockUpstream.URL,
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{
					{
						APIKey:   upstreamApiKey,
						ProxyURL: "http://10.0.0.1:8888",
					},
				},
				Models: []config.OpenAICompatibilityModel{
					{Alias: "iso-embed", Name: "real-embed-v1", Capability: "embedding"},
					{Alias: "iso-rerank", Name: "real-rerank-v1", Capability: "rerank"},
					{Alias: "iso-asr", Name: "real-asr-v1", Capability: "asr"},
					{Alias: "iso-tts", Name: "real-tts-v1", Capability: "tts"},
					{Alias: "iso-img", Name: "real-img-v1", Capability: "image"},
					{Alias: "iso-vid", Name: "real-vid-v1", Capability: "video"},
				},
			},
		},
	}

	authMgr := coreauth.NewManager(nil, nil, nil)
	modelRegistry := registry.GetGlobalRegistry()
	pKey := util.OpenAICompatibleProviderKey("isolated-upstream-prov")
	exec := runtimeexecutor.NewOpenAICompatExecutor(pKey, cfg)
	authMgr.RegisterExecutor(exec)

	const authID = "isolated-auth-0"
	auth := &coreauth.Auth{
		ID:       authID,
		Provider: pKey,
		Attributes: map[string]string{
			"api_key":  upstreamApiKey,
			"base_url": mockUpstream.URL,
		},
	}
	if _, err := authMgr.Register(context.Background(), auth); err != nil {
		t.Fatalf("auth register error: %v", err)
	}

	var regModels []*registry.ModelInfo
	for _, m := range cfg.OpenAICompatibility[0].Models {
		regModels = append(regModels, &registry.ModelInfo{ID: m.Name}, &registry.ModelInfo{ID: m.Alias})
	}
	modelRegistry.RegisterClient(authID, pKey, regModels)
	defer modelRegistry.UnregisterClient(authID)

	apiHandler := &openai.OpenAIAPIHandler{
		BaseAPIHandler: &handlers.BaseAPIHandler{
			AuthManager: authMgr,
			FullConfig:  cfg,
		},
	}

	// 3. Register Usage Collector
	collector := &isolatedUsageCollector{}
	coreusage.DefaultManager().Start(context.Background())
	coreusage.RegisterNamedPlugin("isolated-test-collector", collector)

	// 4. Gin Engine with client authentication
	router := gin.New()
	v1 := router.Group("/v1")
	accMgr := access.NewManager()
	accMgr.SetProviders([]access.Provider{&clientTokenAuth{token: clientSecretKey}})
	v1.Use(func(c *gin.Context) {
		res, err := accMgr.Authenticate(c.Request.Context(), c.Request)
		if err != nil {
			c.AbortWithStatusJSON(err.HTTPStatusCode(), gin.H{"error": err.Message})
			return
		}
		c.Set("userApiKey", res.Principal)
		c.Next()
	})

	v1.POST("/embeddings", apiHandler.Embeddings)
	v1.POST("/rerank", apiHandler.Rerank)
	v1.POST("/audio/transcriptions", apiHandler.AudioTranscriptions)
	v1.POST("/audio/speech", apiHandler.AudioSpeech)
	v1.POST("/images/generations", apiHandler.ImagesGenerations)
	v1.POST("/video/submit", apiHandler.VideoSubmit)
	v1.POST("/video/status", apiHandler.VideoStatus)

	sendReq := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+clientSecretKey)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	// 5. Execute 6 Non-Chat Capability Requests
	// 5.1 Embedding
	respEmbed := sendReq("POST", "/v1/embeddings", `{"model":"iso-embed","input":"test embedding"}`)
	if respEmbed.Code != http.StatusOK {
		t.Fatalf("embedding failed status=%d: %s", respEmbed.Code, respEmbed.Body.String())
	}
	if !gjson.GetBytes(respEmbed.Body.Bytes(), "data.0.embedding").Exists() {
		t.Fatalf("embedding response missing data: %s", respEmbed.Body.String())
	}

	// 5.2 Rerank
	respRerank := sendReq("POST", "/v1/rerank", `{"model":"iso-rerank","query":"search","documents":["doc1"]}`)
	if respRerank.Code != http.StatusOK {
		t.Fatalf("rerank failed status=%d: %s", respRerank.Code, respRerank.Body.String())
	}
	if !gjson.GetBytes(respRerank.Body.Bytes(), "results").Exists() {
		t.Fatalf("rerank response missing results: %s", respRerank.Body.String())
	}

	// 5.3 ASR (Audio Transcriptions)
	respASR := sendReq("POST", "/v1/audio/transcriptions", `{"model":"iso-asr"}`)
	if respASR.Code != http.StatusOK {
		t.Fatalf("ASR failed status=%d: %s", respASR.Code, respASR.Body.String())
	}
	if gjson.GetBytes(respASR.Body.Bytes(), "text").String() != "hello isolated audio world" {
		t.Fatalf("ASR text mismatch: %s", respASR.Body.String())
	}

	// 5.4 TTS (Audio Speech)
	respTTS := sendReq("POST", "/v1/audio/speech", `{"model":"iso-tts","input":"hello TTS"}`)
	if respTTS.Code != http.StatusOK {
		t.Fatalf("TTS failed status=%d: %s", respTTS.Code, respTTS.Body.String())
	}
	if ct := respTTS.Header().Get("Content-Type"); ct != "audio/mpeg" {
		t.Fatalf("TTS content type = %q, want audio/mpeg", ct)
	}
	if respTTS.Body.Len() != 7 || respTTS.Body.Bytes()[0] != 0x49 {
		t.Fatalf("TTS binary corrupted: %v", respTTS.Body.Bytes())
	}

	// 5.5 Image Generations
	respImg := sendReq("POST", "/v1/images/generations", `{"model":"iso-img","prompt":"sunset"}`)
	if respImg.Code != http.StatusOK {
		t.Fatalf("Image failed status=%d: %s", respImg.Code, respImg.Body.String())
	}
	if gjson.GetBytes(respImg.Body.Bytes(), "data.0.url").String() != "https://storage.local/img001.png" {
		t.Fatalf("Image url mismatch: %s", respImg.Body.String())
	}

	// 5.6 Video Submit & Status
	respVidSub := sendReq("POST", "/v1/video/submit", `{"model":"iso-vid","prompt":"city"}`)
	if respVidSub.Code != http.StatusOK {
		t.Fatalf("Video submit failed status=%d: %s", respVidSub.Code, respVidSub.Body.String())
	}
	taskID := gjson.GetBytes(respVidSub.Body.Bytes(), "requestId").String()
	if taskID != "iso-video-task-999" {
		t.Fatalf("Video submit requestId mismatch: %s", respVidSub.Body.String())
	}

	respVidStat := sendReq("POST", "/v1/video/status", `{"requestId":"`+taskID+`"}`)
	if respVidStat.Code != http.StatusOK {
		t.Fatalf("Video status failed status=%d: %s", respVidStat.Code, respVidStat.Body.String())
	}
	if gjson.GetBytes(respVidStat.Body.Bytes(), "status").String() != "succeeded" {
		t.Fatalf("Video status mismatch: %s", respVidStat.Body.String())
	}

	// 6. Verify Upstream Headers: Client Token NEVER sent upstream, Upstream Authorization set correctly
	upstreamHeadersMu.Lock()
	if len(capturedUpstreamHeaders) < 7 {
		t.Fatalf("expected at least 7 upstream requests, got %d", len(capturedUpstreamHeaders))
	}
	for i, h := range capturedUpstreamHeaders {
		authVal := h.Get("Authorization")
		if authVal != "Bearer "+upstreamApiKey {
			t.Errorf("request %d: upstream Authorization = %q, want Bearer %s", i, authVal, upstreamApiKey)
		}
	}
	upstreamHeadersMu.Unlock()

	// 7. Verify Usage Events
	// Wait for records: embed, rerank, asr, tts, image, video submit, video status
	records := collector.WaitCount(t, 6, 2*time.Second)
	capSeen := make(map[string]coreusage.Record)
	for _, rec := range records {
		capSeen[rec.Capability] = rec
	}

	// Embedding usage
	if rec, ok := capSeen["embedding"]; !ok || rec.BillingUnit != "tokens" || rec.Quantity != 12 {
		t.Errorf("embedding usage incorrect: %#v", rec)
	}
	// ASR usage: audio_seconds, NO fabricated tokens
	if rec, ok := capSeen["asr"]; !ok || rec.BillingUnit != "audio_seconds" || rec.Quantity != 4.5 || rec.Detail.TotalTokens != 0 {
		t.Errorf("ASR usage incorrect: %#v", rec)
	}
	// TTS usage: characters
	if rec, ok := capSeen["tts"]; !ok || rec.BillingUnit != "characters" || rec.Quantity != float64(len("hello TTS")) || rec.Detail.TotalTokens != 0 {
		t.Errorf("TTS usage incorrect: %#v", rec)
	}
	// Image usage: images
	if rec, ok := capSeen["image"]; !ok || rec.BillingUnit != "images" || rec.Quantity != 1 || rec.Detail.TotalTokens != 0 {
		t.Errorf("Image usage incorrect: %#v", rec)
	}
	// Video submit usage: video_tasks = 1
	if rec, ok := capSeen["video"]; !ok || rec.BillingUnit != "video_tasks" || rec.Quantity != 1 || rec.Detail.TotalTokens != 0 {
		t.Errorf("Video usage incorrect: %#v", rec)
	}

	// 8. Verify Upstream Error Sanitization & Desensitization
	leakTestActive.Store(true)
	respErr := sendReq("POST", "/v1/images/generations", `{"model":"iso-img","prompt":"error test"}`)
	if respErr.Code != http.StatusInternalServerError && respErr.Code != http.StatusBadGateway {
		t.Fatalf("error test status = %d, want 500/502: %s", respErr.Code, respErr.Body.String())
	}
	errBody := respErr.Body.String()
	leaks := []string{
		mockUpstream.URL,
		upstreamApiKey,
		"/var/app/internal/code.py",
		"isolated-upstream-prov",
		"10.0.0.1:8888",
	}
	for _, leak := range leaks {
		if strings.Contains(errBody, leak) {
			t.Fatalf("SECURITY VIOLATION: upstream error leaked %q: %s", leak, errBody)
		}
	}
}
