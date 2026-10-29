package config

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// Supported Price Status constants
const (
	PriceStatusFreeConfirmed = "free_confirmed"
	PriceStatusPaidConfirmed = "paid_confirmed"
	PriceStatusUnknown       = "unknown"
	PriceStatusFrozen        = "frozen"
)

// Supported Billing Unit constants
const (
	BillingUnitTokens       = "tokens"
	BillingUnitAudioSeconds = "audio_seconds"
	BillingUnitCharacters   = "characters"
	BillingUnitImages       = "images"
	BillingUnitVideoTasks   = "video_tasks"
	BillingUnitVideoStatus  = "video_status"
)

// Supported Action constants
const (
	CostActionAllow  = "allow"
	CostActionDeny   = "deny"
	CostActionWarn   = "warn"
	CostActionFreeze = "freeze"
)

// Supported Currency constants
const (
	CurrencyUSD = "USD"
)

// CostGuardConfig defines global safety, budgeting, and audit policies for upstream pricing.
type CostGuardConfig struct {
	Enabled            bool               `yaml:"enabled" json:"enabled"`
	Currency           string             `yaml:"currency,omitempty" json:"currency,omitempty"`
	UnknownPriceAction string             `yaml:"unknown-price-action,omitempty" json:"unknown-price-action,omitempty"`
	PriceChangeAction  string             `yaml:"price-change-action,omitempty" json:"price-change-action,omitempty"`
	FreeToPaidAction   string             `yaml:"free-to-paid-action,omitempty" json:"free-to-paid-action,omitempty"`
	Fallback           CostFallbackPolicy `yaml:"fallback,omitempty" json:"fallback,omitempty"`
	Budget             CostBudgetPolicy   `yaml:"budget,omitempty" json:"budget,omitempty"`
	Audit              CostAuditPolicy    `yaml:"audit,omitempty" json:"audit,omitempty"`
}

// CostFallbackPolicy configures fallback pricing constraints.
type CostFallbackPolicy struct {
	Allow                  bool `yaml:"allow" json:"allow"`
	RequirePriceCompatible bool `yaml:"require-price-compatible" json:"require-price-compatible"`
	AllowUnknownPrice      bool `yaml:"allow-unknown-price" json:"allow-unknown-price"`
}

// CostBudgetPolicy configures global and per-request/provider budget enforcement.
type CostBudgetPolicy struct {
	EnforcePerRequest  bool    `yaml:"enforce-per-request" json:"enforce-per-request"`
	EnforcePerProvider bool    `yaml:"enforce-per-provider" json:"enforce-per-provider"`
	EnforceGlobal      bool    `yaml:"enforce-global" json:"enforce-global"`
	DailyUSD           float64 `yaml:"daily-usd" json:"daily-usd"`
	MonthlyUSD         float64 `yaml:"monthly-usd" json:"monthly-usd"`
}

// CostAuditPolicy configures logging and audit records for price decisions.
type CostAuditPolicy struct {
	RecordPriceAtRequest bool `yaml:"record-price-at-request" json:"record-price-at-request"`
	RecordPolicyDecision bool `yaml:"record-policy-decision" json:"record-policy-decision"`
}

// ProviderCostPolicy defines pricing safety rules at the provider level.
type ProviderCostPolicy struct {
	Mode               string               `yaml:"mode,omitempty" json:"mode,omitempty"`
	Source             string               `yaml:"source,omitempty" json:"source,omitempty"`
	EffectiveAt        string               `yaml:"effective-at,omitempty" json:"effective-at,omitempty"`
	UnknownPriceAction string               `yaml:"unknown-price-action,omitempty" json:"unknown-price-action,omitempty"`
	PriceChangeAction  string               `yaml:"price-change-action,omitempty" json:"price-change-action,omitempty"`
	FreeToPaidAction   string               `yaml:"free-to-paid-action,omitempty" json:"free-to-paid-action,omitempty"`
	AllowFallback      bool                 `yaml:"allow-fallback,omitempty" json:"allow-fallback,omitempty"`
	ProviderBudget     ProviderBudgetPolicy `yaml:"provider-budget,omitempty" json:"provider-budget,omitempty"`
}

// ProviderBudgetPolicy defines budget caps for a specific provider.
type ProviderBudgetPolicy struct {
	DailyUSD   float64 `yaml:"daily-usd,omitempty" json:"daily-usd,omitempty"`
	MonthlyUSD float64 `yaml:"monthly-usd,omitempty" json:"monthly-usd,omitempty"`
	OnExceeded string  `yaml:"on-exceeded,omitempty" json:"on-exceeded,omitempty"`
}

// ModelCostPolicy defines price and cost constraints for an individual model.
type ModelCostPolicy struct {
	PriceStatus        string  `yaml:"price-status,omitempty" json:"price-status,omitempty"`
	BillingUnit        string  `yaml:"billing-unit,omitempty" json:"billing-unit,omitempty"`
	PromptPer1M        float64 `yaml:"prompt-per-1m,omitempty" json:"prompt-per-1m,omitempty"`
	CompletionPer1M    float64 `yaml:"completion-per-1m,omitempty" json:"completion-per-1m,omitempty"`
	CachePer1M         float64 `yaml:"cache-per-1m,omitempty" json:"cache-per-1m,omitempty"`
	CacheReadPer1M     float64 `yaml:"cache-read-per-1m,omitempty" json:"cache-read-per-1m,omitempty"`
	CacheCreationPer1M float64 `yaml:"cache-creation-per-1m,omitempty" json:"cache-creation-per-1m,omitempty"`
	UnitPrice          float64 `yaml:"unit-price,omitempty" json:"unit-price,omitempty"`
	MaxRequestCost     float64 `yaml:"max-request-cost,omitempty" json:"max-request-cost,omitempty"`
	MaxDailyCost       float64 `yaml:"max-daily-cost,omitempty" json:"max-daily-cost,omitempty"`
	OnNonzeroPrice     string  `yaml:"on-nonzero-price,omitempty" json:"on-nonzero-price,omitempty"`
}

// DefaultCostGuardConfig returns standard safe defaults for CostGuardConfig.
func DefaultCostGuardConfig() CostGuardConfig {
	return CostGuardConfig{
		Enabled:            false,
		Currency:           CurrencyUSD,
		UnknownPriceAction: CostActionDeny,
		PriceChangeAction:  CostActionDeny,
		FreeToPaidAction:   CostActionDeny,
		Fallback: CostFallbackPolicy{
			Allow:                  true,
			RequirePriceCompatible: true,
			AllowUnknownPrice:      false,
		},
		Budget: CostBudgetPolicy{
			EnforcePerRequest:  true,
			EnforcePerProvider: true,
			EnforceGlobal:      true,
			DailyUSD:           0,
			MonthlyUSD:         0,
		},
		Audit: CostAuditPolicy{
			RecordPriceAtRequest: true,
			RecordPolicyDecision: true,
		},
	}
}

// ApplyDefaults fills in default values for unset fields in CostGuardConfig.
func (c *CostGuardConfig) ApplyDefaults() {
	if c == nil {
		return
	}
	if strings.TrimSpace(c.Currency) == "" {
		c.Currency = CurrencyUSD
	}
	if strings.TrimSpace(c.UnknownPriceAction) == "" {
		c.UnknownPriceAction = CostActionDeny
	}
	if strings.TrimSpace(c.PriceChangeAction) == "" {
		c.PriceChangeAction = CostActionDeny
	}
	if strings.TrimSpace(c.FreeToPaidAction) == "" {
		c.FreeToPaidAction = CostActionDeny
	}
}

// ApplyDefaults fills in default values for unset fields in ProviderCostPolicy.
func (p *ProviderCostPolicy) ApplyDefaults() {
	if p == nil {
		return
	}
	if strings.TrimSpace(p.UnknownPriceAction) == "" {
		p.UnknownPriceAction = CostActionDeny
	}
	if strings.TrimSpace(p.PriceChangeAction) == "" {
		p.PriceChangeAction = CostActionDeny
	}
	if strings.TrimSpace(p.FreeToPaidAction) == "" {
		p.FreeToPaidAction = CostActionDeny
	}
	if strings.TrimSpace(p.ProviderBudget.OnExceeded) == "" {
		p.ProviderBudget.OnExceeded = CostActionDeny
	}
}

// ApplyDefaults fills in default values for unset fields in ModelCostPolicy.
func (m *ModelCostPolicy) ApplyDefaults() {
	if m == nil {
		return
	}
	if strings.TrimSpace(m.PriceStatus) == "" {
		m.PriceStatus = PriceStatusUnknown
	}
	if strings.TrimSpace(m.BillingUnit) == "" {
		m.BillingUnit = BillingUnitTokens
	}
	if strings.TrimSpace(m.OnNonzeroPrice) == "" {
		m.OnNonzeroPrice = CostActionDeny
	}
}

// Clone returns a shallow copy of CostGuardConfig.
func (c CostGuardConfig) Clone() CostGuardConfig {
	return c
}

// Clone returns a deep copy of ProviderCostPolicy.
func (p *ProviderCostPolicy) Clone() *ProviderCostPolicy {
	if p == nil {
		return nil
	}
	cp := *p
	return &cp
}

// Clone returns a deep copy of ModelCostPolicy.
func (m *ModelCostPolicy) Clone() *ModelCostPolicy {
	if m == nil {
		return nil
	}
	cp := *m
	return &cp
}

// EffectiveCostPolicy returns the configured policy, or an unknown-status fallback when unset.
func (m OpenAICompatibilityModel) EffectiveCostPolicy() ModelCostPolicy {
	if m.CostPolicy != nil {
		return *m.CostPolicy
	}
	return ModelCostPolicy{
		PriceStatus:    PriceStatusUnknown,
		BillingUnit:    BillingUnitTokens,
		OnNonzeroPrice: CostActionDeny,
	}
}

// HasCostPolicy reports whether a model cost policy is explicitly configured.
func (m OpenAICompatibilityModel) HasCostPolicy() bool {
	return m.CostPolicy != nil
}

// EffectiveCostPolicy returns the configured policy on the provider, or default safe settings when unset.
func (p OpenAICompatibility) EffectiveCostPolicy() ProviderCostPolicy {
	if p.CostPolicy != nil {
		return *p.CostPolicy
	}
	return ProviderCostPolicy{
		UnknownPriceAction: CostActionDeny,
		PriceChangeAction:  CostActionDeny,
		FreeToPaidAction:   CostActionDeny,
		ProviderBudget: ProviderBudgetPolicy{
			OnExceeded: CostActionDeny,
		},
	}
}

// HasCostPolicy reports whether a provider cost policy is explicitly configured.
func (p OpenAICompatibility) HasCostPolicy() bool {
	return p.CostPolicy != nil
}

func isValidCostAction(action string) bool {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case CostActionAllow, CostActionDeny, CostActionWarn, CostActionFreeze:
		return true
	default:
		return false
	}
}

func isValidPriceStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case PriceStatusFreeConfirmed, PriceStatusPaidConfirmed, PriceStatusUnknown, PriceStatusFrozen:
		return true
	default:
		return false
	}
}

func isValidBillingUnit(unit string) bool {
	switch strings.ToLower(strings.TrimSpace(unit)) {
	case BillingUnitTokens, BillingUnitAudioSeconds, BillingUnitCharacters, BillingUnitImages, BillingUnitVideoTasks, BillingUnitVideoStatus:
		return true
	default:
		return false
	}
}

func isValidPriceNumber(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0
}

var effectiveAtLayouts = []string{
	time.RFC3339,
	time.RFC3339Nano,
	"2006-01-02T15:04:05Z07:00",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

func parseEffectiveAt(s string) (time.Time, error) {
	trimmed := strings.TrimSpace(s)
	for _, layout := range effectiveAtLayouts {
		if t, err := time.Parse(layout, trimmed); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("must be RFC3339 or ISO 8601 timestamp (e.g. %s)", time.RFC3339)
}

// ValidateCostGuardConfig validates the global cost-guard configuration.
func ValidateCostGuardConfig(c CostGuardConfig) error {
	if c.Currency != "" {
		currency := strings.ToUpper(strings.TrimSpace(c.Currency))
		if currency != CurrencyUSD {
			return fmt.Errorf("unsupported currency %q: only USD is supported", c.Currency)
		}
	}
	if c.UnknownPriceAction != "" && !isValidCostAction(c.UnknownPriceAction) {
		return fmt.Errorf("unsupported unknown-price-action: %q (must be allow, deny, warn, or freeze)", c.UnknownPriceAction)
	}
	if c.PriceChangeAction != "" && !isValidCostAction(c.PriceChangeAction) {
		return fmt.Errorf("unsupported price-change-action: %q (must be allow, deny, warn, or freeze)", c.PriceChangeAction)
	}
	if c.FreeToPaidAction != "" && !isValidCostAction(c.FreeToPaidAction) {
		return fmt.Errorf("unsupported free-to-paid-action: %q (must be allow, deny, warn, or freeze)", c.FreeToPaidAction)
	}
	if !isValidPriceNumber(c.Budget.DailyUSD) {
		return errors.New("budget daily-usd must be non-negative and finite")
	}
	if !isValidPriceNumber(c.Budget.MonthlyUSD) {
		return errors.New("budget monthly-usd must be non-negative and finite")
	}
	return nil
}

// ValidateProviderCostPolicy validates a provider-level cost policy.
func ValidateProviderCostPolicy(p *ProviderCostPolicy) error {
	if p == nil {
		return nil
	}
	if p.UnknownPriceAction != "" && !isValidCostAction(p.UnknownPriceAction) {
		return fmt.Errorf("unsupported provider unknown-price-action: %q", p.UnknownPriceAction)
	}
	if p.PriceChangeAction != "" && !isValidCostAction(p.PriceChangeAction) {
		return fmt.Errorf("unsupported provider price-change-action: %q", p.PriceChangeAction)
	}
	if p.FreeToPaidAction != "" && !isValidCostAction(p.FreeToPaidAction) {
		return fmt.Errorf("unsupported provider free-to-paid-action: %q", p.FreeToPaidAction)
	}
	if p.ProviderBudget.OnExceeded != "" && !isValidCostAction(p.ProviderBudget.OnExceeded) {
		return fmt.Errorf("unsupported provider budget on-exceeded action: %q", p.ProviderBudget.OnExceeded)
	}
	if !isValidPriceNumber(p.ProviderBudget.DailyUSD) {
		return errors.New("provider budget daily-usd must be non-negative and finite")
	}
	if !isValidPriceNumber(p.ProviderBudget.MonthlyUSD) {
		return errors.New("provider budget monthly-usd must be non-negative and finite")
	}
	if strings.TrimSpace(p.EffectiveAt) != "" {
		if _, err := parseEffectiveAt(p.EffectiveAt); err != nil {
			return fmt.Errorf("invalid effective-at %q: %w", p.EffectiveAt, err)
		}
	}
	return nil
}

// ValidateModelCostPolicy validates an individual model's cost policy.
func ValidateModelCostPolicy(m *ModelCostPolicy) error {
	if m == nil {
		return nil
	}
	if m.PriceStatus != "" && !isValidPriceStatus(m.PriceStatus) {
		return fmt.Errorf("unsupported price-status: %q (must be free_confirmed, paid_confirmed, unknown, or frozen)", m.PriceStatus)
	}
	if m.BillingUnit != "" && !isValidBillingUnit(m.BillingUnit) {
		return fmt.Errorf("unsupported billing-unit: %q (must be tokens, audio_seconds, characters, images, video_tasks, or video_status)", m.BillingUnit)
	}
	if m.OnNonzeroPrice != "" && !isValidCostAction(m.OnNonzeroPrice) {
		return fmt.Errorf("unsupported on-nonzero-price: %q (must be allow, deny, warn, or freeze)", m.OnNonzeroPrice)
	}

	prices := []struct {
		name  string
		value float64
	}{
		{"prompt-per-1m", m.PromptPer1M},
		{"completion-per-1m", m.CompletionPer1M},
		{"cache-per-1m", m.CachePer1M},
		{"cache-read-per-1m", m.CacheReadPer1M},
		{"cache-creation-per-1m", m.CacheCreationPer1M},
		{"unit-price", m.UnitPrice},
		{"max-request-cost", m.MaxRequestCost},
		{"max-daily-cost", m.MaxDailyCost},
	}
	for _, p := range prices {
		if math.IsNaN(p.value) || math.IsInf(p.value, 0) {
			return fmt.Errorf("%s must be a finite number", p.name)
		}
		if p.value < 0 {
			return fmt.Errorf("%s must not be negative: %v", p.name, p.value)
		}
	}

	status := strings.ToLower(strings.TrimSpace(m.PriceStatus))
	if status == PriceStatusFreeConfirmed {
		if m.PromptPer1M != 0 || m.CompletionPer1M != 0 || m.CachePer1M != 0 ||
			m.CacheReadPer1M != 0 || m.CacheCreationPer1M != 0 || m.UnitPrice != 0 {
			return fmt.Errorf("free_confirmed model must have all prices set to 0")
		}
	}

	if status == PriceStatusFrozen {
		if strings.EqualFold(strings.TrimSpace(m.OnNonzeroPrice), CostActionAllow) {
			return fmt.Errorf("frozen model cannot configure allow action for on-nonzero-price")
		}
	}

	return nil
}

// ValidateCostGuard validates all cost-guard policies across global, provider, and model configurations.
func (cfg *Config) ValidateCostGuard() error {
	if cfg == nil {
		return nil
	}
	if err := ValidateCostGuardConfig(cfg.CostGuard); err != nil {
		return fmt.Errorf("cost-guard: %w", err)
	}
	for i := range cfg.OpenAICompatibility {
		p := &cfg.OpenAICompatibility[i]
		if p.CostPolicy != nil {
			if err := ValidateProviderCostPolicy(p.CostPolicy); err != nil {
				return fmt.Errorf("openai-compatibility[%d] (%s): %w", i, p.Name, err)
			}
		}
		for j := range p.Models {
			m := &p.Models[j]
			if m.CostPolicy != nil {
				if err := ValidateModelCostPolicy(m.CostPolicy); err != nil {
					return fmt.Errorf("openai-compatibility[%d].models[%d] (%s): %w", i, j, m.Name, err)
				}
			}
		}
	}
	return nil
}
