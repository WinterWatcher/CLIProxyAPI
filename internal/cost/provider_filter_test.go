package cost

import (
	"context"
	"fmt"
	"sync"
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
	if ok, _ := guard.ValidateUpstreamModelInPool("pool-prov", "m-paid-cheap", "m-paid-cheap"); !ok {
		t.Error("alias pool: paid-cheap -> paid-cheap must be ALLOWED")
	}
}

func TestCandidateFilteringNoGhostReservation(t *testing.T) {
	// A1: 10个候选Provider过滤后只有1个Reservation，被过滤Provider没有预算增加，并发请求不残留
	dir := t.TempDir()
	store, err := NewPersistentBudgetStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	compatList := make([]config.OpenAICompatibility, 10)
	candidateNames := make([]string, 10)
	for i := 0; i < 10; i++ {
		name := fmt.Sprintf("prov-%d", i)
		candidateNames[i] = name
		compatList[i] = config.OpenAICompatibility{
			Name: name,
			CostPolicy: &config.ProviderCostPolicy{
				ProviderBudget: config.ProviderBudgetPolicy{
					DailyUSD:   100.0,
					MonthlyUSD: 1000.0,
				},
			},
			Models: []config.OpenAICompatibilityModel{
				{
					Name:  "chat-model",
					Alias: "chat-model",
					CostPolicy: &config.ModelCostPolicy{
						PriceStatus:          config.PriceStatusPaidConfirmed,
						BillingUnit:          config.BillingUnitTokens,
						PromptPer1M:          float64(i + 1), // prov-0 cheapest, prov-9 most expensive
						PromptConfigured:     true,
						CompletionPer1M:      float64((i + 1) * 2),
						CompletionConfigured: true,
					},
				},
			},
		}
	}

	cfg := &config.Config{
		CostGuard: config.CostGuardConfig{
			Enabled: true,
		},
		OpenAICompatibility: compatList,
	}

	ctx := context.Background()

	// 1. Run FilterCandidateProviders across all 10 candidates
	filterRes := FilterCandidateProviders(ProviderFilterRequest{
		Config:      cfg,
		Providers:   candidateNames,
		Model:       "chat-model",
		Capability:  "chat",
		Measurement: FromTokens(1000, 1000, 0, 0, 0),
		RequestID:   "req-filter-test",
		BudgetStore: store,
	})

	// 2. VERIFY: Zero reservations were created during filtering!
	for i := 0; i < 10; i++ {
		daily, _ := store.CurrentProviderDaily(ctx, candidateNames[i])
		if daily != 0 {
			t.Fatalf("provider %s had ghost reservation during filtering: %d", candidateNames[i], daily)
		}
	}
	globalDaily, _ := store.CurrentGlobalDaily(ctx)
	if globalDaily != 0 {
		t.Fatalf("global daily had ghost reservation during filtering: %d", globalDaily)
	}

	// 3. Now reserve for the final chosen provider ONLY
	chosenProvider := filterRes.AllowedProviders[0]
	res, errReserve := ReserveFinalProvider(ctx, store, cfg, chosenProvider, "chat-model", "chat", filterRes.PrimaryDecision.EstimatedCost, "req-filter-test")
	if errReserve != nil {
		t.Fatalf("ReserveFinalProvider failed: %v", errReserve)
	}
	if res.ID == "" {
		t.Fatal("expected non-empty reservation ID")
	}

	// 4. Verify only chosen provider has spend > 0, other 9 providers remain 0
	chosenDaily, _ := store.CurrentProviderDaily(ctx, chosenProvider)
	if chosenDaily <= 0 {
		t.Fatalf("expected chosen provider spend > 0, got %d", chosenDaily)
	}
	for i := 1; i < 10; i++ {
		daily, _ := store.CurrentProviderDaily(ctx, candidateNames[i])
		if daily != 0 {
			t.Fatalf("unselected candidate %s has unexpected spend: %d", candidateNames[i], daily)
		}
	}

	// 5. Concurrent filtering: 20 goroutines filtering concurrently must leave 0 ghost reservations
	var wg sync.WaitGroup
	for g := 0; g < 20; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			_ = FilterCandidateProviders(ProviderFilterRequest{
				Config:      cfg,
				Providers:   candidateNames,
				Model:       "chat-model",
				Capability:  "chat",
				Measurement: FromTokens(1000, 1000, 0, 0, 0),
				RequestID:   fmt.Sprintf("req-concurrent-%d", gid),
				BudgetStore: store,
			})
		}(g)
	}
	wg.Wait()

	// Verify other 9 providers still remain 0!
	for i := 1; i < 10; i++ {
		daily, _ := store.CurrentProviderDaily(ctx, candidateNames[i])
		if daily != 0 {
			t.Fatalf("unselected candidate %s has ghost reservations after concurrent filtering: %d", candidateNames[i], daily)
		}
	}
}
