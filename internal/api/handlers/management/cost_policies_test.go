package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cost"
)

func setupTestManagementHandler(t *testing.T) (*Handler, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.yaml")

	policyStore, err := cost.NewPersistentPolicyStore(dir)
	if err != nil {
		t.Fatalf("failed to init policy store: %v", err)
	}

	h := &Handler{
		cfg:            &config.Config{},
		configFilePath: configFile,
		failedAttempts: make(map[string]*attemptInfo),
		policyStore:    policyStore,
	}

	r := gin.New()
	mgmt := r.Group("/v0/management")
	{
		mgmt.GET("/cost-policies", h.GetCostPolicies)
		mgmt.GET("/cost-policies/:provider/:model", h.GetCostPolicyByModel)
		mgmt.POST("/cost-policies/dry-run", h.PostCostPoliciesDryRun)
		mgmt.POST("/cost-policies/activate", h.PostCostPoliciesActivate)
		mgmt.POST("/cost-policies/rollback", h.PostCostPoliciesRollback)
		mgmt.POST("/cost-policies/revoke", h.PostCostPoliciesRevoke)
		mgmt.GET("/cost-policies/audit", h.GetCostPoliciesAudit)
	}

	return h, r
}

func sampleAPIPolicy(prov, model string, price float64) *cost.PolicySnapshot {
	p := &cost.PolicySnapshot{
		SchemaVersion:            "cost-policy/v1",
		PolicyVersion:            "cpamp-" + prov + "-" + model + "-r1",
		Provider:                 prov,
		PublicModel:              model,
		UpstreamModelHash:        "hash-123",
		PriceStatus:              "paid_confirmed",
		BillingUnit:              "tokens",
		PromptPer1M:              price,
		CompletionPer1M:          price * 2,
		CachePer1M:               price * 0.5,
		UnitPrice:                0.0,
		MaxRequestCostUSD:        1.0,
		ProviderDailyBudgetUSD:   100.0,
		ProviderMonthlyBudgetUSD: 2000.0,
		GlobalDailyBudgetUSD:     500.0,
		GlobalMonthlyBudgetUSD:   10000.0,
		EffectiveAtMS:            time.Now().UnixMilli(),
		Source:                   "cpamp-approved",
	}
	p.ContentHash = p.ComputeContentHash()
	return p
}

func TestManagementAPI_CostPoliciesFullFlow(t *testing.T) {
	_, r := setupTestManagementHandler(t)

	snap := sampleAPIPolicy("openai", "gpt-4o", 2.5)
	body, _ := json.Marshal(snap)

	// 1. Dry run
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/management/cost-policies/dry-run", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("dry run failed: code=%d body=%s", w.Code, w.Body.String())
	}
	var dryRunResp struct {
		Allowed bool             `json:"allowed"`
		Report  cost.DryRunReport `json:"report"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &dryRunResp)
	if !dryRunResp.Allowed {
		t.Fatalf("expected dry run allowed for approved policy, got: %+v", dryRunResp)
	}

	// 2. Activate
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v0/management/cost-policies/activate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("activate failed: code=%d body=%s", w.Code, w.Body.String())
	}

	// 3. List
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v0/management/cost-policies", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("list failed: code=%d body=%s", w.Code, w.Body.String())
	}
	var listResp struct {
		Policies []*cost.PolicySnapshot `json:"policies"`
		Count    int                    `json:"count"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &listResp)
	if listResp.Count != 1 || len(listResp.Policies) != 1 {
		t.Fatalf("expected 1 active policy, got %d", listResp.Count)
	}

	// 4. Get by Model
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v0/management/cost-policies/openai/gpt-4o", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("get by model failed: code=%d body=%s", w.Code, w.Body.String())
	}

	// 5. Audit
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v0/management/cost-policies/audit", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("audit failed: code=%d body=%s", w.Code, w.Body.String())
	}
	var auditResp struct {
		Events []cost.PolicyAuditEvent `json:"events"`
		Count  int                     `json:"count"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &auditResp)
	if auditResp.Count == 0 {
		t.Fatal("expected audit events to be recorded")
	}

	// 6. Rollback
	rollbackBody := `{"provider":"openai","model":"gpt-4o","reason":"test rollback"}`
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v0/management/cost-policies/rollback", bytes.NewBufferString(rollbackBody))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	// Will fail because there is no previous version yet
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for rollback without previous version, got %d", w.Code)
	}

	// 7. Revoke
	revokeBody := `{"provider":"openai","model":"gpt-4o","version":"cpamp-openai-gpt-4o-r1","reason":"test revoke"}`
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v0/management/cost-policies/revoke", bytes.NewBufferString(revokeBody))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("revoke failed: code=%d body=%s", w.Code, w.Body.String())
	}
}
