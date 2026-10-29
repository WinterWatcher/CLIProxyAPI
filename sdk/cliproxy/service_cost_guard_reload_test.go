package cliproxy

import (
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestServiceReloadCostGuardPolicyPreserved(t *testing.T) {
	initialCfg := &config.Config{
		CostGuard: internalconfig.CostGuardConfig{
			Enabled:            true,
			Currency:           "USD",
			UnknownPriceAction: internalconfig.CostActionDeny,
			Budget: internalconfig.CostBudgetPolicy{
				DailyUSD: 10.0,
			},
		},
		OpenAICompatibility: []internalconfig.OpenAICompatibility{
			{
				Name:    "provider-a",
				BaseURL: "https://api.a.com/v1",
				CostPolicy: &internalconfig.ProviderCostPolicy{
					Mode: "locked",
				},
				Models: []internalconfig.OpenAICompatibilityModel{
					{
						Name:  "m1",
						Alias: "m1",
						CostPolicy: &internalconfig.ModelCostPolicy{
							PriceStatus:     internalconfig.PriceStatusPaidConfirmed,
							BillingUnit:     internalconfig.BillingUnitTokens,
							PromptPer1M:     1.5,
							CompletionPer1M: 3.0,
						},
					},
				},
			},
		},
	}

	service := &Service{}
	commit1 := service.commitConfigUpdate(initialCfg)
	if commit1.cfg == nil {
		t.Fatal("expected commitConfigUpdate to succeed for valid initial cost policy")
	}
	current := service.currentConfig()
	if !current.CostGuard.Enabled || current.CostGuard.Budget.DailyUSD != 10.0 {
		t.Fatalf("expected cost guard enabled with daily-usd 10, got %+v", current.CostGuard)
	}
	if current.OpenAICompatibility[0].CostPolicy.Mode != "locked" {
		t.Fatalf("expected provider cost policy locked, got %s", current.OpenAICompatibility[0].CostPolicy.Mode)
	}
	if current.OpenAICompatibility[0].Models[0].CostPolicy.PromptPer1M != 1.5 {
		t.Fatalf("expected model prompt price 1.5, got %v", current.OpenAICompatibility[0].Models[0].CostPolicy.PromptPer1M)
	}

	// Update with new valid cost policy
	updatedCfg := initialCfg.CloneForRuntime()
	updatedCfg.CostGuard.Budget.DailyUSD = 25.0
	updatedCfg.OpenAICompatibility[0].Models[0].CostPolicy.PromptPer1M = 2.0

	commit2 := service.commitConfigUpdate(updatedCfg)
	if commit2.cfg == nil {
		t.Fatal("expected commitConfigUpdate to succeed for valid updated cost policy")
	}
	current = service.currentConfig()
	if current.CostGuard.Budget.DailyUSD != 25.0 {
		t.Fatalf("expected updated daily-usd 25, got %v", current.CostGuard.Budget.DailyUSD)
	}
	if current.OpenAICompatibility[0].Models[0].CostPolicy.PromptPer1M != 2.0 {
		t.Fatalf("expected updated prompt price 2.0, got %v", current.OpenAICompatibility[0].Models[0].CostPolicy.PromptPer1M)
	}
}

func TestServiceReloadInvalidCostGuardPolicyRejection(t *testing.T) {
	initialCfg := &config.Config{
		CostGuard: internalconfig.CostGuardConfig{
			Enabled:            true,
			Currency:           "USD",
			UnknownPriceAction: internalconfig.CostActionDeny,
			Budget: internalconfig.CostBudgetPolicy{
				DailyUSD: 10.0,
			},
		},
		OpenAICompatibility: []internalconfig.OpenAICompatibility{
			{
				Name:    "provider-a",
				BaseURL: "https://api.a.com/v1",
				Models: []internalconfig.OpenAICompatibilityModel{
					{
						Name:  "m1",
						Alias: "m1",
						CostPolicy: &internalconfig.ModelCostPolicy{
							PriceStatus:     internalconfig.PriceStatusPaidConfirmed,
							BillingUnit:     internalconfig.BillingUnitTokens,
							PromptPer1M:     1.5,
							CompletionPer1M: 3.0,
						},
					},
				},
			},
		},
	}

	service := &Service{}
	commit1 := service.commitConfigUpdate(initialCfg)
	if commit1.cfg == nil {
		t.Fatal("expected commitConfigUpdate to succeed for valid initial config")
	}

	// Invalid update: free_confirmed model with non-zero price
	invalidCfg := initialCfg.CloneForRuntime()
	invalidCfg.OpenAICompatibility[0].Models[0].CostPolicy.PriceStatus = internalconfig.PriceStatusFreeConfirmed
	invalidCfg.OpenAICompatibility[0].Models[0].CostPolicy.PromptPer1M = 99.0

	commitInvalid := service.commitConfigUpdate(invalidCfg)
	if commitInvalid.cfg != nil {
		t.Fatal("expected commitConfigUpdate to reject invalid cost guard policy")
	}

	// Verify service.cfg is untouched and preserves previous valid config
	current := service.currentConfig()
	if current.OpenAICompatibility[0].Models[0].CostPolicy.PriceStatus != internalconfig.PriceStatusPaidConfirmed {
		t.Fatalf("expected price status to remain paid_confirmed, got %s", current.OpenAICompatibility[0].Models[0].CostPolicy.PriceStatus)
	}
	if current.OpenAICompatibility[0].Models[0].CostPolicy.PromptPer1M != 1.5 {
		t.Fatalf("expected prompt price to remain 1.5, got %v", current.OpenAICompatibility[0].Models[0].CostPolicy.PromptPer1M)
	}
}
