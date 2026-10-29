package cost

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// PolicySnapshot defines the sanitized, versioned price policy exchange contract between CPAMP and CLIProxyAPI.
// NOTE: This contract explicitly excludes secrets, API keys, proxy URLs, node URIs, cookies, and credentials.
type PolicySnapshot struct {
	SchemaVersion            string  `json:"schema_version"`
	PolicyVersion            string  `json:"policy_version"`
	Provider                 string  `json:"provider"`
	PublicModel              string  `json:"public_model"`
	UpstreamModelHash        string  `json:"upstream_model_hash"`
	PriceStatus              string  `json:"price_status"`
	BillingUnit              string  `json:"billing_unit"`
	PromptPer1M              float64 `json:"prompt_per_1m"`
	CompletionPer1M          float64 `json:"completion_per_1m"`
	CachePer1M               float64 `json:"cache_per_1m"`
	CacheReadPer1M           float64 `json:"cache_read_per_1m"`
	CacheCreationPer1M       float64 `json:"cache_creation_per_1m"`
	UnitPrice                float64 `json:"unit_price"`
	MaxRequestCostUSD        float64 `json:"max_request_cost_usd"`
	ProviderDailyBudgetUSD   float64 `json:"provider_daily_budget_usd"`
	ProviderMonthlyBudgetUSD float64 `json:"provider_monthly_budget_usd"`
	GlobalDailyBudgetUSD     float64 `json:"global_daily_budget_usd"`
	GlobalMonthlyBudgetUSD   float64 `json:"global_monthly_budget_usd"`
	EffectiveAtMS            int64   `json:"effective_at_ms"`
	ExpiresAtMS              int64   `json:"expires_at_ms,omitempty"`
	Source                   string  `json:"source"`
	ContentHash              string  `json:"content_hash"`
	Signature                string  `json:"signature,omitempty"`
}

// ComputeContentHash computes deterministic SHA-256 hash over the policy payload.
// signature and content_hash fields are strictly excluded.
func (s *PolicySnapshot) ComputeContentHash() string {
	raw := fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s|%.6f|%.6f|%.6f|%.6f|%.6f|%.6f|%.6f|%.6f|%.6f|%.6f|%.6f|%d|%d|%s",
		strings.TrimSpace(s.SchemaVersion),
		strings.TrimSpace(s.PolicyVersion),
		strings.ToLower(strings.TrimSpace(s.Provider)),
		strings.ToLower(strings.TrimSpace(s.PublicModel)),
		strings.TrimSpace(s.UpstreamModelHash),
		strings.ToLower(strings.TrimSpace(s.PriceStatus)),
		strings.ToLower(strings.TrimSpace(s.BillingUnit)),
		s.PromptPer1M,
		s.CompletionPer1M,
		s.CachePer1M,
		s.CacheReadPer1M,
		s.CacheCreationPer1M,
		s.UnitPrice,
		s.MaxRequestCostUSD,
		s.ProviderDailyBudgetUSD,
		s.ProviderMonthlyBudgetUSD,
		s.GlobalDailyBudgetUSD,
		s.GlobalMonthlyBudgetUSD,
		s.EffectiveAtMS,
		s.ExpiresAtMS,
		strings.TrimSpace(s.Source),
	)
	h := sha256.Sum256([]byte(raw))
	return "sha256:" + hex.EncodeToString(h[:])
}

// ComputeSignature calculates an HMAC-SHA256 signature for the content hash.
func (s *PolicySnapshot) ComputeSignature(secret string) string {
	if secret == "" {
		return ""
	}
	hash := s.ComputeContentHash()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(hash))
	return "hmac-sha256:" + hex.EncodeToString(mac.Sum(nil))
}

// VerifySignature verifies the signature against the provided secret.
func (s *PolicySnapshot) VerifySignature(secret string) bool {
	if secret == "" {
		return true
	}
	if s.Signature == "" {
		return false
	}
	expected := s.ComputeSignature(secret)
	return hmac.Equal([]byte(s.Signature), []byte(expected))
}

// IsExpired checks whether the policy is expired at the given timestamp.
func (s *PolicySnapshot) IsExpired(nowMS int64) bool {
	if s.ExpiresAtMS <= 0 {
		return false
	}
	return nowMS > s.ExpiresAtMS
}

// Validate verifies that the policy snapshot is structurally sound and satisfies security constraints.
func (s *PolicySnapshot) Validate() error {
	if strings.TrimSpace(s.SchemaVersion) == "" {
		s.SchemaVersion = "cost-policy/v1"
	}
	if s.SchemaVersion != "cost-policy/v1" {
		return fmt.Errorf("unsupported schema_version: %s", s.SchemaVersion)
	}
	if strings.TrimSpace(s.PolicyVersion) == "" {
		return errors.New("policy_version is required")
	}
	if strings.TrimSpace(s.Provider) == "" {
		return errors.New("provider is required")
	}
	if strings.TrimSpace(s.PublicModel) == "" {
		return errors.New("public_model is required")
	}
	if strings.TrimSpace(s.BillingUnit) == "" {
		return errors.New("billing_unit is required")
	}
	if s.EffectiveAtMS <= 0 {
		return errors.New("effective_at_ms must be positive")
	}
	if s.ExpiresAtMS > 0 && s.ExpiresAtMS < s.EffectiveAtMS {
		return errors.New("expires_at_ms cannot be earlier than effective_at_ms")
	}
	priceStatus := strings.ToLower(strings.TrimSpace(s.PriceStatus))
	if priceStatus == "unknown" {
		return errors.New("unknown price policy is not allowed")
	}
	if priceStatus == "free_confirmed" {
		if s.PromptPer1M > 0 || s.CompletionPer1M > 0 || s.CachePer1M > 0 || s.CacheReadPer1M > 0 || s.CacheCreationPer1M > 0 || s.UnitPrice > 0 {
			return errors.New("free_confirmed policy cannot have non-zero prices")
		}
	}
	if s.PromptPer1M < 0 || s.CompletionPer1M < 0 || s.CachePer1M < 0 || s.CacheReadPer1M < 0 || s.CacheCreationPer1M < 0 || s.UnitPrice < 0 || s.MaxRequestCostUSD < 0 {
		return errors.New("negative price values are forbidden")
	}

	// Security check: ensure no secrets are leaked in metadata
	for _, str := range []string{s.Provider, s.PublicModel, s.Source} {
		lower := strings.ToLower(str)
		if strings.Contains(lower, "bearer ") || strings.Contains(lower, "sk-") || strings.Contains(lower, "password") || strings.Contains(lower, "secret") {
			return errors.New("policy snapshot contains prohibited sensitive content")
		}
	}

	expectedHash := s.ComputeContentHash()
	if s.ContentHash != "" {
		normGot := s.ContentHash
		if !strings.HasPrefix(normGot, "sha256:") {
			normGot = "sha256:" + normGot
		}
		if normGot != expectedHash {
			return fmt.Errorf("content_hash mismatch: got %s, want %s", s.ContentHash, expectedHash)
		}
	} else {
		s.ContentHash = expectedHash
	}
	return nil
}

// UnmarshalPolicySnapshotStrict unmarshals JSON and rejects any illegal or unknown fields.
func UnmarshalPolicySnapshotStrict(data []byte) (*PolicySnapshot, error) {
	var p PolicySnapshot
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("invalid policy snapshot JSON: %w", err)
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}
