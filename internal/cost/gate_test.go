package cost

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestGateCostGuardDisabledAllowsEverything(t *testing.T) {
	cfg := &config.Config{
		CostGuard: config.CostGuardConfig{
			Enabled: false,
		},
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name: "prov1",
				Models: []config.OpenAICompatibilityModel{
					{Name: "unpriced-model", Alias: "unpriced-model"}, // no cost policy
				},
			},
		},
	}

	dec := EvaluatePreflight(GateRequest{
		Config:   cfg,
		Provider: "prov1",
		Model:    "unpriced-model",
	})
	if !dec.Allowed {
		t.Fatalf("expected allowed when cost-guard is disabled, got: %+v", dec)
	}
	if dec.Reason != ReasonCostGuardDisabled {
		t.Errorf("expected reason %s, got %s", ReasonCostGuardDisabled, dec.Reason)
	}
}

func TestGateCostGuardEnabledMissingModelPolicyDenies(t *testing.T) {
	cfg := &config.Config{
		CostGuard: config.CostGuardConfig{
			Enabled:            true,
			UnknownPriceAction: config.CostActionDeny,
		},
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name: "prov1",
				Models: []config.OpenAICompatibilityModel{
					{Name: "unpriced-model", Alias: "unpriced-model"}, // no cost policy
				},
			},
		},
	}

	dec := EvaluatePreflight(GateRequest{
		Config:   cfg,
		Provider: "prov1",
		Model:    "unpriced-model",
	})
	if dec.Allowed {
		t.Fatalf("expected denied for missing model cost policy, got: %+v", dec)
	}
	if dec.Reason != ReasonUnknownPrice {
		t.Errorf("expected reason %s, got %s", ReasonUnknownPrice, dec.Reason)
	}
	if dec.Code != CodePricePolicyViolation {
		t.Errorf("expected code %s, got %s", CodePricePolicyViolation, dec.Code)
	}
}

func TestGateCostGuardEnabledUnknownPriceStatusDenies(t *testing.T) {
	cfg := &config.Config{
		CostGuard: config.CostGuardConfig{
			Enabled:            true,
			UnknownPriceAction: config.CostActionDeny,
		},
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name: "prov1",
				Models: []config.OpenAICompatibilityModel{
					{
						Name:  "unknown-price-model",
						Alias: "unknown-price-model",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus: config.PriceStatusUnknown,
						},
					},
				},
			},
		},
	}

	dec := EvaluatePreflight(GateRequest{
		Config:   cfg,
		Provider: "prov1",
		Model:    "unknown-price-model",
	})
	if dec.Allowed {
		t.Fatalf("expected denied for unknown price status: %+v", dec)
	}
	if dec.Reason != ReasonUnknownPrice {
		t.Errorf("expected reason %s, got %s", ReasonUnknownPrice, dec.Reason)
	}
}

func TestGateCostGuardEnabledFrozenModelDenies(t *testing.T) {
	cfg := &config.Config{
		CostGuard: config.CostGuardConfig{
			Enabled: true,
		},
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name: "prov1",
				Models: []config.OpenAICompatibilityModel{
					{
						Name:  "frozen-model",
						Alias: "frozen-model",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus: config.PriceStatusFrozen,
						},
					},
				},
			},
		},
	}

	dec := EvaluatePreflight(GateRequest{
		Config:   cfg,
		Provider: "prov1",
		Model:    "frozen-model",
	})
	if dec.Allowed {
		t.Fatalf("expected denied for frozen model: %+v", dec)
	}
	if dec.Reason != ReasonModelPriceFrozen {
		t.Errorf("expected reason %s, got %s", ReasonModelPriceFrozen, dec.Reason)
	}
	if dec.Decision != DecisionFreeze {
		t.Errorf("expected decision %s, got %s", DecisionFreeze, dec.Decision)
	}
}

func TestGateFreeConfirmedZeroPriceAllows(t *testing.T) {
	cfg := &config.Config{
		CostGuard: config.CostGuardConfig{
			Enabled: true,
		},
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name: "prov1",
				Models: []config.OpenAICompatibilityModel{
					{
						Name:  "free-model",
						Alias: "free-model",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus: config.PriceStatusFreeConfirmed,
							BillingUnit: config.BillingUnitTokens,
						},
					},
				},
			},
		},
	}

	// 100k input tokens on free model -> cost is 0 -> allowed!
	dec := EvaluatePreflight(GateRequest{
		Config:      cfg,
		Provider:    "prov1",
		Model:       "free-model",
		Measurement: FromTokens(100_000, 0, 0, 0, 0),
	})
	if !dec.Allowed {
		t.Fatalf("expected free_confirmed model to be allowed, got: %+v", dec)
	}
	if dec.Reason != ReasonExplicitFree {
		t.Errorf("expected reason %s, got %s", ReasonExplicitFree, dec.Reason)
	}
	if dec.EstimatedCost.Amount != 0 || !dec.EstimatedCost.ExplicitlyFree {
		t.Errorf("expected 0 cost and explicitly free: %+v", dec.EstimatedCost)
	}
}

func TestGatePaidConfirmedWithinBudgetAllows(t *testing.T) {
	cfg := &config.Config{
		CostGuard: config.CostGuardConfig{
			Enabled: true,
		},
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name: "prov1",
				Models: []config.OpenAICompatibilityModel{
					{
						Name:  "paid-model",
						Alias: "paid-model",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus:          config.PriceStatusPaidConfirmed,
							BillingUnit:          config.BillingUnitTokens,
							PromptPer1M:          2.0,
							PromptConfigured:     true,
							CompletionPer1M:      5.0,
							CompletionConfigured: true,
							MaxRequestCost:       0.10, // max $0.10
						},
					},
				},
			},
		},
	}

	// 10k prompt tokens at $2/1M = $0.02 (<= $0.10) -> allow!
	dec := EvaluatePreflight(GateRequest{
		Config:      cfg,
		Provider:    "prov1",
		Model:       "paid-model",
		Measurement: FromTokens(10_000, 0, 0, 0, 0),
	})
	if !dec.Allowed {
		t.Fatalf("expected paid model within budget to be allowed: %+v", dec)
	}
	if dec.Reason != ReasonAllowed {
		t.Errorf("expected reason %s, got %s", ReasonAllowed, dec.Reason)
	}
}

func TestGatePaidConfirmedCostExceedsMaxRequestCostDenies(t *testing.T) {
	cfg := &config.Config{
		CostGuard: config.CostGuardConfig{
			Enabled: true,
		},
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name: "prov1",
				Models: []config.OpenAICompatibilityModel{
					{
						Name:  "expensive-model",
						Alias: "expensive-model",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus:          config.PriceStatusPaidConfirmed,
							BillingUnit:          config.BillingUnitTokens,
							PromptPer1M:          100.0,
							PromptConfigured:     true,
							CompletionPer1M:      200.0,
							CompletionConfigured: true,
							MaxRequestCost:       0.50, // max $0.50
						},
					},
				},
			},
		},
	}

	// 10k prompt tokens at $100/1M = $1.00 (> $0.50) -> deny!
	dec := EvaluatePreflight(GateRequest{
		Config:      cfg,
		Provider:    "prov1",
		Model:       "expensive-model",
		Measurement: FromTokens(10_000, 0, 0, 0, 0),
	})
	if dec.Allowed {
		t.Fatalf("expected cost exceeding max-request-cost to be denied: %+v", dec)
	}
	if dec.Reason != ReasonMaxRequestCostExceeded {
		t.Errorf("expected reason %s, got %s", ReasonMaxRequestCostExceeded, dec.Reason)
	}
}

func TestGatePaidConfirmedMissingQuantityDenies(t *testing.T) {
	cfg := &config.Config{
		CostGuard: config.CostGuardConfig{
			Enabled: true,
		},
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name: "prov1",
				Models: []config.OpenAICompatibilityModel{
					{
						Name:  "paid-model",
						Alias: "paid-model",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus:          config.PriceStatusPaidConfirmed,
							BillingUnit:          config.BillingUnitTokens,
							PromptPer1M:          2.0,
							PromptConfigured:     true,
							CompletionPer1M:      5.0,
							CompletionConfigured: true,
						},
					},
				},
			},
		},
	}

	// Missing tokens measurement on paid model -> deny!
	dec := EvaluatePreflight(GateRequest{
		Config:      cfg,
		Provider:    "prov1",
		Model:       "paid-model",
		Measurement: Measurement{}, // empty
	})
	if dec.Allowed {
		t.Fatalf("expected missing quantity on paid model to be denied: %+v", dec)
	}
	if dec.Reason != ReasonMissingQuantity {
		t.Errorf("expected reason %s, got %s", ReasonMissingQuantity, dec.Reason)
	}
}

func TestGateVideoSubmitAndStatus(t *testing.T) {
	cfg := &config.Config{
		CostGuard: config.CostGuardConfig{
			Enabled: true,
		},
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name: "video-prov",
				Models: []config.OpenAICompatibilityModel{
					{
						Name:       "vid-gen",
						Alias:      "vid-gen",
						Capability: "video",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus:         config.PriceStatusPaidConfirmed,
							BillingUnit:         config.BillingUnitVideoTasks,
							UnitPrice:           0.50,
							UnitPriceConfigured: true,
							MaxRequestCost:      1.00,
						},
					},
				},
			},
		},
	}

	// 1. Submit: 1 task at $0.50 <= $1.00 -> allow!
	decSubmit := EvaluatePreflight(GateRequest{
		Config:      cfg,
		Provider:    "video-prov",
		Model:       "vid-gen",
		Capability:  "video",
		Measurement: FromVideoSubmit(1),
	})
	if !decSubmit.Allowed {
		t.Fatalf("expected video submit within budget to be allowed: %+v", decSubmit)
	}
	if decSubmit.BillingUnit != config.BillingUnitVideoTasks {
		t.Errorf("expected billing unit %s, got %s", config.BillingUnitVideoTasks, decSubmit.BillingUnit)
	}

	// 2. Status query: always guaranteed 0-cost explicit free
	decStatus := EvaluatePreflight(GateRequest{
		Config:      cfg,
		Provider:    "video-prov",
		Model:       "vid-gen",
		Capability:  "video_status",
		Measurement: FromVideoStatus(),
	})
	if !decStatus.Allowed {
		t.Fatalf("expected video status to be allowed: %+v", decStatus)
	}
	if decStatus.BillingUnit != config.BillingUnitVideoStatus {
		t.Errorf("expected billing unit %s, got %s", config.BillingUnitVideoStatus, decStatus.BillingUnit)
	}
	if decStatus.EstimatedCost.Amount != 0 || !decStatus.EstimatedCost.ExplicitlyFree {
		t.Errorf("expected explicitly free 0 cost for video status query: %+v", decStatus.EstimatedCost)
	}
}

func TestGateAuditSinkDoesNotContainSecrets(t *testing.T) {
	sink := NewMemoryCostAuditSink(10)
	SetGlobalAuditSink(sink)

	cfg := &config.Config{
		CostGuard: config.CostGuardConfig{
			Enabled: true,
		},
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name: "safe-provider",
				Models: []config.OpenAICompatibilityModel{
					{
						Name:  "real-secret-upstream-model",
						Alias: "public-model",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus: config.PriceStatusUnknown,
						},
					},
				},
			},
		},
	}

	dec := EvaluatePreflight(GateRequest{
		Config:    cfg,
		Provider:  "safe-provider",
		Model:     "public-model",
		RequestID: "req-12345",
	})
	if dec.Allowed {
		t.Fatal("expected deny")
	}

	last := sink.LastEvent()
	if last == nil {
		t.Fatal("expected recorded audit event")
	}
	if last.Provider != "safe-provider" || last.PublicModel != "public-model" {
		t.Errorf("unexpected audit fields: %+v", last)
	}
	// Upstream model should be hashed, not raw
	if strings.Contains(last.UpstreamModelHash, "secret") {
		t.Errorf("upstream model should be hashed: %s", last.UpstreamModelHash)
	}
}

func TestGateBuildErrorResponseStructureAndNoSecrets(t *testing.T) {
	dec := GateDecision{
		Allowed:     false,
		Decision:    DecisionDeny,
		Code:        CodePricePolicyViolation,
		Reason:      ReasonUnknownPrice,
		Provider:    "test-prov",
		Model:       "test-model",
		BillingUnit: config.BillingUnitTokens,
		EstimatedCost: CostEstimate{
			AmountKnown: false,
		},
	}

	body, status := BuildGateErrorResponse(dec)
	if status != http.StatusPaymentRequired {
		t.Errorf("expected HTTP 402, got %d", status)
	}

	var parsed struct {
		Error struct {
			Code        string `json:"code"`
			Message     string `json:"message"`
			Reason      string `json:"reason"`
			Provider    string `json:"provider"`
			Model       string `json:"model"`
			BillingUnit string `json:"billing_unit"`
			AmountKnown bool   `json:"amount_known"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal error response failed: %v", err)
	}

	if parsed.Error.Code != CodePricePolicyViolation {
		t.Errorf("expected code %s, got %s", CodePricePolicyViolation, parsed.Error.Code)
	}
	if parsed.Error.Reason != ReasonUnknownPrice {
		t.Errorf("expected reason %s, got %s", ReasonUnknownPrice, parsed.Error.Reason)
	}
	if parsed.Error.AmountKnown {
		t.Errorf("expected amount_known to be false")
	}

	// Verify no credential keywords
	bodyStr := string(body)
	for _, secretWord := range []string{"api_key", "bearer", "password", "proxy_url"} {
		if strings.Contains(strings.ToLower(bodyStr), secretWord) {
			t.Errorf("error response leaked credential keyword %s: %s", secretWord, bodyStr)
		}
	}
}

func TestGateDoesNotModifyPayload(t *testing.T) {
	origPayload := []byte(`{"model":"my-alias","input":"hello"}`)
	payloadCopy := make([]byte, len(origPayload))
	copy(payloadCopy, origPayload)

	cfg := &config.Config{
		CostGuard: config.CostGuardConfig{
			Enabled: true,
		},
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name: "p1",
				Models: []config.OpenAICompatibilityModel{
					{
						Name:  "m1",
						Alias: "my-alias",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus: config.PriceStatusFreeConfirmed,
							BillingUnit: config.BillingUnitCharacters,
						},
					},
				},
			},
		},
	}

	_ = EvaluatePreflight(GateRequest{
		Config:      cfg,
		Provider:    "p1",
		Model:       "my-alias",
		Capability:  "tts",
		Measurement: FromTextCharacters("hello"),
		RawPayload:  payloadCopy,
	})

	if string(payloadCopy) != string(origPayload) {
		t.Fatalf("payload was modified: got %s, want %s", string(payloadCopy), string(origPayload))
	}
}
