package cost

import (
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// Fallback decision reasons.
const (
	FallbackReasonAllowed                  = "fallback_allowed"
	FallbackReasonPrimaryFreeCandidatePaid = "free_to_paid_fallback_blocked"
	FallbackReasonCandidateUnknown         = "unknown_price_fallback_blocked"
	FallbackReasonCandidateHigherPrice     = "higher_price_fallback_blocked"
	FallbackReasonCandidateFrozen          = "frozen_fallback_blocked"
	FallbackReasonBillingUnitMismatch      = "billing_unit_mismatch_fallback_blocked"
	FallbackReasonInvalidPricePolicy       = "invalid_price_policy_fallback_blocked"
)

// IsFallbackPriceCompatible checks if a candidate provider/model is price-compatible with the primary.
//
// Compatibility Rules:
// 1. Candidate is frozen -> DENY
// 2. Billing units differ -> DENY
// 3. Candidate price status is unknown -> DENY
// 4. Primary is free_confirmed:
//   - Candidate is free_confirmed -> ALLOW
//   - Candidate is paid_confirmed -> DENY (Free cannot fall back to Paid!)
//   - Candidate is unknown -> DENY
//
// 5. Primary is paid_confirmed:
//   - Candidate is free_confirmed -> ALLOW (Paid falling back to Free is always safe!)
//   - Candidate is paid_confirmed:
//   - Candidate estimated cost <= primary estimated cost -> ALLOW (same price or cheaper)
//   - Candidate estimated cost > primary estimated cost -> DENY (cannot fall back to more expensive provider!)
//   - If cost not quantifiable, compare unit rate / prompt rate: candidate rate <= primary rate -> ALLOW, else DENY.
func IsFallbackPriceCompatible(primary, candidate CostEstimate, primaryPolicy, candidatePolicy *config.ModelCostPolicy) (bool, string) {
	// Rule 1: Candidate is frozen
	if strings.EqualFold(strings.TrimSpace(candidate.PriceStatus), config.PriceStatusFrozen) ||
		(candidatePolicy != nil && strings.EqualFold(strings.TrimSpace(candidatePolicy.PriceStatus), config.PriceStatusFrozen)) {
		return false, FallbackReasonCandidateFrozen
	}

	// Rule 2: Billing unit mismatch
	primaryUnit := strings.TrimSpace(primary.BillingUnit)
	if primaryUnit == "" && primaryPolicy != nil {
		primaryUnit = strings.TrimSpace(primaryPolicy.BillingUnit)
	}
	candidateUnit := strings.TrimSpace(candidate.BillingUnit)
	if candidateUnit == "" && candidatePolicy != nil {
		candidateUnit = strings.TrimSpace(candidatePolicy.BillingUnit)
	}
	if primaryUnit != "" && candidateUnit != "" && !strings.EqualFold(primaryUnit, candidateUnit) {
		return false, FallbackReasonBillingUnitMismatch
	}

	// Rule 3: Candidate price is unknown
	if !candidate.Known && !candidate.ExplicitlyFree {
		if strings.EqualFold(strings.TrimSpace(candidate.PriceStatus), config.PriceStatusUnknown) ||
			candidate.PriceStatus == "" ||
			candidate.Status == StatusUnknownPrice ||
			candidate.Status == StatusInvalidPolicy {
			return false, FallbackReasonCandidateUnknown
		}
	}

	// Rule 4: Primary is Free
	if primary.ExplicitlyFree || strings.EqualFold(strings.TrimSpace(primary.PriceStatus), config.PriceStatusFreeConfirmed) {
		if candidate.ExplicitlyFree || strings.EqualFold(strings.TrimSpace(candidate.PriceStatus), config.PriceStatusFreeConfirmed) {
			return true, FallbackReasonAllowed
		}
		// Free -> Paid is strictly forbidden!
		return false, FallbackReasonPrimaryFreeCandidatePaid
	}

	// Rule 5: Primary is Paid
	if strings.EqualFold(strings.TrimSpace(primary.PriceStatus), config.PriceStatusPaidConfirmed) {
		// Paid -> Free is always allowed (saves cost)
		if candidate.ExplicitlyFree || strings.EqualFold(strings.TrimSpace(candidate.PriceStatus), config.PriceStatusFreeConfirmed) {
			return true, FallbackReasonAllowed
		}

		// Paid -> Paid: Compare deterministic nanodollars
		if primary.AmountKnown && candidate.AmountKnown {
			if candidate.AmountNanos <= primary.AmountNanos {
				return true, FallbackReasonAllowed
			}
			return false, fmt.Sprintf("%s: candidate %d nanos > primary %d nanos", FallbackReasonCandidateHigherPrice, candidate.AmountNanos, primary.AmountNanos)
		}

		// If amount is not known (e.g. Pre-flight without token count), compare unit rates directly
		if primaryPolicy != nil && candidatePolicy != nil {
			if primaryUnit == config.BillingUnitTokens {
				// Compare prompt rate and completion rate
				if candidatePolicy.PromptPer1M <= primaryPolicy.PromptPer1M &&
					candidatePolicy.CompletionPer1M <= primaryPolicy.CompletionPer1M {
					return true, FallbackReasonAllowed
				}
				return false, FallbackReasonCandidateHigherPrice
			}

			// Non-token unit price
			if candidatePolicy.UnitPrice <= primaryPolicy.UnitPrice {
				return true, FallbackReasonAllowed
			}
			return false, FallbackReasonCandidateHigherPrice
		}
	}

	// Default fallback deny if indeterminate
	return false, FallbackReasonCandidateUnknown
}
