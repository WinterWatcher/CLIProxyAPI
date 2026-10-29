package cost

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// PolicySnapshot defines the sanitized, versioned price policy exchange contract between CPAMP and CLIProxyAPI.
// NOTE: This contract explicitly excludes secrets, API keys, proxy URLs, node URIs, cookies, and credentials.
type PolicySnapshot struct {
	PolicyVersion     string  `json:"policy_version"`
	Provider          string  `json:"provider"`
	PublicModel       string  `json:"public_model"`
	UpstreamModelHash string  `json:"upstream_model_hash"`
	PriceStatus       string  `json:"price_status"`
	BillingUnit       string  `json:"billing_unit"`
	PromptPer1M       float64 `json:"prompt_per_1m"`
	CompletionPer1M   float64 `json:"completion_per_1m"`
	CachePer1M        float64 `json:"cache_per_1m"`
	UnitPrice         float64 `json:"unit_price"`
	MaxRequestCostUSD float64 `json:"max_request_cost_usd"`
	ProviderBudgetUSD float64 `json:"provider_budget_usd"`
	GlobalBudgetUSD   float64 `json:"global_budget_usd"`
	EffectiveAtMS     int64   `json:"effective_at_ms"`
	ExpiresAtMS       int64   `json:"expires_at_ms,omitempty"`
	ContentHash       string  `json:"content_hash"`
}

// ComputeContentHash computes deterministic SHA-256 hash over the policy payload.
func (s *PolicySnapshot) ComputeContentHash() string {
	raw := fmt.Sprintf("%s|%s|%s|%s|%s|%s|%.6f|%.6f|%.6f|%.6f|%.6f|%.6f|%.6f|%d|%d",
		strings.TrimSpace(s.PolicyVersion),
		strings.ToLower(strings.TrimSpace(s.Provider)),
		strings.ToLower(strings.TrimSpace(s.PublicModel)),
		strings.TrimSpace(s.UpstreamModelHash),
		strings.ToLower(strings.TrimSpace(s.PriceStatus)),
		strings.ToLower(strings.TrimSpace(s.BillingUnit)),
		s.PromptPer1M,
		s.CompletionPer1M,
		s.CachePer1M,
		s.UnitPrice,
		s.MaxRequestCostUSD,
		s.ProviderBudgetUSD,
		s.GlobalBudgetUSD,
		s.EffectiveAtMS,
		s.ExpiresAtMS,
	)
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

// Validate verifies that the policy snapshot is structurally sound and satisfies security constraints.
func (s *PolicySnapshot) Validate() error {
	if strings.TrimSpace(s.PolicyVersion) == "" {
		return errors.New("policy_version is required")
	}
	if strings.TrimSpace(s.Provider) == "" {
		return errors.New("provider is required")
	}
	if strings.TrimSpace(s.PublicModel) == "" {
		return errors.New("public_model is required")
	}
	if s.EffectiveAtMS <= 0 {
		return errors.New("effective_at_ms must be positive")
	}
	expectedHash := s.ComputeContentHash()
	if s.ContentHash != "" && s.ContentHash != expectedHash {
		return fmt.Errorf("content_hash mismatch: got %s, want %s", s.ContentHash, expectedHash)
	}
	return nil
}
