package cost

import (
	"fmt"
	"strings"
)

// PolicyDiff captures the structured difference between an active and candidate policy.
type PolicyDiff struct {
	Provider          string  `json:"provider"`
	PublicModel       string  `json:"public_model"`
	OldPolicyVersion  string  `json:"old_policy_version,omitempty"`
	NewPolicyVersion  string  `json:"new_policy_version"`
	OldPriceStatus    string  `json:"old_price_status,omitempty"`
	NewPriceStatus    string  `json:"new_price_status"`
	OldBillingUnit    string  `json:"old_billing_unit,omitempty"`
	NewBillingUnit    string  `json:"new_billing_unit"`
	OldUnitPrice      float64 `json:"old_unit_price"`
	NewUnitPrice      float64 `json:"new_unit_price"`
	OldMaxRequestCost float64 `json:"old_max_request_cost"`
	NewMaxRequestCost float64 `json:"new_max_request_cost"`
	RiskLevel         string  `json:"risk_level"` // "critical", "high", "medium", "low"
	Decision          string  `json:"decision"`   // "allow", "deny", "pending"
	Reason            string  `json:"reason"`
}

// DryRunReport summarizes the dry-run evaluation outcome.
type DryRunReport struct {
	Diffs     []PolicyDiff `json:"diffs"`
	RiskLevel string       `json:"risk_level"`
	Decision  string       `json:"decision"`
	Allowed   bool         `json:"allowed"`
	Summary   string       `json:"summary"`
}

// ComputePolicyDiff compares old and new policy snapshots and classifies risk.
func ComputePolicyDiff(oldSnap, newSnap *PolicySnapshot) PolicyDiff {
	diff := PolicyDiff{
		Provider:          newSnap.Provider,
		PublicModel:       newSnap.PublicModel,
		NewPolicyVersion:  newSnap.PolicyVersion,
		NewPriceStatus:    newSnap.PriceStatus,
		NewBillingUnit:    newSnap.BillingUnit,
		NewUnitPrice:      newSnap.UnitPrice,
		NewMaxRequestCost: newSnap.MaxRequestCostUSD,
		RiskLevel:         "low",
		Decision:          "allow",
		Reason:            "price unchanged",
	}

	if oldSnap == nil {
		if strings.ToLower(newSnap.PriceStatus) == "unknown" {
			diff.RiskLevel = "critical"
			diff.Decision = "deny"
			diff.Reason = "unknown price policy is not allowed"
			return diff
		}
		if strings.ToLower(newSnap.PriceStatus) == "free_confirmed" {
			diff.RiskLevel = "low"
			diff.Decision = "allow"
			diff.Reason = "new free model policy added"
			return diff
		}
		if newSnap.Source == "cpamp-approved" {
			diff.RiskLevel = "low"
			diff.Decision = "allow"
			diff.Reason = "new approved model price policy added"
			return diff
		}
		diff.RiskLevel = "medium"
		diff.Decision = "pending"
		diff.Reason = "new model price policy added"
		return diff
	}

	diff.OldPolicyVersion = oldSnap.PolicyVersion
	diff.OldPriceStatus = oldSnap.PriceStatus
	diff.OldBillingUnit = oldSnap.BillingUnit
	diff.OldUnitPrice = oldSnap.UnitPrice
	diff.OldMaxRequestCost = oldSnap.MaxRequestCostUSD

	oldStatus := strings.ToLower(strings.TrimSpace(oldSnap.PriceStatus))
	newStatus := strings.ToLower(strings.TrimSpace(newSnap.PriceStatus))

	// Rule 1: free -> paid (critical + deny)
	if oldStatus == "free_confirmed" && newStatus != "free_confirmed" {
		diff.RiskLevel = "critical"
		diff.Decision = "deny"
		diff.Reason = "free to paid price change rejected"
		return diff
	}

	// Rule 2: known -> unknown (critical + deny)
	if oldStatus != "unknown" && newStatus == "unknown" {
		diff.RiskLevel = "critical"
		diff.Decision = "deny"
		diff.Reason = "known to unknown price status rejected"
		return diff
	}

	// Rule 3: billing unit changed (critical + deny)
	if oldSnap.BillingUnit != "" && newSnap.BillingUnit != "" && !strings.EqualFold(oldSnap.BillingUnit, newSnap.BillingUnit) {
		diff.RiskLevel = "critical"
		diff.Decision = "deny"
		diff.Reason = fmt.Sprintf("billing unit change (%s -> %s) requires freezing", oldSnap.BillingUnit, newSnap.BillingUnit)
		return diff
	}

	// Rule 4: upstream model changed (critical + deny)
	if oldSnap.UpstreamModelHash != "" && newSnap.UpstreamModelHash != "" && oldSnap.UpstreamModelHash != newSnap.UpstreamModelHash {
		diff.RiskLevel = "critical"
		diff.Decision = "deny"
		diff.Reason = "upstream model remapped"
		return diff
	}

	// Rule 5: price increase (high + pending)
	priceIncreased := newSnap.PromptPer1M > oldSnap.PromptPer1M ||
		newSnap.CompletionPer1M > oldSnap.CompletionPer1M ||
		newSnap.UnitPrice > oldSnap.UnitPrice ||
		newSnap.CachePer1M > oldSnap.CachePer1M ||
		newSnap.CacheReadPer1M > oldSnap.CacheReadPer1M ||
		newSnap.CacheCreationPer1M > oldSnap.CacheCreationPer1M

	if priceIncreased {
		diff.RiskLevel = "high"
		diff.Decision = "pending"
		diff.Reason = "price increased"
		return diff
	}

	// Rule 6: price decrease (medium + pending)
	priceDecreased := newSnap.PromptPer1M < oldSnap.PromptPer1M ||
		newSnap.CompletionPer1M < oldSnap.CompletionPer1M ||
		newSnap.UnitPrice < oldSnap.UnitPrice ||
		newSnap.CachePer1M < oldSnap.CachePer1M ||
		newSnap.CacheReadPer1M < oldSnap.CacheReadPer1M ||
		newSnap.CacheCreationPer1M < oldSnap.CacheCreationPer1M

	if priceDecreased {
		diff.RiskLevel = "medium"
		diff.Decision = "pending"
		diff.Reason = "price decreased"
		return diff
	}

	// Rule 7: price unchanged (low + allow)
	diff.RiskLevel = "low"
	diff.Decision = "allow"
	diff.Reason = "price unchanged"
	return diff
}
