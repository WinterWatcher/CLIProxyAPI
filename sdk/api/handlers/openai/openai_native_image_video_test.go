package openai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

type mockAccessProvider struct {
	expectedKey string
}

func (p *mockAccessProvider) Identifier() string {
	return "mock"
}

func (p *mockAccessProvider) Authenticate(ctx context.Context, r *http.Request) (*access.Result, *access.AuthError) {
	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") {
		return nil, access.NewNoCredentialsError()
	}
	token := strings.TrimPrefix(authHeader, "Bearer ")
	if token != p.expectedKey {
		return nil, access.NewInvalidCredentialError()
	}
	return &access.Result{Principal: token, Provider: "config-inline"}, nil
}

func setupNativeImageVideoTestEnv(t *testing.T, upstreamHandler http.HandlerFunc, enableClientAuth bool) (*gin.Engine, *OpenAIAPIHandler, *httptest.Server) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	upstreamServer := httptest.NewServer(upstreamHandler)
	t.Cleanup(upstreamServer.Close)

	cfg := &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name:    "test-custom-provider",
				BaseURL: upstreamServer.URL,
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{
					{
						APIKey:   "upstream-secret-key-xyz",
						ProxyURL: "http://10.0.0.1:8888",
					},
				},
				Routes: map[string]string{
					"image":        "/custom/image/generations",
					"video_submit": "/custom/video/submit",
					"video_status": "/custom/video/status",
				},
				Models: []config.OpenAICompatibilityModel{
					{
						Alias:      "img-alias",
						Name:       "real-img-model",
						Capability: "image",
					},
					{
						Alias:      "vid-alias",
						Name:       "real-vid-model",
						Capability: "video",
					},
					{
						Alias:      "embed-model",
						Name:       "real-embed-model",
						Capability: "embedding",
					},
				},
			},
			{
				Name:    "test-default-provider",
				BaseURL: upstreamServer.URL,
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{
					{
						APIKey: "upstream-default-key-2",
					},
				},
				Models: []config.OpenAICompatibilityModel{
					{
						Alias:      "img-default-route",
						Name:       "real-img-default",
						Capability: "image",
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

	router := gin.New()
	v1 := router.Group("/v1")
	if enableClientAuth {
		accMgr := access.NewManager()
		accMgr.SetProviders([]access.Provider{&mockAccessProvider{expectedKey: "valid-client-token"}})
		v1.Use(func(c *gin.Context) {
			res, err := accMgr.Authenticate(c.Request.Context(), c.Request)
			if err != nil {
				c.AbortWithStatusJSON(err.HTTPStatusCode(), gin.H{"error": err.Message})
				return
			}
			c.Set("userApiKey", res.Principal)
			c.Next()
		})
	}

	v1.POST("/images/generations", apiHandler.ImagesGenerations)
	v1.POST("/video/submit", apiHandler.VideoSubmit)
	v1.POST("/video/status", apiHandler.VideoStatus)

	return router, apiHandler, upstreamServer
}

// 1. 正常图片请求转发
func TestNativeImage_Forwarding(t *testing.T) {
	var called atomic.Bool
	router, _, _ := setupNativeImageVideoTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		if r.URL.Path != "/images/generations" {
			t.Errorf("upstream path = %q, want /images/generations", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer upstream-default-key-2" {
			t.Errorf("upstream auth = %q, want Bearer upstream-default-key-2", r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		if gjson.GetBytes(body, "model").String() != "real-img-default" {
			t.Errorf("upstream model = %q, want real-img-default", gjson.GetBytes(body, "model").String())
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"created":1234,"data":[{"url":"https://example.com/cat.png"}]}`))
	}, false)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"img-default-route","prompt":"cute cat"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if !called.Load() {
		t.Fatal("upstream was not called")
	}
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body.String())
	}
	if gjson.GetBytes(resp.Body.Bytes(), "data.0.url").String() != "https://example.com/cat.png" {
		t.Fatalf("unexpected response: %s", resp.Body.String())
	}
}

// 2. 图片模型alias替换
func TestNativeImage_ModelAliasReplacement(t *testing.T) {
	var called atomic.Bool
	router, _, _ := setupNativeImageVideoTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		if r.URL.Path != "/custom/image/generations" {
			t.Errorf("upstream path = %q, want /custom/image/generations", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if gjson.GetBytes(body, "model").String() != "real-img-model" {
			t.Errorf("upstream model = %q, want real-img-model (alias replaced)", gjson.GetBytes(body, "model").String())
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"created":5678,"data":[{"b64_json":"abcd"}]}`))
	}, false)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"img-alias","prompt":"sunset"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if !called.Load() {
		t.Fatal("upstream was not called")
	}
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body.String())
	}
	if gjson.GetBytes(resp.Body.Bytes(), "data.0.b64_json").String() != "abcd" {
		t.Fatalf("unexpected response: %s", resp.Body.String())
	}
}

// 3. 图片能力不匹配
func TestNativeImage_CapabilityMismatch(t *testing.T) {
	var called atomic.Bool
	router, _, _ := setupNativeImageVideoTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
	}, false)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"embed-model","prompt":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if called.Load() {
		t.Fatal("upstream should not be called for capability mismatch")
	}
	if resp.Code != http.StatusBadRequest && resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 400 or 404: %s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(strings.ToLower(resp.Body.String()), "capability") && !strings.Contains(strings.ToLower(resp.Body.String()), "not support") {
		t.Fatalf("expected capability mismatch error, got: %s", resp.Body.String())
	}
}

// 4. 图片未知模型
func TestNativeImage_UnknownModel(t *testing.T) {
	var called atomic.Bool
	router, _, _ := setupNativeImageVideoTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
	}, false)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"non-existent-image-model","prompt":"test"}`))
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

// 5. 图片未授权
func TestNativeImage_Unauthorized(t *testing.T) {
	var called atomic.Bool
	router, _, _ := setupNativeImageVideoTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
	}, true)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"img-alias","prompt":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	// No Authorization header
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if called.Load() {
		t.Fatal("upstream should not be called when client is unauthorized")
	}
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", resp.Code, resp.Body.String())
	}
}

// 6. 图片上游401
func TestNativeImage_Upstream401(t *testing.T) {
	router, _, _ := setupNativeImageVideoTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream auth failed","type":"invalid_request_error"}}`))
	}, false)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"img-alias","prompt":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", resp.Code, resp.Body.String())
	}
}

// 7. 图片上游429
func TestNativeImage_Upstream429(t *testing.T) {
	router, _, _ := setupNativeImageVideoTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "45")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limit exceeded","type":"rate_limit_error"}}`))
	}, false)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"img-alias","prompt":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", resp.Code, resp.Body.String())
	}
	if resp.Header().Get("Retry-After") != "45" {
		t.Errorf("Retry-After header = %q, want 45", resp.Header().Get("Retry-After"))
	}
}

// 8. 图片上游500
func TestNativeImage_Upstream500(t *testing.T) {
	router, _, _ := setupNativeImageVideoTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"internal error","type":"server_error"}}`))
	}, false)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"img-alias","prompt":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusInternalServerError && resp.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 500 or 502: %s", resp.Code, resp.Body.String())
	}
}

// 9. 图片错误不泄漏内部信息
func TestNativeImage_ErrorDoesNotLeakInternalInfo(t *testing.T) {
	router, _, upstreamServer := setupNativeImageVideoTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		// Leaking internal info
		_, _ = w.Write([]byte(`{"error":{"message":"failed connecting to ` + r.Host + ` with key upstream-secret-key-xyz at /data/app/internal/secrets.yaml on provider test-custom-provider via proxy http://10.0.0.1:8888"}}`))
	}, false)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"img-alias","prompt":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	body := resp.Body.String()
	forbidden := []string{
		upstreamServer.URL,
		"upstream-secret-key-xyz",
		"/data/app/internal/secrets.yaml",
		"test-custom-provider",
		"10.0.0.1:8888",
	}
	for _, f := range forbidden {
		if strings.Contains(body, f) {
			t.Fatalf("leak detected in response body: contains %q: %s", f, body)
		}
	}
}

// 10. 客户端Authorization不转发到上游
func TestNativeImage_ClientAuthNotForwarded(t *testing.T) {
	var capturedAuth atomic.Value
	router, _, _ := setupNativeImageVideoTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		capturedAuth.Store(r.Header.Get("Authorization"))
		for k, v := range r.Header {
			for _, val := range v {
				if strings.Contains(val, "client-secret-token-12345") {
					t.Errorf("client token leaked in header %s: %s", k, val)
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"url":"https://example.com/ok.png"}]}`))
	}, false)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"img-alias","prompt":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer client-secret-token-12345")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body.String())
	}
	upAuth, _ := capturedAuth.Load().(string)
	if strings.Contains(upAuth, "client-secret-token-12345") {
		t.Fatalf("client authorization was forwarded to upstream: %s", upAuth)
	}
	if upAuth != "Bearer upstream-secret-key-xyz" {
		t.Fatalf("upstream authorization = %q, want Bearer upstream-secret-key-xyz", upAuth)
	}
}

// 11. 正常视频submit
func TestNativeVideo_SubmitNormal(t *testing.T) {
	var called atomic.Bool
	router, _, _ := setupNativeImageVideoTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		if r.URL.Path != "/video/submit" {
			t.Errorf("upstream path = %q, want /video/submit", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if gjson.GetBytes(body, "model").String() != "real-vid-default" {
			t.Errorf("upstream model = %q, want real-vid-default", gjson.GetBytes(body, "model").String())
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"requestId":"task-vid-001","status":"submitted"}`))
	}, false)

	req := httptest.NewRequest(http.MethodPost, "/v1/video/submit", strings.NewReader(`{"model":"vid-default-route","prompt":"ocean waves"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if !called.Load() {
		t.Fatal("upstream was not called for video submit")
	}
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body.String())
	}
	reqID := gjson.GetBytes(resp.Body.Bytes(), "requestId").String()
	if reqID != "task-vid-001" {
		t.Fatalf("requestId = %q, want task-vid-001; body: %s", reqID, resp.Body.String())
	}
}

// 12. 正常视频status
func TestNativeVideo_StatusNormal(t *testing.T) {
	// First submit a task
	var submitCalled, statusCalled atomic.Bool
	router, _, _ := setupNativeImageVideoTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/video/submit" {
			submitCalled.Store(true)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"requestId":"task-vid-002","status":"submitted"}`))
			return
		}
		if r.URL.Path == "/video/status" {
			statusCalled.Store(true)
			body, _ := io.ReadAll(r.Body)
			if gjson.GetBytes(body, "requestId").String() != "task-vid-002" {
				t.Errorf("status body requestId = %q, want task-vid-002", gjson.GetBytes(body, "requestId").String())
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"requestId":"task-vid-002","status":"succeeded","video_url":"https://example.com/result.mp4"}`))
			return
		}
		t.Errorf("unexpected path: %s", r.URL.Path)
	}, false)

	// Step 1: Submit
	subReq := httptest.NewRequest(http.MethodPost, "/v1/video/submit", strings.NewReader(`{"model":"vid-default-route","prompt":"ocean waves"}`))
	subReq.Header.Set("Content-Type", "application/json")
	subResp := httptest.NewRecorder()
	router.ServeHTTP(subResp, subReq)

	if subResp.Code != http.StatusOK {
		t.Fatalf("submit status = %d: %s", subResp.Code, subResp.Body.String())
	}
	if !submitCalled.Load() {
		t.Fatal("submit was not called")
	}

	// Step 2: Query status
	statReq := httptest.NewRequest(http.MethodPost, "/v1/video/status", strings.NewReader(`{"requestId":"task-vid-002"}`))
	statReq.Header.Set("Content-Type", "application/json")
	statResp := httptest.NewRecorder()
	router.ServeHTTP(statResp, statReq)

	if !statusCalled.Load() {
		t.Fatal("status was not called")
	}
	if statResp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", statResp.Code, statResp.Body.String())
	}
	if gjson.GetBytes(statResp.Body.Bytes(), "status").String() != "succeeded" {
		t.Fatalf("status body: %s", statResp.Body.String())
	}
	if gjson.GetBytes(statResp.Body.Bytes(), "video_url").String() != "https://example.com/result.mp4" {
		t.Fatalf("video_url body: %s", statResp.Body.String())
	}
}

// 13. 视频requestId缺失
func TestNativeVideo_MissingRequestId(t *testing.T) {
	var called atomic.Bool
	router, _, _ := setupNativeImageVideoTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
	}, false)

	req := httptest.NewRequest(http.MethodPost, "/v1/video/status", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if called.Load() {
		t.Fatal("upstream should not be called when requestId is missing")
	}
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.Code, resp.Body.String())
	}
}

// 14. 视频任务不存在
func TestNativeVideo_TaskNotFound(t *testing.T) {
	var called atomic.Bool
	router, _, _ := setupNativeImageVideoTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
	}, false)

	req := httptest.NewRequest(http.MethodPost, "/v1/video/status", strings.NewReader(`{"requestId":"non-existent-task-id-123"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if called.Load() {
		t.Fatal("upstream should not be called when video task does not exist locally")
	}
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", resp.Code, resp.Body.String())
	}
}

// 15. 视频模型alias
func TestNativeVideo_ModelAlias(t *testing.T) {
	var called atomic.Bool
	router, _, _ := setupNativeImageVideoTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		if r.URL.Path != "/custom/video/submit" {
			t.Errorf("upstream path = %q, want /custom/video/submit", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if gjson.GetBytes(body, "model").String() != "real-vid-model" {
			t.Errorf("upstream model = %q, want real-vid-model", gjson.GetBytes(body, "model").String())
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"requestId":"task-alias-001"}`))
	}, false)

	req := httptest.NewRequest(http.MethodPost, "/v1/video/submit", strings.NewReader(`{"model":"vid-alias","prompt":"city timelapse"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if !called.Load() {
		t.Fatal("upstream was not called")
	}
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.Code, resp.Body.String())
	}
	if gjson.GetBytes(resp.Body.Bytes(), "requestId").String() != "task-alias-001" {
		t.Fatalf("requestId = %q, want task-alias-001", gjson.GetBytes(resp.Body.Bytes(), "requestId").String())
	}
}

// 16. 视频能力不匹配
func TestNativeVideo_CapabilityMismatch(t *testing.T) {
	var called atomic.Bool
	router, _, _ := setupNativeImageVideoTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
	}, false)

	req := httptest.NewRequest(http.MethodPost, "/v1/video/submit", strings.NewReader(`{"model":"embed-model","prompt":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if called.Load() {
		t.Fatal("upstream should not be called for video capability mismatch")
	}
	if resp.Code != http.StatusBadRequest && resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 400 or 404: %s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(strings.ToLower(resp.Body.String()), "capability") && !strings.Contains(strings.ToLower(resp.Body.String()), "not support") {
		t.Fatalf("expected capability mismatch error, got: %s", resp.Body.String())
	}
}

// 17. 视频上游错误脱敏
func TestNativeVideo_ErrorDesensitized(t *testing.T) {
	router, _, upstreamServer := setupNativeImageVideoTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"crash in ` + r.Host + ` with key upstream-secret-key-xyz at /app/video/handler.py on test-custom-provider via proxy http://10.0.0.1:8888"}}`))
	}, false)

	req := httptest.NewRequest(http.MethodPost, "/v1/video/submit", strings.NewReader(`{"model":"vid-alias","prompt":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	body := resp.Body.String()
	forbidden := []string{
		upstreamServer.URL,
		"upstream-secret-key-xyz",
		"/app/video/handler.py",
		"test-custom-provider",
		"10.0.0.1:8888",
	}
	for _, f := range forbidden {
		if strings.Contains(body, f) {
			t.Fatalf("leak detected in video error response: contains %q: %s", f, body)
		}
	}
}
