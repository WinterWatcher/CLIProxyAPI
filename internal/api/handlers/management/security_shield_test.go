package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestSecurityShieldHandlers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	h := &Handler{}

	router.GET("/v0/management/security-shield/status", h.GetSecurityShieldStatus)
	router.GET("/v0/management/security-shield/rules", h.GetSecurityShieldRules)
	router.POST("/v0/management/security-shield/rules", h.PostSecurityShieldRules)

	t.Run("GetStatus returns valid telemetry", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v0/management/security-shield/status", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d", w.Code)
		}

		var resp SecurityShieldStatusResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		if resp.WafRiskLevel == "" {
			t.Errorf("expected non-empty WafRiskLevel")
		}
	})

	t.Run("GetRules returns rules or not found gracefully", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v0/management/security-shield/rules", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK && w.Code != http.StatusNotFound {
			t.Fatalf("unexpected code: %d", w.Code)
		}
	})

	t.Run("PostRules validates JSON body", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v0/management/security-shield/rules", strings.NewReader("invalid-json"))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for bad json, got %d", w.Code)
		}
	})
}
