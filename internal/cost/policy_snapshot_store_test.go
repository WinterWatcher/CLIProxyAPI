package cost

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func sampleStorePolicy(prov, model string, promptPrice float64) *PolicySnapshot {
	p := &PolicySnapshot{
		SchemaVersion:            "cost-policy/v1",
		PolicyVersion:            "cpamp-" + prov + "-" + model + "-r1",
		Provider:                 prov,
		PublicModel:              model,
		UpstreamModelHash:        "upstream-hash-1",
		PriceStatus:              "paid_confirmed",
		BillingUnit:              "tokens",
		PromptPer1M:              promptPrice,
		CompletionPer1M:          promptPrice * 2,
		CachePer1M:               promptPrice * 0.5,
		UnitPrice:                0.0,
		MaxRequestCostUSD:        1.0,
		ProviderDailyBudgetUSD:   100.0,
		ProviderMonthlyBudgetUSD: 2000.0,
		GlobalDailyBudgetUSD:     500.0,
		GlobalMonthlyBudgetUSD:   10000.0,
		EffectiveAtMS:            time.Now().UnixMilli(),
		Source:                   "cpamp-approved",
	}
	if promptPrice == 0 {
		p.PriceStatus = "free_confirmed"
		p.CompletionPer1M = 0
		p.CachePer1M = 0
	}
	p.ContentHash = p.ComputeContentHash()
	return p
}

func TestPolicyStore_ActivationAndIdempotency(t *testing.T) {
	dir := t.TempDir()
	store, err := NewPersistentPolicyStore(dir)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}

	snap := sampleStorePolicy("openai", "gpt-4o", 2.5)

	// First activation
	act1, err := store.Activate(snap, false)
	if err != nil {
		t.Fatalf("activation failed: %v", err)
	}
	if act1.PolicyVersion != snap.PolicyVersion {
		t.Fatalf("unexpected version: %s", act1.PolicyVersion)
	}

	// Idempotent second activation
	act2, err := store.Activate(snap, false)
	if err != nil {
		t.Fatalf("idempotent activation failed: %v", err)
	}
	if act2.PolicyVersion != snap.PolicyVersion || act2.ContentHash != snap.ContentHash {
		t.Fatalf("idempotent activation mismatch: %s vs %s", act1.ContentHash, act2.ContentHash)
	}

	// Hash tracking
	actHash := store.GetActivePolicyHash("openai", "gpt-4o")
	if actHash != snap.ContentHash {
		t.Fatalf("active hash mismatch: got %s, want %s", actHash, snap.ContentHash)
	}
}

func TestPolicyStore_DryRunRejectionRules(t *testing.T) {
	dir := t.TempDir()
	store, err := NewPersistentPolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Activate Free policy
	freeSnap := sampleStorePolicy("openai", "free-model", 0.0)
	_, err = store.Activate(freeSnap, false)
	if err != nil {
		t.Fatal(err)
	}

	// 2. Candidate: Free to Paid -> Deny
	paidCandidate := sampleStorePolicy("openai", "free-model", 5.0)
	paidCandidate.PolicyVersion = "cpamp-openai-free-model-r2"
	paidCandidate.EffectiveAtMS = time.Now().UnixMilli() + 1000
	paidCandidate.ContentHash = paidCandidate.ComputeContentHash()

	report, err := store.DryRun(paidCandidate)
	if err != nil {
		t.Fatal(err)
	}
	if report.Allowed || report.Decision != "deny" || report.RiskLevel != "critical" {
		t.Fatalf("expected critical deny for free to paid, got: %+v", report)
	}

	// Activating without force should fail
	_, err = store.Activate(paidCandidate, false)
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("expected error activating denied policy without force, got: %v", err)
	}

	// Verify old free policy remains active!
	curr, ok := store.GetActivePolicy("openai", "free-model")
	if !ok || curr.PriceStatus != "free_confirmed" {
		t.Fatalf("expected old free policy to remain active, got: %+v", curr)
	}

	// 3. Candidate: Billing Unit change -> Deny
	unitChangeCandidate := sampleStorePolicy("openai", "free-model", 0.0)
	unitChangeCandidate.BillingUnit = "characters"
	unitChangeCandidate.PolicyVersion = "cpamp-openai-free-model-r3"
	unitChangeCandidate.EffectiveAtMS = time.Now().UnixMilli() + 2000
	unitChangeCandidate.ContentHash = unitChangeCandidate.ComputeContentHash()

	reportUnit, err := store.DryRun(unitChangeCandidate)
	if err != nil {
		t.Fatal(err)
	}
	if reportUnit.Allowed || reportUnit.Decision != "deny" || reportUnit.RiskLevel != "critical" {
		t.Fatalf("expected critical deny for unit change, got: %+v", reportUnit)
	}

	// 4. Candidate: Upstream model remapped -> Deny
	remapCandidate := sampleStorePolicy("openai", "free-model", 0.0)
	remapCandidate.UpstreamModelHash = "different-upstream-hash"
	remapCandidate.PolicyVersion = "cpamp-openai-free-model-r4"
	remapCandidate.EffectiveAtMS = time.Now().UnixMilli() + 3000
	remapCandidate.ContentHash = remapCandidate.ComputeContentHash()

	reportRemap, err := store.DryRun(remapCandidate)
	if err != nil {
		t.Fatal(err)
	}
	if reportRemap.Allowed || reportRemap.Decision != "deny" || reportRemap.RiskLevel != "critical" {
		t.Fatalf("expected critical deny for remap, got: %+v", reportRemap)
	}
}

func TestPolicyStore_RollbackAndRevoke(t *testing.T) {
	dir := t.TempDir()
	store, err := NewPersistentPolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	// V1
	v1 := sampleStorePolicy("anthropic", "claude-3-opus", 15.0)
	v1.PolicyVersion = "v1"
	v1.ContentHash = v1.ComputeContentHash()
	_, err = store.Activate(v1, true)
	if err != nil {
		t.Fatal(err)
	}

	// V2
	v2 := sampleStorePolicy("anthropic", "claude-3-opus", 20.0)
	v2.PolicyVersion = "v2"
	v2.EffectiveAtMS = time.Now().UnixMilli() + 1000
	v2.ContentHash = v2.ComputeContentHash()
	_, err = store.Activate(v2, true)
	if err != nil {
		t.Fatal(err)
	}

	// Check previous policy hash
	prevHash := store.GetPreviousPolicyHash("anthropic", "claude-3-opus")
	if prevHash != v1.ContentHash {
		t.Fatalf("previous hash mismatch: got %s want %s", prevHash, v1.ContentHash)
	}

	// Rollback to v1
	rolled, err := store.Rollback("anthropic", "claude-3-opus", "v1")
	if err != nil {
		t.Fatalf("rollback failed: %v", err)
	}
	if rolled.PromptPer1M != 15.0 {
		t.Fatalf("expected rolled back price to be 15.0, got: %f", rolled.PromptPer1M)
	}

	// Revoke v1
	err = store.Revoke("anthropic", "claude-3-opus", "v1", "revoked by admin")
	if err != nil {
		t.Fatalf("revoke failed: %v", err)
	}

	// Verify revoked policy cannot be re-activated
	_, err = store.Activate(v1, true)
	if err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("expected error activating revoked policy, got: %v", err)
	}
}

func TestPolicyStore_PersistenceAndRestart(t *testing.T) {
	dir := t.TempDir()
	store1, err := NewPersistentPolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	snap := sampleStorePolicy("meta", "llama-3-70b", 0.9)
	_, err = store1.Activate(snap, true)
	if err != nil {
		t.Fatal(err)
	}

	// Restart store
	store2, err := NewPersistentPolicyStore(dir)
	if err != nil {
		t.Fatalf("failed to reload store: %v", err)
	}

	recovered, ok := store2.GetActivePolicy("meta", "llama-3-70b")
	if !ok || recovered == nil {
		t.Fatal("expected policy to survive restart")
	}
	if recovered.ContentHash != snap.ContentHash {
		t.Fatalf("recovered hash mismatch: got %s, want %s", recovered.ContentHash, snap.ContentHash)
	}
}

func TestPolicyStore_FailClosedOnCorruptJSON(t *testing.T) {
	dir := t.TempDir()
	storeFile := filepath.Join(dir, "cost_policies.json")
	if err := os.WriteFile(storeFile, []byte("{ corrupt json: ["), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := NewPersistentPolicyStore(dir)
	if err == nil || !strings.Contains(err.Error(), "corrupted") {
		t.Fatalf("expected fail-closed error on corrupt JSON, got: %v", err)
	}
}

func TestPolicyStore_GateIntegration(t *testing.T) {
	dir := t.TempDir()
	store, _ := NewPersistentPolicyStore(dir)

	cfg := &config.Config{
		CostGuard: config.CostGuardConfig{
			Enabled:            true,
			UnknownPriceAction: config.CostActionDeny,
		},
	}

	// 1. Without active policy in store or config -> fail-closed
	req1 := GateRequest{
		Config:      cfg,
		Provider:    "test-prov",
		Model:       "unregistered-model",
		PolicyStore: store,
		Measurement: FromTokens(100, 50, 0, 0, 0),
	}
	dec1 := EvaluatePreflight(req1)
	if dec1.Allowed || dec1.Decision != DecisionDeny {
		t.Fatalf("expected fail-closed deny without policy, got: %+v", dec1)
	}

	// 2. Activate valid policy
	snap := sampleStorePolicy("test-prov", "unregistered-model", 1.0)
	_, _ = store.Activate(snap, true)

	dec2 := EvaluatePreflight(req1)
	if !dec2.Allowed || dec2.Decision != DecisionAllow {
		t.Fatalf("expected allow after activating policy, got: %+v", dec2)
	}
	if dec2.PolicyVersion != snap.PolicyVersion || dec2.ContentHash != snap.ContentHash {
		t.Fatalf("expected decision to record policy version and hash, got: %+v", dec2)
	}

	// 3. Expired policy -> fail-closed
	snapExpired := sampleStorePolicy("test-prov", "expired-model", 1.0)
	snapExpired.EffectiveAtMS = 1000
	snapExpired.ExpiresAtMS = 2000 // In the distant past
	snapExpired.ContentHash = snapExpired.ComputeContentHash()
	// Store directly in activePolicies to simulate expiration over time
	store.activePolicies[policyKey("test-prov", "expired-model")] = snapExpired

	reqExpired := GateRequest{
		Config:      cfg,
		Provider:    "test-prov",
		Model:       "expired-model",
		PolicyStore: store,
	}
	decExpired := EvaluatePreflight(reqExpired)
	if decExpired.Allowed || decExpired.Decision != DecisionDeny {
		t.Fatalf("expected expired policy to fail closed, got: %+v", decExpired)
	}
}
