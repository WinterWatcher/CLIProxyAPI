package cost

import (
	"strings"
	"testing"
)

func sampleValidPolicy() *PolicySnapshot {
	return &PolicySnapshot{
		SchemaVersion:            "cost-policy/v1",
		PolicyVersion:            "cpamp-openai-gpt-4o-r1",
		Provider:                 "openai",
		PublicModel:              "gpt-4o",
		UpstreamModelHash:        "upstream-hash-123",
		PriceStatus:              "paid_confirmed",
		BillingUnit:              "tokens",
		PromptPer1M:              2.5,
		CompletionPer1M:          10.0,
		CachePer1M:               1.25,
		CacheReadPer1M:           1.25,
		CacheCreationPer1M:       2.5,
		UnitPrice:                0.0,
		MaxRequestCostUSD:        1.0,
		ProviderDailyBudgetUSD:   100.0,
		ProviderMonthlyBudgetUSD: 2000.0,
		GlobalDailyBudgetUSD:     500.0,
		GlobalMonthlyBudgetUSD:   10000.0,
		EffectiveAtMS:            1700000000000,
		ExpiresAtMS:              1800000000000,
		Source:                   "cpamp-approved",
	}
}

func TestPolicySnapshot_DeterministicContentHash(t *testing.T) {
	p1 := sampleValidPolicy()
	hash1 := p1.ComputeContentHash()
	if !strings.HasPrefix(hash1, "sha256:") {
		t.Fatalf("expected sha256: prefix, got %s", hash1)
	}

	p2 := sampleValidPolicy()
	hash2 := p2.ComputeContentHash()
	if hash1 != hash2 {
		t.Fatalf("expected identical hashes, got %s != %s", hash1, hash2)
	}
}

func TestPolicySnapshot_JSONFieldOrderDoesNotAffectHash(t *testing.T) {
	// JSON 1
	json1 := `{
		"schema_version": "cost-policy/v1",
		"policy_version": "v1",
		"provider": "openai",
		"public_model": "gpt-4o",
		"price_status": "paid_confirmed",
		"billing_unit": "tokens",
		"prompt_per_1m": 2.5,
		"effective_at_ms": 1700000000000
	}`
	// JSON 2: reversed fields
	json2 := `{
		"effective_at_ms": 1700000000000,
		"prompt_per_1m": 2.5,
		"billing_unit": "tokens",
		"price_status": "paid_confirmed",
		"public_model": "gpt-4o",
		"provider": "openai",
		"policy_version": "v1",
		"schema_version": "cost-policy/v1"
	}`

	p1, err1 := UnmarshalPolicySnapshotStrict([]byte(json1))
	if err1 != nil {
		t.Fatalf("failed to unmarshal json1: %v", err1)
	}
	p2, err2 := UnmarshalPolicySnapshotStrict([]byte(json2))
	if err2 != nil {
		t.Fatalf("failed to unmarshal json2: %v", err2)
	}

	if p1.ComputeContentHash() != p2.ComputeContentHash() {
		t.Fatalf("expected hash to be order-independent, got %s != %s", p1.ComputeContentHash(), p2.ComputeContentHash())
	}
}

func TestPolicySnapshot_PriceChangeChangesHash(t *testing.T) {
	p1 := sampleValidPolicy()
	hash1 := p1.ComputeContentHash()

	p2 := sampleValidPolicy()
	p2.PromptPer1M = 2.500001
	hash2 := p2.ComputeContentHash()

	if hash1 == hash2 {
		t.Fatal("expected hash to change when prompt price changes")
	}
}

func TestPolicySnapshot_ZeroPriceVsNonZero(t *testing.T) {
	p1 := sampleValidPolicy()
	p1.PromptPer1M = 0.0
	hash1 := p1.ComputeContentHash()

	p2 := sampleValidPolicy()
	p2.PromptPer1M = 0.000001
	hash2 := p2.ComputeContentHash()

	if hash1 == hash2 {
		t.Fatal("expected zero price and non-zero price to have different hash")
	}
}

func TestPolicySnapshot_ProviderChangeChangesHash(t *testing.T) {
	p1 := sampleValidPolicy()
	hash1 := p1.ComputeContentHash()

	p2 := sampleValidPolicy()
	p2.Provider = "anthropic"
	hash2 := p2.ComputeContentHash()

	if hash1 == hash2 {
		t.Fatal("expected hash to change when provider changes")
	}
}

func TestPolicySnapshot_BillingUnitChangeChangesHash(t *testing.T) {
	p1 := sampleValidPolicy()
	hash1 := p1.ComputeContentHash()

	p2 := sampleValidPolicy()
	p2.BillingUnit = "characters"
	hash2 := p2.ComputeContentHash()

	if hash1 == hash2 {
		t.Fatal("expected hash to change when billing unit changes")
	}
}

func TestPolicySnapshot_SignatureExcludedFromContentHash(t *testing.T) {
	p := sampleValidPolicy()
	hashBefore := p.ComputeContentHash()

	p.Signature = "hmac-sha256:some-arbitrary-sig"
	hashAfter := p.ComputeContentHash()

	if hashBefore != hashAfter {
		t.Fatalf("signature must not participate in content_hash: %s != %s", hashBefore, hashAfter)
	}
}

func TestPolicySnapshot_SignatureVerification(t *testing.T) {
	p := sampleValidPolicy()
	secret := "test-secret-key-12345"

	sig := p.ComputeSignature(secret)
	if !strings.HasPrefix(sig, "hmac-sha256:") {
		t.Fatalf("expected hmac-sha256: prefix, got %s", sig)
	}
	p.Signature = sig

	if !p.VerifySignature(secret) {
		t.Fatal("signature verification failed with correct secret")
	}

	if p.VerifySignature("wrong-secret") {
		t.Fatal("signature verification should have failed with wrong secret")
	}
}

func TestPolicySnapshot_RejectsUnknownPolicy(t *testing.T) {
	p := sampleValidPolicy()
	p.PriceStatus = "unknown"
	p.ContentHash = p.ComputeContentHash()

	err := p.Validate()
	if err == nil || !strings.Contains(err.Error(), "unknown price policy is not allowed") {
		t.Fatalf("expected error for unknown price policy, got %v", err)
	}
}

func TestPolicySnapshot_RejectsFreeConfirmedWithNonZeroPrice(t *testing.T) {
	p := sampleValidPolicy()
	p.PriceStatus = "free_confirmed"
	p.PromptPer1M = 0.5
	p.ContentHash = p.ComputeContentHash()

	err := p.Validate()
	if err == nil || !strings.Contains(err.Error(), "cannot have non-zero prices") {
		t.Fatalf("expected error for free_confirmed with non-zero price, got %v", err)
	}
}

func TestPolicySnapshot_RejectsExpiredTimestamp(t *testing.T) {
	p := sampleValidPolicy()
	p.EffectiveAtMS = 1700000000000
	p.ExpiresAtMS = 1600000000000 // before effective

	err := p.Validate()
	if err == nil || !strings.Contains(err.Error(), "expires_at_ms cannot be earlier than effective_at_ms") {
		t.Fatalf("expected error for expires_at < effective_at, got %v", err)
	}

	// Check IsExpired helper
	p.ExpiresAtMS = 1750000000000
	if !p.IsExpired(1760000000000) {
		t.Fatal("expected IsExpired to be true when now > expires_at")
	}
	if p.IsExpired(1740000000000) {
		t.Fatal("expected IsExpired to be false when now < expires_at")
	}
}

func TestPolicySnapshot_StrictJSONRejectsProhibitedFields(t *testing.T) {
	badJSON := `{
		"schema_version": "cost-policy/v1",
		"policy_version": "v1",
		"provider": "openai",
		"public_model": "gpt-4o",
		"price_status": "paid_confirmed",
		"billing_unit": "tokens",
		"effective_at_ms": 1700000000000,
		"illegal_field": "injected_secret"
	}`

	_, err := UnmarshalPolicySnapshotStrict([]byte(badJSON))
	if err == nil {
		t.Fatal("expected error when unknown field is present")
	}
	if !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected unknown field error, got: %v", err)
	}
}

func TestPolicySnapshot_RejectsSecretsInMetadata(t *testing.T) {
	p := sampleValidPolicy()
	p.Source = "Bearer sk-1234567890abcdef"
	p.ContentHash = p.ComputeContentHash()

	err := p.Validate()
	if err == nil || !strings.Contains(err.Error(), "prohibited sensitive content") {
		t.Fatalf("expected error for secret in metadata, got %v", err)
	}
}
