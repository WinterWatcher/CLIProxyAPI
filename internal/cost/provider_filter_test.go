package cost

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestProviderFilterWorkflow(t *testing.T) {
	cfg := &config.Config{
		CostGuard: config.CostGuardConfig{
			Enabled:            true,
			UnknownPriceAction: config.CostActionDeny,
		},
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name: "free-prov-1",
				Models: []config.OpenAICompatibilityModel{
					{
						Name:  "chat-model",
						Alias: "chat-model",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus: config.PriceStatusFreeConfirmed,
							BillingUnit: config.BillingUnitTokens,
						},
					},
				},
			},
			{
				Name: "free-prov-2",
				Models: []config.OpenAICompatibilityModel{
					{
						Name:  "chat-model",
						Alias: "chat-model",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus: config.PriceStatusFreeConfirmed,
							BillingUnit: config.BillingUnitTokens,
						},
					},
				},
			},
			{
				Name: "paid-cheap-prov",
				Models: []config.OpenAICompatibilityModel{
					{
						Name:  "chat-model",
						Alias: "chat-model",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus:          config.PriceStatusPaidConfirmed,
							BillingUnit:          config.BillingUnitTokens,
							PromptPer1M:          1.0,
							PromptConfigured:     true,
							CompletionPer1M:      2.0,
							CompletionConfigured: true,
						},
					},
				},
			},
			{
				Name: "paid-expensive-prov",
				Models: []config.OpenAICompatibilityModel{
					{
						Name:  "chat-model",
						Alias: "chat-model",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus:          config.PriceStatusPaidConfirmed,
							BillingUnit:          config.BillingUnitTokens,
							PromptPer1M:          10.0,
							PromptConfigured:     true,
							CompletionPer1M:      20.0,
							CompletionConfigured: true,
						},
					},
				},
			},
			{
				Name: "unknown-prov",
				Models: []config.OpenAICompatibilityModel{
					{
						Name:  "chat-model",
						Alias: "chat-model",
					},
				},
			},
			{
				Name: "frozen-prov",
				Models: []config.OpenAICompatibilityModel{
					{
						Name:  "chat-model",
						Alias: "chat-model",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus: config.PriceStatusFrozen,
						},
					},
				},
			},
		},
	}

	// Case 1: Primary is Free (free-prov-1).
	// Candidates: [free-prov-1, free-prov-2, paid-cheap-prov, unknown-prov, frozen-prov]
	// Expected: [free-prov-1, free-prov-2] allowed; paid, unknown, frozen blocked!
	res1 := FilterCandidateProviders(ProviderFilterRequest{
		Config:      cfg,
		Providers:   []string{"free-prov-1", "free-prov-2", "paid-cheap-prov", "unknown-prov", "frozen-prov"},
		Model:       "chat-model",
		Capability:  "chat",
		Measurement: FromTokens(100, 100, 0, 0, 0),
	})

	if len(res1.AllowedProviders) != 2 {
		t.Fatalf("expected 2 allowed providers for free primary, got %d: %v", len(res1.AllowedProviders), res1.AllowedProviders)
	}
	if res1.AllowedProviders[0] != "free-prov-1" || res1.AllowedProviders[1] != "free-prov-2" {
		t.Errorf("unexpected allowed providers: %v", res1.AllowedProviders)
	}

	// Case 2: Primary is Paid (paid-cheap-prov).
	// Candidates: [paid-cheap-prov, free-prov-1, paid-expensive-prov, unknown-prov]
	// Expected: [paid-cheap-prov, free-prov-1] allowed; expensive blocked; unknown blocked!
	res2 := FilterCandidateProviders(ProviderFilterRequest{
		Config:      cfg,
		Providers:   []string{"paid-cheap-prov", "free-prov-1", "paid-expensive-prov", "unknown-prov"},
		Model:       "chat-model",
		Capability:  "chat",
		Measurement: FromTokens(1000, 1000, 0, 0, 0),
	})

	if len(res2.AllowedProviders) != 2 {
		t.Fatalf("expected 2 allowed providers (paid-cheap, free), got %d: %v", len(res2.AllowedProviders), res2.AllowedProviders)
	}
	if res2.AllowedProviders[0] != "paid-cheap-prov" || res2.AllowedProviders[1] != "free-prov-1" {
		t.Errorf("unexpected allowed providers: %v", res2.AllowedProviders)
	}

	// Case 3: All candidates are denied (unknown, frozen).
	// Expected: AllowedProviders is empty!
	res3 := FilterCandidateProviders(ProviderFilterRequest{
		Config:      cfg,
		Providers:   []string{"unknown-prov", "frozen-prov"},
		Model:       "chat-model",
		Capability:  "chat",
		Measurement: FromTokens(100, 100, 0, 0, 0),
	})

	if len(res3.AllowedProviders) != 0 {
		t.Fatalf("expected 0 allowed providers, got %d: %v", len(res3.AllowedProviders), res3.AllowedProviders)
	}

	// Case 4: CostGuard disabled: all candidates allowed!
	cfgDisabled := &config.Config{
		CostGuard: config.CostGuardConfig{Enabled: false},
	}
	res4 := FilterCandidateProviders(ProviderFilterRequest{
		Config:    cfgDisabled,
		Providers: []string{"prov-a", "prov-b", "prov-c"},
		Model:     "any",
	})
	if len(res4.AllowedProviders) != 3 {
		t.Fatalf("expected all 3 providers allowed when cost-guard disabled, got %d", len(res4.AllowedProviders))
	}
}

func TestFallbackGuardRuntimeCheck(t *testing.T) {
	cfg := &config.Config{
		CostGuard: config.CostGuardConfig{
			Enabled: true,
		},
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name: "p-free",
				Models: []config.OpenAICompatibilityModel{
					{
						Name:  "chat",
						Alias: "chat",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus: config.PriceStatusFreeConfirmed,
							BillingUnit: config.BillingUnitTokens,
						},
					},
				},
			},
			{
				Name: "p-paid",
				Models: []config.OpenAICompatibilityModel{
					{
						Name:  "chat",
						Alias: "chat",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus:          config.PriceStatusPaidConfirmed,
							BillingUnit:          config.BillingUnitTokens,
							PromptPer1M:          2.0,
							PromptConfigured:     true,
							CompletionPer1M:      4.0,
							CompletionConfigured: true,
						},
					},
				},
			},
		},
	}

	freeEst := CostEstimate{
		BillingUnit:    config.BillingUnitTokens,
		PriceStatus:    config.PriceStatusFreeConfirmed,
		ExplicitlyFree: true,
		Known:          true,
	}

	guard := NewFallbackGuard(cfg, "p-free", "chat", "chat", freeEst, cfg.OpenAICompatibility[0].Models[0].CostPolicy, []string{"p-free"}, FromTokens(100, 100, 0, 0, 0), "req-1")

	// 1. Candidate not in allowed list -> rejected
	if ok, _ := guard.ValidateCandidateAuth("p-paid", "chat"); ok {
		t.Error("p-paid should be rejected since it was not in allowed list")
	}

	// 2. Candidate in allowed list -> allowed
	if ok, _ := guard.ValidateCandidateAuth("p-free", "chat"); !ok {
		t.Error("p-free should be allowed")
	}
}

func TestAliasPoolModelFallback(t *testing.T) {
	cfg := &config.Config{
		CostGuard: config.CostGuardConfig{
			Enabled: true,
		},
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name: "pool-prov",
				Models: []config.OpenAICompatibilityModel{
					{
						Name:  "m-free",
						Alias: "my-alias",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus: config.PriceStatusFreeConfirmed,
							BillingUnit: config.BillingUnitTokens,
						},
					},
					{
						Name:  "m-paid-cheap",
						Alias: "my-alias",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus:          config.PriceStatusPaidConfirmed,
							BillingUnit:          config.BillingUnitTokens,
							PromptPer1M:          1.0,
							PromptConfigured:     true,
							CompletionPer1M:      2.0,
							CompletionConfigured: true,
						},
					},
					{
						Name:  "m-paid-expensive",
						Alias: "my-alias",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus:          config.PriceStatusPaidConfirmed,
							BillingUnit:          config.BillingUnitTokens,
							PromptPer1M:          5.0,
							PromptConfigured:     true,
							CompletionPer1M:      10.0,
							CompletionConfigured: true,
						},
					},
				},
			},
		},
	}

	freeEst := CostEstimate{
		PriceStatus:    config.PriceStatusFreeConfirmed,
		BillingUnit:    config.BillingUnitTokens,
		ExplicitlyFree: true,
		Known:          true,
	}
	guard := NewFallbackGuard(cfg, "pool-prov", "my-alias", "chat", freeEst, cfg.OpenAICompatibility[0].Models[0].CostPolicy, []string{"pool-prov"}, FromTokens(100, 100, 0, 0, 0), "req-pool")

	// 1. Alias pool: free -> paid is strictly BLOCKED
	if ok, _ := guard.ValidateUpstreamModelInPool("pool-prov", "m-free", "m-paid-cheap"); ok {
		t.Error("alias pool: free -> paid must be BLOCKED")
	}

	// 2. Alias pool: paid-cheap -> paid-expensive is BLOCKED
	if ok, _ := guard.ValidateUpstreamModelInPool("pool-prov", "m-paid-cheap", "m-paid-expensive"); ok {
		t.Error("alias pool: paid-cheap -> paid-expensive must be BLOCKED")
	}

	// 3. Alias pool: paid-expensive -> paid-cheap is ALLOWED
	if ok, _ := guard.ValidateUpstreamModelInPool("pool-prov", "m-paid-expensive", "m-paid-cheap"); !ok {
		t.Error("alias pool: paid-expensive -> paid-cheap must be ALLOWED")
	}
}
