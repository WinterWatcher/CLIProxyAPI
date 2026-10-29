package cost

import (
	"testing"
)

func TestPolicySnapshotContractValidationAndHash(t *testing.T) {
	snap := PolicySnapshot{
		PolicyVersion:     "v1.0.0",
		Provider:          "openai",
		PublicModel:       "gpt-4o",
		UpstreamModelHash: "hash-upstream-1",
		PriceStatus:       "paid_confirmed",
		BillingUnit:       "tokens",
		PromptPer1M:       2.5,
		CompletionPer1M:   10.0,
		CachePer1M:        1.25,
		EffectiveAtMS:     1700000000000,
	}

	snap.ContentHash = snap.ComputeContentHash()
	if snap.ContentHash == "" {
		t.Fatal("expected non-empty content hash")
	}

	if err := snap.Validate(); err != nil {
		t.Fatalf("validation failed: %v", err)
	}

	// Mismatched hash must fail
	snap.ContentHash = "tampered-hash"
	if err := snap.Validate(); err == nil {
		t.Fatal("expected validation error for tampered hash")
	}
}
