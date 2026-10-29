package config

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCostGuardConfigDefaults(t *testing.T) {
	def := DefaultCostGuardConfig()
	if def.Enabled {
		t.Errorf("expected Enabled=false, got %v", def.Enabled)
	}
	if def.Currency != CurrencyUSD {
		t.Errorf("expected Currency=%s, got %s", CurrencyUSD, def.Currency)
	}
	if def.UnknownPriceAction != CostActionDeny {
		t.Errorf("expected UnknownPriceAction=deny, got %s", def.UnknownPriceAction)
	}
	if def.PriceChangeAction != CostActionDeny {
		t.Errorf("expected PriceChangeAction=deny, got %s", def.PriceChangeAction)
	}
	if def.FreeToPaidAction != CostActionDeny {
		t.Errorf("expected FreeToPaidAction=deny, got %s", def.FreeToPaidAction)
	}
	if !def.Fallback.Allow || !def.Fallback.RequirePriceCompatible || def.Fallback.AllowUnknownPrice {
		t.Errorf("unexpected fallback policy defaults: %+v", def.Fallback)
	}
	if !def.Budget.EnforcePerRequest || !def.Budget.EnforcePerProvider || !def.Budget.EnforceGlobal {
		t.Errorf("unexpected budget enforcement defaults: %+v", def.Budget)
	}
	if def.Budget.DailyUSD != 0 || def.Budget.MonthlyUSD != 0 {
		t.Errorf("unexpected budget defaults: %+v", def.Budget)
	}
	if !def.Audit.RecordPriceAtRequest || !def.Audit.RecordPolicyDecision {
		t.Errorf("unexpected audit policy defaults: %+v", def.Audit)
	}

	// ApplyDefaults on empty struct
	var empty CostGuardConfig
	empty.ApplyDefaults()
	if empty.Currency != CurrencyUSD || empty.UnknownPriceAction != CostActionDeny ||
		empty.PriceChangeAction != CostActionDeny || empty.FreeToPaidAction != CostActionDeny {
		t.Errorf("ApplyDefaults did not set safe defaults: %+v", empty)
	}
}

func TestCostGuardConfigYAMLDecode(t *testing.T) {
	raw := `
cost-guard:
  enabled: true
  currency: USD
  unknown-price-action: deny
  price-change-action: warn
  free-to-paid-action: deny
  fallback:
    allow: true
    require-price-compatible: true
    allow-unknown-price: false
  budget:
    enforce-per-request: true
    enforce-per-provider: true
    enforce-global: true
    daily-usd: 25.50
    monthly-usd: 500.00
  audit:
    record-price-at-request: true
    record-policy-decision: true

openai-compatibility:
  - name: test-provider
    base-url: https://api.example.com/v1
    cost-policy:
      mode: locked
      source: operator
      effective-at: "2026-10-29T00:00:00Z"
      unknown-price-action: deny
      price-change-action: deny
      free-to-paid-action: deny
      allow-fallback: false
      provider-budget:
        daily-usd: 10.00
        monthly-usd: 100.00
        on-exceeded: freeze
    models:
      - name: test-upstream
        alias: test-alias
        cost-policy:
          price-status: paid_confirmed
          billing-unit: tokens
          prompt-per-1m: 0.50
          completion-per-1m: 1.50
          cache-per-1m: 0.25
          cache-read-per-1m: 0.10
          cache-creation-per-1m: 0.50
          unit-price: 0
          max-request-cost: 0.05
          max-daily-cost: 5.00
          on-nonzero-price: allow
`
	cfg, err := ParseConfigBytes([]byte(raw))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}

	if !cfg.CostGuard.Enabled {
		t.Fatal("expected cost-guard.enabled to be true")
	}
	if cfg.CostGuard.Currency != "USD" {
		t.Errorf("expected currency USD, got %s", cfg.CostGuard.Currency)
	}
	if cfg.CostGuard.PriceChangeAction != "warn" {
		t.Errorf("expected price-change-action warn, got %s", cfg.CostGuard.PriceChangeAction)
	}
	if cfg.CostGuard.Budget.DailyUSD != 25.50 || cfg.CostGuard.Budget.MonthlyUSD != 500.00 {
		t.Errorf("unexpected budget values: %+v", cfg.CostGuard.Budget)
	}

	if len(cfg.OpenAICompatibility) != 1 {
		t.Fatalf("expected 1 provider, got %d", len(cfg.OpenAICompatibility))
	}
	p := cfg.OpenAICompatibility[0]
	if p.CostPolicy == nil {
		t.Fatal("expected provider cost-policy to be parsed")
	}
	if p.CostPolicy.Mode != "locked" || p.CostPolicy.Source != "operator" {
		t.Errorf("unexpected provider cost-policy: %+v", p.CostPolicy)
	}
	if p.CostPolicy.EffectiveAt != "2026-10-29T00:00:00Z" {
		t.Errorf("unexpected effective-at: %s", p.CostPolicy.EffectiveAt)
	}
	if p.CostPolicy.ProviderBudget.OnExceeded != "freeze" {
		t.Errorf("unexpected on-exceeded: %s", p.CostPolicy.ProviderBudget.OnExceeded)
	}

	if len(p.Models) != 1 || p.Models[0].CostPolicy == nil {
		t.Fatalf("expected model cost-policy to be parsed")
	}
	m := p.Models[0].CostPolicy
	if m.PriceStatus != PriceStatusPaidConfirmed {
		t.Errorf("expected price-status %s, got %s", PriceStatusPaidConfirmed, m.PriceStatus)
	}
	if m.BillingUnit != BillingUnitTokens {
		t.Errorf("expected billing-unit %s, got %s", BillingUnitTokens, m.BillingUnit)
	}
	if m.PromptPer1M != 0.50 || m.CompletionPer1M != 1.50 || m.MaxRequestCost != 0.05 {
		t.Errorf("unexpected model pricing values: %+v", m)
	}
}

func TestCostGuardConfigYAMLEncode(t *testing.T) {
	cfg := Config{
		CostGuard: CostGuardConfig{
			Enabled:            true,
			Currency:           "USD",
			UnknownPriceAction: "deny",
			PriceChangeAction:  "deny",
			FreeToPaidAction:   "deny",
			Budget: CostBudgetPolicy{
				DailyUSD:   10.5,
				MonthlyUSD: 100,
			},
		},
		OpenAICompatibility: []OpenAICompatibility{
			{
				Name:    "demo-provider",
				BaseURL: "https://api.example.com",
				CostPolicy: &ProviderCostPolicy{
					Mode:               "locked",
					UnknownPriceAction: "deny",
				},
				Models: []OpenAICompatibilityModel{
					{
						Name:  "m1",
						Alias: "m1",
						CostPolicy: &ModelCostPolicy{
							PriceStatus:    PriceStatusFreeConfirmed,
							BillingUnit:    BillingUnitTokens,
							MaxRequestCost: 0,
						},
					},
				},
			},
		},
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("yaml.Marshal() error = %v", err)
	}
	yamlStr := string(data)
	if !strings.Contains(yamlStr, "cost-guard:") {
		t.Errorf("encoded YAML missing cost-guard: %s", yamlStr)
	}
	if !strings.Contains(yamlStr, "cost-policy:") {
		t.Errorf("encoded YAML missing cost-policy: %s", yamlStr)
	}
	if !strings.Contains(yamlStr, "price-status: free_confirmed") {
		t.Errorf("encoded YAML missing price-status: %s", yamlStr)
	}
}

func TestLegacyConfigStillLoads(t *testing.T) {
	legacyYAML := `
port: 8317
openai-compatibility:
  - name: existing-provider
    base-url: https://example.com/v1
    api-key-entries:
      - api-key: sk-dummy-key
    models:
      - name: existing-model
        alias: existing-model
`
	cfg, err := ParseConfigBytes([]byte(legacyYAML))
	if err != nil {
		t.Fatalf("ParseConfigBytes() legacy config error = %v", err)
	}
	if cfg.CostGuard.Enabled {
		t.Error("expected cost-guard.enabled to be false for legacy config")
	}
	if len(cfg.OpenAICompatibility) != 1 {
		t.Fatalf("expected 1 provider, got %d", len(cfg.OpenAICompatibility))
	}
	p := cfg.OpenAICompatibility[0]
	if p.CostPolicy != nil {
		t.Errorf("expected legacy provider cost policy to be nil, got %+v", p.CostPolicy)
	}
	if len(p.Models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(p.Models))
	}
	if p.Models[0].CostPolicy != nil {
		t.Errorf("expected legacy model cost policy to be nil, got %+v", p.Models[0].CostPolicy)
	}
}

func TestUnknownPriceIsNotFree(t *testing.T) {
	modelWithoutPolicy := OpenAICompatibilityModel{
		Name:  "unknown-model",
		Alias: "unknown-model",
	}
	effective := modelWithoutPolicy.EffectiveCostPolicy()
	if effective.PriceStatus != PriceStatusUnknown {
		t.Fatalf("expected PriceStatus=%s, got %s", PriceStatusUnknown, effective.PriceStatus)
	}
	if effective.PriceStatus == PriceStatusFreeConfirmed {
		t.Fatal("unconfigured model must NEVER be assumed to be free_confirmed")
	}
}

func TestFreeConfirmedRequiresZeroPrices(t *testing.T) {
	// Valid free_confirmed
	validFree := &ModelCostPolicy{
		PriceStatus:     PriceStatusFreeConfirmed,
		BillingUnit:     BillingUnitTokens,
		PromptPer1M:     0,
		CompletionPer1M: 0,
		CachePer1M:      0,
		UnitPrice:       0,
		MaxRequestCost:  0,
	}
	if err := ValidateModelCostPolicy(validFree); err != nil {
		t.Fatalf("expected valid free_confirmed to pass, got: %v", err)
	}

	// Invalid free_confirmed with non-zero prompt price
	invalidPrompt := &ModelCostPolicy{
		PriceStatus:     PriceStatusFreeConfirmed,
		BillingUnit:     BillingUnitTokens,
		PromptPer1M:     0.01,
		CompletionPer1M: 0,
	}
	if err := ValidateModelCostPolicy(invalidPrompt); err == nil {
		t.Fatal("expected free_confirmed with non-zero prompt price to fail validation")
	}

	// Invalid free_confirmed with non-zero unit price
	invalidUnit := &ModelCostPolicy{
		PriceStatus: PriceStatusFreeConfirmed,
		BillingUnit: BillingUnitImages,
		UnitPrice:   0.02,
	}
	if err := ValidateModelCostPolicy(invalidUnit); err == nil {
		t.Fatal("expected free_confirmed with non-zero unit price to fail validation")
	}
}

func TestPaidConfirmedAllowsNonZeroPrices(t *testing.T) {
	paid := &ModelCostPolicy{
		PriceStatus:     PriceStatusPaidConfirmed,
		BillingUnit:     BillingUnitTokens,
		PromptPer1M:     1.50,
		CompletionPer1M: 2.50,
		CachePer1M:      0.75,
		UnitPrice:       0,
		MaxRequestCost:  10.0,
		OnNonzeroPrice:  CostActionAllow,
	}
	if err := ValidateModelCostPolicy(paid); err != nil {
		t.Fatalf("expected paid_confirmed to allow non-zero prices, got: %v", err)
	}
}

func TestFrozenPolicyValidation(t *testing.T) {
	frozenAllow := &ModelCostPolicy{
		PriceStatus:    PriceStatusFrozen,
		BillingUnit:    BillingUnitTokens,
		OnNonzeroPrice: CostActionAllow,
	}
	if err := ValidateModelCostPolicy(frozenAllow); err == nil {
		t.Fatal("expected frozen model with allow action to fail validation")
	}

	frozenDeny := &ModelCostPolicy{
		PriceStatus:    PriceStatusFrozen,
		BillingUnit:    BillingUnitTokens,
		OnNonzeroPrice: CostActionDeny,
	}
	if err := ValidateModelCostPolicy(frozenDeny); err != nil {
		t.Fatalf("expected frozen model with deny action to pass, got: %v", err)
	}

	frozenFreeze := &ModelCostPolicy{
		PriceStatus:    PriceStatusFrozen,
		BillingUnit:    BillingUnitTokens,
		OnNonzeroPrice: CostActionFreeze,
	}
	if err := ValidateModelCostPolicy(frozenFreeze); err != nil {
		t.Fatalf("expected frozen model with freeze action to pass, got: %v", err)
	}
}

func TestUnsupportedActionRejected(t *testing.T) {
	for _, action := range []string{"allow", "deny", "warn", "freeze"} {
		cfg := CostGuardConfig{UnknownPriceAction: action}
		if err := ValidateCostGuardConfig(cfg); err != nil {
			t.Errorf("action %q should be valid, got: %v", action, err)
		}
	}

	for _, invalid := range []string{"ignore", "pass", "block", "unknown", "yes"} {
		cfg := CostGuardConfig{UnknownPriceAction: invalid}
		if err := ValidateCostGuardConfig(cfg); err == nil {
			t.Errorf("action %q should be rejected", invalid)
		}
	}
}

func TestUnsupportedBillingUnitRejected(t *testing.T) {
	for _, unit := range []string{"tokens", "audio_seconds", "characters", "images", "video_tasks", "video_status"} {
		policy := &ModelCostPolicy{BillingUnit: unit, PriceStatus: PriceStatusPaidConfirmed}
		if err := ValidateModelCostPolicy(policy); err != nil {
			t.Errorf("billing unit %q should be valid, got: %v", unit, err)
		}
	}

	for _, invalid := range []string{"requests", "seconds", "hours", "calls", "megabytes"} {
		policy := &ModelCostPolicy{BillingUnit: invalid, PriceStatus: PriceStatusPaidConfirmed}
		if err := ValidateModelCostPolicy(policy); err == nil {
			t.Errorf("billing unit %q should be rejected", invalid)
		}
	}
}

func TestNegativePriceRejected(t *testing.T) {
	tests := []struct {
		name   string
		policy ModelCostPolicy
	}{
		{"negative prompt", ModelCostPolicy{PromptPer1M: -0.1}},
		{"negative completion", ModelCostPolicy{CompletionPer1M: -1.0}},
		{"negative cache", ModelCostPolicy{CachePer1M: -0.5}},
		{"negative cache read", ModelCostPolicy{CacheReadPer1M: -0.2}},
		{"negative cache creation", ModelCostPolicy{CacheCreationPer1M: -0.3}},
		{"negative unit price", ModelCostPolicy{UnitPrice: -0.05}},
		{"negative max request cost", ModelCostPolicy{MaxRequestCost: -0.01}},
		{"negative max daily cost", ModelCostPolicy{MaxDailyCost: -5.0}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.policy
			p.BillingUnit = BillingUnitTokens
			p.PriceStatus = PriceStatusPaidConfirmed
			if err := ValidateModelCostPolicy(&p); err == nil {
				t.Fatalf("expected %s to be rejected", tc.name)
			}
		})
	}
}

func TestNaNAndInfPriceRejected(t *testing.T) {
	nanPolicy := ModelCostPolicy{
		PromptPer1M: math.NaN(),
		BillingUnit: BillingUnitTokens,
		PriceStatus: PriceStatusPaidConfirmed,
	}
	if err := ValidateModelCostPolicy(&nanPolicy); err == nil {
		t.Fatal("expected NaN price to be rejected")
	}

	infPolicy := ModelCostPolicy{
		PromptPer1M: math.Inf(1),
		BillingUnit: BillingUnitTokens,
		PriceStatus: PriceStatusPaidConfirmed,
	}
	if err := ValidateModelCostPolicy(&infPolicy); err == nil {
		t.Fatal("expected +Inf price to be rejected")
	}

	negInfPolicy := ModelCostPolicy{
		UnitPrice:   math.Inf(-1),
		BillingUnit: BillingUnitImages,
		PriceStatus: PriceStatusPaidConfirmed,
	}
	if err := ValidateModelCostPolicy(&negInfPolicy); err == nil {
		t.Fatal("expected -Inf price to be rejected")
	}
}

func TestProviderPolicyClone(t *testing.T) {
	original := &ProviderCostPolicy{
		Mode:               "locked",
		Source:             "operator",
		EffectiveAt:        "2026-10-29T12:00:00Z",
		UnknownPriceAction: CostActionDeny,
		ProviderBudget: ProviderBudgetPolicy{
			DailyUSD:   15.0,
			MonthlyUSD: 150.0,
			OnExceeded: CostActionFreeze,
		},
	}

	cloned := original.Clone()
	if cloned == nil {
		t.Fatal("expected non-nil clone")
	}
	// Verify deep copy mutation isolation
	cloned.Mode = "updated"
	cloned.ProviderBudget.DailyUSD = 999.0
	if original.Mode != "locked" {
		t.Errorf("original modified after clone mutation: mode=%s", original.Mode)
	}
	if original.ProviderBudget.DailyUSD != 15.0 {
		t.Errorf("original modified after clone mutation: budget=%v", original.ProviderBudget.DailyUSD)
	}
}

func TestModelPolicyClone(t *testing.T) {
	original := &ModelCostPolicy{
		PriceStatus:     PriceStatusPaidConfirmed,
		BillingUnit:     BillingUnitTokens,
		PromptPer1M:     2.0,
		CompletionPer1M: 4.0,
		MaxRequestCost:  0.25,
		OnNonzeroPrice:  CostActionAllow,
	}

	cloned := original.Clone()
	if cloned == nil {
		t.Fatal("expected non-nil clone")
	}
	cloned.PromptPer1M = 99.0
	if original.PromptPer1M != 2.0 {
		t.Errorf("original modified after clone mutation: %v", original.PromptPer1M)
	}

	// Test through Config.CloneForRuntime
	cfg := &Config{
		CostGuard: CostGuardConfig{
			Enabled:  true,
			Currency: "USD",
			Budget:   CostBudgetPolicy{DailyUSD: 50.0},
		},
		OpenAICompatibility: []OpenAICompatibility{
			{
				Name:       "p1",
				CostPolicy: &ProviderCostPolicy{Mode: "locked"},
				Models: []OpenAICompatibilityModel{
					{
						Name:       "m1",
						CostPolicy: original,
					},
				},
			},
		},
	}
	runtimeClone := cfg.CloneForRuntime()
	if runtimeClone == nil {
		t.Fatal("CloneForRuntime returned nil")
	}
	runtimeClone.CostGuard.Budget.DailyUSD = 200.0
	runtimeClone.OpenAICompatibility[0].CostPolicy.Mode = "modified"
	runtimeClone.OpenAICompatibility[0].Models[0].CostPolicy.PromptPer1M = 888.0

	if cfg.CostGuard.Budget.DailyUSD != 50.0 {
		t.Errorf("original cfg CostGuard budget modified: %v", cfg.CostGuard.Budget.DailyUSD)
	}
	if cfg.OpenAICompatibility[0].CostPolicy.Mode != "locked" {
		t.Errorf("original provider cost policy modified: %s", cfg.OpenAICompatibility[0].CostPolicy.Mode)
	}
	if cfg.OpenAICompatibility[0].Models[0].CostPolicy.PromptPer1M != 2.0 {
		t.Errorf("original model cost policy modified: %v", cfg.OpenAICompatibility[0].Models[0].CostPolicy.PromptPer1M)
	}
}

func TestHotReloadPreservesCostPolicy(t *testing.T) {
	initialYAML := `
cost-guard:
  enabled: true
  currency: USD
  unknown-price-action: deny
openai-compatibility:
  - name: my-provider
    base-url: https://api.my-provider.com/v1
    cost-policy:
      mode: locked
      source: operator
    models:
      - name: model-a
        alias: model-a
        cost-policy:
          price-status: paid_confirmed
          billing-unit: tokens
          prompt-per-1m: 1.00
`
	cfg1, err := ParseConfigBytes([]byte(initialYAML))
	if err != nil {
		t.Fatalf("first parse error: %v", err)
	}
	if !cfg1.CostGuard.Enabled || cfg1.OpenAICompatibility[0].CostPolicy == nil ||
		cfg1.OpenAICompatibility[0].Models[0].CostPolicy == nil {
		t.Fatal("failed to load initial cost policy")
	}

	updatedYAML := `
cost-guard:
  enabled: true
  currency: USD
  unknown-price-action: warn
openai-compatibility:
  - name: my-provider
    base-url: https://api.my-provider.com/v1
    cost-policy:
      mode: locked
      source: operator
    models:
      - name: model-a
        alias: model-a
        cost-policy:
          price-status: paid_confirmed
          billing-unit: tokens
          prompt-per-1m: 1.25
`
	cfg2, err := ParseConfigBytes([]byte(updatedYAML))
	if err != nil {
		t.Fatalf("second parse error: %v", err)
	}
	if cfg2.CostGuard.UnknownPriceAction != "warn" {
		t.Errorf("expected updated action warn, got %s", cfg2.CostGuard.UnknownPriceAction)
	}
	if cfg2.OpenAICompatibility[0].Models[0].CostPolicy.PromptPer1M != 1.25 {
		t.Errorf("expected updated prompt-per-1m 1.25, got %v", cfg2.OpenAICompatibility[0].Models[0].CostPolicy.PromptPer1M)
	}
}

func TestInvalidHotReloadKeepsPreviousConfig(t *testing.T) {
	validYAML := `
cost-guard:
  enabled: true
  currency: USD
openai-compatibility:
  - name: valid-prov
    base-url: https://api.valid.com/v1
    models:
      - name: m1
        alias: m1
        cost-policy:
          price-status: paid_confirmed
          billing-unit: tokens
          prompt-per-1m: 2.0
`
	validCfg, err := ParseConfigBytes([]byte(validYAML))
	if err != nil {
		t.Fatalf("valid config parse failed: %v", err)
	}

	// Invalid update: free_confirmed with non-zero price
	invalidYAML := `
cost-guard:
  enabled: true
  currency: USD
openai-compatibility:
  - name: valid-prov
    base-url: https://api.valid.com/v1
    models:
      - name: m1
        alias: m1
        cost-policy:
          price-status: free_confirmed
          billing-unit: tokens
          prompt-per-1m: 2.0
`
	_, errInvalid := ParseConfigBytes([]byte(invalidYAML))
	if errInvalid == nil {
		t.Fatal("expected invalid config to be rejected during parse")
	}

	// Verify validCfg remains unchanged
	if validCfg.OpenAICompatibility[0].Models[0].CostPolicy.PriceStatus != PriceStatusPaidConfirmed {
		t.Fatal("valid config state was mutated")
	}
}

func TestManagementOutputRedactsSecretsButKeepsCostPolicy(t *testing.T) {
	prov := OpenAICompatibility{
		Name:    "demo-provider",
		BaseURL: "https://api.example.com",
		APIKeyEntries: []OpenAICompatibilityAPIKey{
			{APIKey: "sk-super-secret-key-12345", ProxyURL: "http://user:pass@proxy.example.com:8080"},
		},
		CostPolicy: &ProviderCostPolicy{
			Mode:               "locked",
			Source:             "operator",
			UnknownPriceAction: CostActionDeny,
			ProviderBudget: ProviderBudgetPolicy{
				DailyUSD: 10.0,
			},
		},
		Models: []OpenAICompatibilityModel{
			{
				Name:  "gpt-model",
				Alias: "gpt-model",
				CostPolicy: &ModelCostPolicy{
					PriceStatus:     PriceStatusPaidConfirmed,
					BillingUnit:     BillingUnitTokens,
					PromptPer1M:     2.50,
					CompletionPer1M: 10.00,
				},
			},
		},
	}

	// Serialize provider's CostPolicy and Model's CostPolicy to JSON
	provData, err := json.Marshal(prov.CostPolicy)
	if err != nil {
		t.Fatalf("marshal provider cost policy: %v", err)
	}
	modelData, err := json.Marshal(prov.Models[0].CostPolicy)
	if err != nil {
		t.Fatalf("marshal model cost policy: %v", err)
	}

	// Verify no secrets exist in the cost policy JSON representations
	for _, text := range []string{string(provData), string(modelData)} {
		if strings.Contains(text, "sk-super-secret") {
			t.Fatal("secret API key leaked into cost policy representation")
		}
		if strings.Contains(text, "pass@") {
			t.Fatal("proxy credentials leaked into cost policy representation")
		}
	}

	// Verify cost policy fields are fully preserved
	var decodedModelPolicy ModelCostPolicy
	if err := json.Unmarshal(modelData, &decodedModelPolicy); err != nil {
		t.Fatalf("unmarshal model cost policy: %v", err)
	}
	if decodedModelPolicy.PriceStatus != PriceStatusPaidConfirmed || decodedModelPolicy.PromptPer1M != 2.50 {
		t.Errorf("cost policy fields corrupted: %+v", decodedModelPolicy)
	}
}

func TestExplicitZeroPriceIsPreserved(t *testing.T) {
	raw := `
openai-compatibility:
  - name: zero-cost-provider
    base-url: https://api.zero.com/v1
    models:
      - name: free-model
        alias: free-model
        cost-policy:
          price-status: free_confirmed
          billing-unit: tokens
          prompt-per-1m: 0
          completion-per-1m: 0
          unit-price: 0
          max-request-cost: 0
`
	cfg, err := ParseConfigBytes([]byte(raw))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}

	m := cfg.OpenAICompatibility[0].Models[0]
	if m.CostPolicy == nil {
		t.Fatal("expected CostPolicy to be present")
	}
	if m.CostPolicy.PromptPer1M != 0 || m.CostPolicy.CompletionPer1M != 0 || m.CostPolicy.UnitPrice != 0 {
		t.Errorf("prices not zero: %+v", m.CostPolicy)
	}
	if m.CostPolicy.MaxRequestCost != 0 {
		t.Errorf("max-request-cost not zero: %v", m.CostPolicy.MaxRequestCost)
	}
	if m.CostPolicy.PriceStatus != PriceStatusFreeConfirmed {
		t.Errorf("expected price-status free_confirmed, got %s", m.CostPolicy.PriceStatus)
	}
}

func TestMissingPricePolicyRemainsUnknown(t *testing.T) {
	m := OpenAICompatibilityModel{
		Name:  "no-policy-model",
		Alias: "no-policy-model",
	}
	if m.HasCostPolicy() {
		t.Fatal("expected HasCostPolicy()=false when unconfigured")
	}
	eff := m.EffectiveCostPolicy()
	if eff.PriceStatus != PriceStatusUnknown {
		t.Fatalf("expected PriceStatus=%s, got %s", PriceStatusUnknown, eff.PriceStatus)
	}
}

func TestCostGuardDisabledPreservesLegacyBehavior(t *testing.T) {
	raw := `
cost-guard:
  enabled: false
openai-compatibility:
  - name: provider-legacy
    base-url: https://api.legacy.com/v1
    models:
      - name: m-legacy
        alias: m-legacy
`
	cfg, err := ParseConfigBytes([]byte(raw))
	if err != nil {
		t.Fatalf("ParseConfigBytes error = %v", err)
	}
	if cfg.CostGuard.Enabled {
		t.Fatal("expected CostGuard to be disabled")
	}
	// Models without policy stay unconfigured
	if cfg.OpenAICompatibility[0].Models[0].CostPolicy != nil {
		t.Fatal("expected nil CostPolicy on legacy model")
	}
}

func TestEffectiveAtFormatValidation(t *testing.T) {
	validFormats := []string{
		"2026-10-29T15:04:05Z",
		"2026-10-29T15:04:05+08:00",
		"2026-10-29T15:04:05.123456Z",
		"2026-10-29 15:04:05",
		"2026-10-29",
	}
	for _, fmtStr := range validFormats {
		p := &ProviderCostPolicy{EffectiveAt: fmtStr}
		if err := ValidateProviderCostPolicy(p); err != nil {
			t.Errorf("format %q should be valid, got: %v", fmtStr, err)
		}
	}

	invalidFormats := []string{
		"invalid-time",
		"10/29/2026",
		"yesterday",
		"2026-99-99",
	}
	for _, fmtStr := range invalidFormats {
		p := &ProviderCostPolicy{EffectiveAt: fmtStr}
		if err := ValidateProviderCostPolicy(p); err == nil {
			t.Errorf("format %q should be rejected", fmtStr)
		}
	}
}
