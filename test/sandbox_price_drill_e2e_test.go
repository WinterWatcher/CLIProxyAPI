package test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cost"
)

// MockSupplierSandbox records all received requests without leaking credentials.
type MockSupplierSandbox struct {
	server           *httptest.Server
	mu               sync.Mutex
	requestCount     int32
	receivedModels   []string
	receivedBodyHash []string
	hasAudioData     bool
	hasImageData     bool
	hasVideoData     bool
	responseMode     string // "200", "429", "500", "overrun", "slow", "abort"
}

func newMockSupplierSandbox() *MockSupplierSandbox {
	s := &MockSupplierSandbox{
		receivedModels:   make([]string, 0),
		receivedBodyHash: make([]string, 0),
		responseMode:     "200",
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&s.requestCount, 1)

		bodyBytes, _ := io.ReadAll(r.Body)
		h := sha256.Sum256(bodyBytes)
		hashStr := hex.EncodeToString(h[:])

		s.mu.Lock()
		s.receivedBodyHash = append(s.receivedBodyHash, hashStr)

		// Model detection
		var reqPayload map[string]any
		_ = json.Unmarshal(bodyBytes, &reqPayload)
		if m, ok := reqPayload["model"].(string); ok {
			s.receivedModels = append(s.receivedModels, m)
		}

		// Modality payload checking
		ct := r.Header.Get("Content-Type")
		if strings.Contains(ct, "audio") || strings.Contains(string(bodyBytes), "duration") {
			s.hasAudioData = true
		}
		if strings.Contains(ct, "image") || strings.Contains(string(bodyBytes), "image") {
			s.hasImageData = true
		}
		if strings.Contains(string(bodyBytes), "video") {
			s.hasVideoData = true
		}
		mode := s.responseMode
		s.mu.Unlock()

		switch mode {
		case "429":
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limit exceeded","type":"requests"}}`))
		case "500":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"internal error"}}`))
		case "overrun":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1000,"completion_tokens":4000,"total_tokens":5000}}`))
		case "slow":
			time.Sleep(100 * time.Millisecond)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"slow response"}}]}`))
		case "abort":
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, _ := hj.Hijack()
				_ = conn.Close()
				return
			}
			w.WriteHeader(http.StatusBadGateway)
		default: // "200"
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hello from sandbox"}}],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}}`))
		}
	})

	s.server = httptest.NewServer(handler)
	return s
}

func (s *MockSupplierSandbox) Close() {
	if s.server != nil {
		s.server.Close()
	}
}

func (s *MockSupplierSandbox) SetResponseMode(mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.responseMode = mode
}

func (s *MockSupplierSandbox) GetRequestCount() int32 {
	return atomic.LoadInt32(&s.requestCount)
}

// ------------------- SCENARIO TESTS -------------------

// Scenario 1: Free normal call
func TestE2E_Scenario1_FreeConfirmedNormalCall(t *testing.T) {
	sandbox := newMockSupplierSandbox()
	defer sandbox.Close()

	dir := t.TempDir()
	policyStore, err := cost.NewPersistentPolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	budgetStore, err := cost.NewPersistentBudgetStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	// 1. CPAMP approves free_confirmed -> CLIProxy activates policy
	freePolicy := &cost.PolicySnapshot{
		SchemaVersion:     "cost-policy/v1",
		PolicyVersion:     "cpamp-openai-free-model-r1",
		Provider:          "openai",
		PublicModel:       "free-model",
		UpstreamModelHash: "hash-upstream-free",
		PriceStatus:       "free_confirmed",
		BillingUnit:       "tokens",
		PromptPer1M:       0,
		CompletionPer1M:   0,
		EffectiveAtMS:     time.Now().UnixMilli(),
		Source:            "cpamp-approved",
	}
	freePolicy.ContentHash = freePolicy.ComputeContentHash()

	dryRun, err := policyStore.DryRun(freePolicy)
	if err != nil || !dryRun.Allowed {
		t.Fatalf("dry run failed for free policy: %+v, err: %v", dryRun, err)
	}
	_, err = policyStore.Activate(freePolicy, false)
	if err != nil {
		t.Fatalf("activate free policy failed: %v", err)
	}

	// 2. Gate evaluation
	cfg := &config.Config{
		CostGuard: config.CostGuardConfig{
			Enabled: true,
		},
	}
	gateReq := cost.GateRequest{
		Config:      cfg,
		Provider:    "openai",
		Model:       "free-model",
		Measurement: cost.FromTokens(100, 50, 0, 0, 0),
		RequestID:   "req-free-1",
		PolicyStore: policyStore,
		BudgetStore: budgetStore,
	}
	dec := cost.EvaluatePreflight(gateReq)
	if !dec.Allowed {
		t.Fatalf("expected free call to be allowed, got: %+v", dec)
	}
	if dec.EstimatedCost.AmountNanos != 0 {
		t.Fatalf("expected cost to be explicitly 0, got %d", dec.EstimatedCost.AmountNanos)
	}
	if dec.PolicyVersion != freePolicy.PolicyVersion {
		t.Fatalf("expected policy version to be recorded, got %s", dec.PolicyVersion)
	}
}

// Scenario 2: Free to paid blocked
func TestE2E_Scenario2_FreeToPaidCandidateBlocked(t *testing.T) {
	sandbox := newMockSupplierSandbox()
	defer sandbox.Close()

	dir := t.TempDir()
	policyStore, _ := cost.NewPersistentPolicyStore(dir)

	// Active is free
	freePolicy := &cost.PolicySnapshot{
		SchemaVersion:     "cost-policy/v1",
		PolicyVersion:     "cpamp-openai-free-model-r1",
		Provider:          "openai",
		PublicModel:       "free-model",
		PriceStatus:       "free_confirmed",
		BillingUnit:       "tokens",
		EffectiveAtMS:     time.Now().UnixMilli(),
		Source:            "cpamp-approved",
	}
	freePolicy.ContentHash = freePolicy.ComputeContentHash()
	_, _ = policyStore.Activate(freePolicy, false)

	// Supplier changed price: candidate is paid
	paidCandidate := &cost.PolicySnapshot{
		SchemaVersion:     "cost-policy/v1",
		PolicyVersion:     "cpamp-openai-free-model-r2",
		Provider:          "openai",
		PublicModel:       "free-model",
		PriceStatus:       "paid_confirmed",
		BillingUnit:       "tokens",
		PromptPer1M:       5.0,
		CompletionPer1M:   15.0,
		EffectiveAtMS:     time.Now().UnixMilli() + 1000,
		Source:            "cpamp-candidate",
	}
	paidCandidate.ContentHash = paidCandidate.ComputeContentHash()

	// Dry run MUST deny free_to_paid
	report, err := policyStore.DryRun(paidCandidate)
	if err != nil {
		t.Fatal(err)
	}
	if report.Allowed || report.Decision != "deny" || report.RiskLevel != "critical" {
		t.Fatalf("expected free to paid candidate to be denied: %+v", report)
	}

	// Active policy must remain the free one
	active, ok := policyStore.GetActivePolicy("openai", "free-model")
	if !ok || active.PriceStatus != "free_confirmed" {
		t.Fatal("active policy should not change")
	}
}

// Scenario 3: Price increase with approval workflow
func TestE2E_Scenario3_PriceIncreaseApprovalWorkflow(t *testing.T) {
	dir := t.TempDir()
	policyStore, _ := cost.NewPersistentPolicyStore(dir)

	// 1. Active paid $1
	v1 := &cost.PolicySnapshot{
		SchemaVersion:     "cost-policy/v1",
		PolicyVersion:     "cpamp-openai-paid-model-r1",
		Provider:          "openai",
		PublicModel:       "paid-model",
		PriceStatus:       "paid_confirmed",
		BillingUnit:       "tokens",
		PromptPer1M:       1.0,
		CompletionPer1M:   2.0,
		EffectiveAtMS:     time.Now().UnixMilli(),
		Source:            "cpamp-approved",
	}
	v1.ContentHash = v1.ComputeContentHash()
	_, _ = policyStore.Activate(v1, true)

	// 2. Candidate paid $2 (price increase)
	v2 := &cost.PolicySnapshot{
		SchemaVersion:     "cost-policy/v1",
		PolicyVersion:     "cpamp-openai-paid-model-r2",
		Provider:          "openai",
		PublicModel:       "paid-model",
		PriceStatus:       "paid_confirmed",
		BillingUnit:       "tokens",
		PromptPer1M:       2.0,
		CompletionPer1M:   4.0,
		EffectiveAtMS:     time.Now().UnixMilli() + 1000,
		Source:            "cpamp-candidate",
	}
	v2.ContentHash = v2.ComputeContentHash()

	report, err := policyStore.DryRun(v2)
	if err != nil {
		t.Fatal(err)
	}
	if report.Decision != "pending" || report.RiskLevel != "high" {
		t.Fatalf("expected high risk pending for price increase: %+v", report)
	}

	// 3. Before approval: active is still v1 ($1)
	curr, _ := policyStore.GetActivePolicy("openai", "paid-model")
	if curr.PromptPer1M != 1.0 {
		t.Fatalf("expected active to remain $1, got %f", curr.PromptPer1M)
	}

	// 4. Admin approves: force=true activates v2
	v2Approved, err := policyStore.Activate(v2, true)
	if err != nil {
		t.Fatalf("activation failed after approval: %v", err)
	}
	if v2Approved.PromptPer1M != 2.0 {
		t.Fatalf("expected active to be updated to $2, got %f", v2Approved.PromptPer1M)
	}
}

// Scenario 4: Fallback to more expensive provider blocked
func TestE2E_Scenario4_FallbackMoreExpensiveBlocked(t *testing.T) {
	sandbox := newMockSupplierSandbox()
	defer sandbox.Close()

	cfg := &config.Config{
		CostGuard: config.CostGuardConfig{
			Enabled: true,
		},
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name: "provider-cheap",
				CostPolicy: &config.ProviderCostPolicy{
					AllowFallback: true,
				},
				Models: []config.OpenAICompatibilityModel{
					{
						Name: "shared-model",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus:          "paid_confirmed",
							BillingUnit:          "tokens",
							PromptPer1M:          1.0,
							CompletionPer1M:      2.0,
							PromptConfigured:     true,
							CompletionConfigured: true,
						},
					},
				},
			},
			{
				Name: "provider-expensive",
				Models: []config.OpenAICompatibilityModel{
					{
						Name: "shared-model",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus:          "paid_confirmed",
							BillingUnit:          "tokens",
							PromptPer1M:          10.0,
							CompletionPer1M:      20.0,
							PromptConfigured:     true,
							CompletionConfigured: true,
						},
					},
				},
			},
		},
	}

	filterRes := cost.FilterCandidateProviders(cost.ProviderFilterRequest{
		Config:      cfg,
		Providers:   []string{"provider-cheap", "provider-expensive"},
		Model:       "shared-model",
		Measurement: cost.FromTokens(100, 50, 0, 0, 0),
	})

	if len(filterRes.AllowedProviders) != 1 || filterRes.AllowedProviders[0] != "provider-cheap" {
		t.Fatalf("expected only provider-cheap to be allowed, got: %+v", filterRes.AllowedProviders)
	}
}

// Scenario 5: Fallback to same price provider allowed
func TestE2E_Scenario5_FallbackSamePriceAllowed(t *testing.T) {
	cfg := &config.Config{
		CostGuard: config.CostGuardConfig{
			Enabled: true,
		},
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name: "provider-a",
				CostPolicy: &config.ProviderCostPolicy{
					AllowFallback: true,
				},
				Models: []config.OpenAICompatibilityModel{
					{
						Name: "shared-model",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus:          "paid_confirmed",
							BillingUnit:          "tokens",
							PromptPer1M:          1.0,
							CompletionPer1M:      2.0,
							PromptConfigured:     true,
							CompletionConfigured: true,
						},
					},
				},
			},
			{
				Name: "provider-b",
				Models: []config.OpenAICompatibilityModel{
					{
						Name: "shared-model",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus:          "paid_confirmed",
							BillingUnit:          "tokens",
							PromptPer1M:          1.0,
							CompletionPer1M:      2.0,
							PromptConfigured:     true,
							CompletionConfigured: true,
						},
					},
				},
			},
		},
	}

	filterRes := cost.FilterCandidateProviders(cost.ProviderFilterRequest{
		Config:      cfg,
		Providers:   []string{"provider-a", "provider-b"},
		Model:       "shared-model",
		Measurement: cost.FromTokens(100, 50, 0, 0, 0),
	})

	if len(filterRes.AllowedProviders) != 2 {
		t.Fatalf("expected both same-price providers allowed, got: %+v", filterRes.AllowedProviders)
	}
}

// Scenario 6: Global budget exceeded blocks all providers
func TestE2E_Scenario6_GlobalBudgetExceededBlocksAll(t *testing.T) {
	dir := t.TempDir()
	budgetStore, _ := cost.NewPersistentBudgetStore(dir)

	cfg := &config.Config{
		CostGuard: config.CostGuardConfig{
			Enabled: true,
			Budget: config.CostBudgetPolicy{
				DailyUSD: 0.000001,
			},
		},
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name: "prov-1",
				Models: []config.OpenAICompatibilityModel{
					{
						Name: "m1",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus:          "paid_confirmed",
							BillingUnit:          "tokens",
							PromptPer1M:          100.0,
							CompletionPer1M:      200.0,
							PromptConfigured:     true,
							CompletionConfigured: true,
						},
					},
				},
			},
		},
	}

	_, _ = budgetStore.Reserve(context.Background(), cost.ReservationRequest{
		ReservationKey:        "res-1",
		Provider:              "prov-1",
		Model:                 "m1",
		BillingUnit:           "tokens",
		AmountNanos:           2000,
		GlobalDailyLimitNanos: 1000,
	})

	dec := cost.EvaluatePreflight(cost.GateRequest{
		Config:      cfg,
		Provider:    "prov-1",
		Model:       "m1",
		Measurement: cost.FromTokens(1000, 500, 0, 0, 0),
		BudgetStore: budgetStore,
	})

	if dec.Allowed || dec.Reason != cost.ReasonGlobalBudgetExceeded {
		t.Fatalf("expected GlobalBudgetExceeded deny, got: %+v", dec)
	}
}

func makeTestPolicy(prov, model string, price float64) *cost.PolicySnapshot {
	p := &cost.PolicySnapshot{
		SchemaVersion:            "cost-policy/v1",
		PolicyVersion:            "cpamp-" + prov + "-" + model + "-r1",
		Provider:                 prov,
		PublicModel:              model,
		UpstreamModelHash:        "upstream-hash-1",
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
	if price == 0 {
		p.PriceStatus = "free_confirmed"
		p.CompletionPer1M = 0
		p.CachePer1M = 0
	}
	p.ContentHash = p.ComputeContentHash()
	return p
}

// Scenario 7: Upstream actual cost exceeds reserved triggers overrun freeze
func TestE2E_Scenario7_ActualCostExceedsReservedTriggersOverrunFreeze(t *testing.T) {
	dir := t.TempDir()
	budgetStore, _ := cost.NewPersistentBudgetStore(dir)

	ctx := context.Background()
	res, err := budgetStore.Reserve(ctx, cost.ReservationRequest{
		ReservationKey: "req-overrun-1",
		Provider:       "overrun-prov",
		Model:          "overrun-model",
		BillingUnit:    "tokens",
		AmountNanos:    1000,
	})
	if err != nil {
		t.Fatal(err)
	}

	errSettle := budgetStore.Settle(ctx, res.ID, 2000, 0, 0)
	if !errors.Is(errSettle, cost.ErrBudgetOverrun) {
		t.Fatalf("expected ErrBudgetOverrun, got: %v", errSettle)
	}

	isFrozen, _ := budgetStore.IsFrozen(ctx, "overrun-prov", "overrun-model")
	if !isFrozen {
		t.Fatal("expected provider to be frozen")
	}

	dec := cost.EvaluatePreflight(cost.GateRequest{
		Config: &config.Config{
			CostGuard: config.CostGuardConfig{Enabled: true},
			OpenAICompatibility: []config.OpenAICompatibility{
				{
					Name: "overrun-prov",
					Models: []config.OpenAICompatibilityModel{
						{
							Name: "overrun-model",
							CostPolicy: &config.ModelCostPolicy{
								PriceStatus:      "paid_confirmed",
								BillingUnit:      "tokens",
								PromptPer1M:      1.0,
								PromptConfigured: true,
							},
						},
					},
				},
			},
		},
		Provider:    "overrun-prov",
		Model:       "overrun-model",
		Measurement: cost.FromTokens(10, 0, 0, 0, 0),
		BudgetStore: budgetStore,
	})

	if dec.Allowed || dec.Reason != cost.ReasonBudgetOverrunFreeze {
		t.Fatalf("expected BudgetOverrunFreeze deny, got: %+v", dec)
	}

	_ = budgetStore.UnfreezeProvider(ctx, "overrun-prov")
	_ = budgetStore.UnfreezeModel(ctx, "overrun-prov", "overrun-model")
	isFrozenAfter, _ := budgetStore.IsFrozen(ctx, "overrun-prov", "overrun-model")
	if isFrozenAfter {
		t.Fatal("expected provider and model to be unfrozen")
	}
}

// Scenario 8: Policy Hash error rejected
func TestE2E_Scenario8_PolicyHashErrorRejected(t *testing.T) {
	dir := t.TempDir()
	policyStore, _ := cost.NewPersistentPolicyStore(dir)

	// Valid policy active
	p1 := makeTestPolicy("openai", "gpt-4o", 2.5)
	_, _ = policyStore.Activate(p1, false)

	// Tampered policy (content modified, hash outdated)
	tampered := makeTestPolicy("openai", "gpt-4o", 5.0)
	tampered.ContentHash = p1.ContentHash // mismatch!

	_, err := policyStore.Activate(tampered, true)
	if err == nil || !strings.Contains(err.Error(), "content_hash mismatch") {
		t.Fatalf("expected hash mismatch error, got: %v", err)
	}

	// Current active policy is unaffected
	active, _ := policyStore.GetActivePolicy("openai", "gpt-4o")
	if active.PromptPer1M != 2.5 {
		t.Fatalf("expected active policy to remain unchanged, got: %f", active.PromptPer1M)
	}
}

// Scenario 9: Expired policy rejected
func TestE2E_Scenario9_ExpiredPolicyRejected(t *testing.T) {
	dir := t.TempDir()
	policyStore, _ := cost.NewPersistentPolicyStore(dir)

	expired := makeTestPolicy("openai", "old-model", 1.0)
	expired.EffectiveAtMS = 1000
	expired.ExpiresAtMS = 2000 // In the past
	expired.ContentHash = expired.ComputeContentHash()

	_, err := policyStore.Activate(expired, true)
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expected error activating expired policy, got: %v", err)
	}
}

// Scenario 10: Process restart recovery
func TestE2E_Scenario10_ProcessRestartRecovery(t *testing.T) {
	dir := t.TempDir()

	// Instance 1
	store1, _ := cost.NewPersistentPolicyStore(dir)
	bStore1, _ := cost.NewPersistentBudgetStore(dir)

	snap := makeTestPolicy("google", "gemini-1.5-pro", 3.5)
	_, _ = store1.Activate(snap, true)

	// Cause an overrun freeze on Instance 1
	ctx := context.Background()
	res, _ := bStore1.Reserve(ctx, cost.ReservationRequest{
		ReservationKey: "req-freeze-restart",
		Provider:       "google",
		Model:          "gemini-1.5-pro",
		AmountNanos:    100,
	})
	_ = bStore1.Settle(ctx, res.ID, 500, 0, 0)

	// Simulate process restart -> Instance 2 loads same dir
	store2, err := cost.NewPersistentPolicyStore(dir)
	if err != nil {
		t.Fatalf("store restart failed: %v", err)
	}
	bStore2, err := cost.NewPersistentBudgetStore(dir)
	if err != nil {
		t.Fatalf("budget store restart failed: %v", err)
	}

	recoveredPolicy, ok := store2.GetActivePolicy("google", "gemini-1.5-pro")
	if !ok || recoveredPolicy.PromptPer1M != 3.5 {
		t.Fatalf("policy not recovered correctly: %+v", recoveredPolicy)
	}

	isFrozen, _ := bStore2.IsFrozen(ctx, "google", "gemini-1.5-pro")
	if !isFrozen {
		t.Fatal("freeze state not recovered correctly after restart")
	}
}

// Modality & Usage Audit Verification
func TestE2E_ModalityAndUsageAuditVerification(t *testing.T) {
	dir := t.TempDir()
	policyStore, _ := cost.NewPersistentPolicyStore(dir)

	// ASR with and without duration
	asrWithDur := cost.ExtractNativeMeasurement("asr", []byte(`{"duration":12.5}`), "application/json")
	if asrWithDur.AudioSeconds != 12.5 {
		t.Fatalf("expected 12.5 audio seconds, got: %f", asrWithDur.AudioSeconds)
	}
	asrWithoutDur := cost.ExtractNativeMeasurement("asr", []byte(`{}`), "application/json")
	if asrWithoutDur.AudioSecondsPresent {
		t.Fatal("expected quantity unknown for ASR without duration")
	}

	// TTS unicode chars
	ttsMeasurement := cost.ExtractNativeMeasurement("tts", []byte(`{"input":"你好世界 Hello"}`), "application/json")
	if ttsMeasurement.Characters != 10 { // 4 chinese + 1 space + 5 english = 10 chars
		t.Fatalf("expected 10 characters, got %d", ttsMeasurement.Characters)
	}

	// Image count
	imgMeasurement := cost.ExtractNativeMeasurement("image", []byte(`{"n":3}`), "application/json")
	if imgMeasurement.ImageCount != 3 {
		t.Fatalf("expected 3 images, got %d", imgMeasurement.ImageCount)
	}

	// Video submit vs status
	vidSubmit := cost.ExtractNativeMeasurement("video", []byte(`{"prompt":"cat"}`), "application/json")
	if vidSubmit.VideoTasks != 1 || vidSubmit.IsVideoStatus {
		t.Fatalf("expected video submit operation 1, got %+v", vidSubmit)
	}
	vidStatus := cost.ExtractNativeMeasurement("video_status", []byte(`{}`), "application/json")
	if !vidStatus.IsVideoStatus {
		t.Fatal("expected video status to be recognized")
	}

	// Verify Audit Events
	snap := makeTestPolicy("openai", "gpt-4o", 2.5)
	_, _ = policyStore.Activate(snap, true)
	_ = policyStore.Revoke("openai", "gpt-4o", snap.PolicyVersion, "test audit")

	events := policyStore.ListAuditEvents(10, 0)
	if len(events) < 2 {
		t.Fatalf("expected at least 2 audit events, got %d", len(events))
	}
	foundRevoke := false
	for _, e := range events {
		if e.Action == "revoke" {
			foundRevoke = true
			if e.Version != snap.PolicyVersion {
				t.Fatalf("unexpected audit event details: %+v", e)
			}
		}
	}
	if !foundRevoke {
		t.Fatal("expected revoke action in audit events")
	}
}
