package cost

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestFallbackCompatibilityMatrix(t *testing.T) {
	freeEst := CostEstimate{
		BillingUnit:    config.BillingUnitTokens,
		PriceStatus:    config.PriceStatusFreeConfirmed,
		ExplicitlyFree: true,
		Known:          true,
		AmountKnown:    true,
		AmountNanos:    0,
	}
	freePol := &config.ModelCostPolicy{
		PriceStatus: config.PriceStatusFreeConfirmed,
		BillingUnit: config.BillingUnitTokens,
	}

	paidLowEst := CostEstimate{
		BillingUnit: config.BillingUnitTokens,
		PriceStatus: config.PriceStatusPaidConfirmed,
		Known:       true,
		AmountKnown: true,
		AmountNanos: 1_000_000_000, // $1.00
	}
	paidLowPol := &config.ModelCostPolicy{
		PriceStatus:     config.PriceStatusPaidConfirmed,
		BillingUnit:     config.BillingUnitTokens,
		PromptPer1M:     1.0,
		CompletionPer1M: 2.0,
	}

	paidSameEst := CostEstimate{
		BillingUnit: config.BillingUnitTokens,
		PriceStatus: config.PriceStatusPaidConfirmed,
		Known:       true,
		AmountKnown: true,
		AmountNanos: 1_000_000_000, // $1.00
	}
	paidSamePol := &config.ModelCostPolicy{
		PriceStatus:     config.PriceStatusPaidConfirmed,
		BillingUnit:     config.BillingUnitTokens,
		PromptPer1M:     1.0,
		CompletionPer1M: 2.0,
	}

	paidHighEst := CostEstimate{
		BillingUnit: config.BillingUnitTokens,
		PriceStatus: config.PriceStatusPaidConfirmed,
		Known:       true,
		AmountKnown: true,
		AmountNanos: 3_000_000_000, // $3.00
	}
	paidHighPol := &config.ModelCostPolicy{
		PriceStatus:     config.PriceStatusPaidConfirmed,
		BillingUnit:     config.BillingUnitTokens,
		PromptPer1M:     3.0,
		CompletionPer1M: 6.0,
	}

	unknownEst := CostEstimate{
		BillingUnit: config.BillingUnitTokens,
		PriceStatus: config.PriceStatusUnknown,
		Known:       false,
		AmountKnown: false,
		Status:      StatusUnknownPrice,
	}
	unknownPol := &config.ModelCostPolicy{
		PriceStatus: config.PriceStatusUnknown,
		BillingUnit: config.BillingUnitTokens,
	}

	frozenEst := CostEstimate{
		BillingUnit: config.BillingUnitTokens,
		PriceStatus: config.PriceStatusFrozen,
		Known:       false,
		AmountKnown: false,
		Status:      StatusFrozen,
	}
	frozenPol := &config.ModelCostPolicy{
		PriceStatus: config.PriceStatusFrozen,
		BillingUnit: config.BillingUnitTokens,
	}

	diffUnitEst := CostEstimate{
		BillingUnit: config.BillingUnitAudioSeconds,
		PriceStatus: config.PriceStatusPaidConfirmed,
		Known:       true,
		AmountKnown: true,
		AmountNanos: 500_000_000,
	}
	diffUnitPol := &config.ModelCostPolicy{
		PriceStatus: config.PriceStatusPaidConfirmed,
		BillingUnit: config.BillingUnitAudioSeconds,
		UnitPrice:   0.005,
	}

	// 1. Free -> Free: ALLOW
	if ok, _ := IsFallbackPriceCompatible(freeEst, freeEst, freePol, freePol); !ok {
		t.Error("free -> free should be ALLOWED")
	}

	// 2. Free -> Paid: DENY
	if ok, reason := IsFallbackPriceCompatible(freeEst, paidLowEst, freePol, paidLowPol); ok {
		t.Errorf("free -> paid should be DENIED, got allowed (reason: %s)", reason)
	}

	// 3. Free -> Unknown: DENY
	if ok, _ := IsFallbackPriceCompatible(freeEst, unknownEst, freePol, unknownPol); ok {
		t.Error("free -> unknown should be DENIED")
	}

	// 4. Paid -> Free: ALLOW
	if ok, _ := IsFallbackPriceCompatible(paidLowEst, freeEst, paidLowPol, freePol); !ok {
		t.Error("paid -> free should be ALLOWED")
	}

	// 5. Paid -> Same Price: ALLOW
	if ok, _ := IsFallbackPriceCompatible(paidLowEst, paidSameEst, paidLowPol, paidSamePol); !ok {
		t.Error("paid -> same price should be ALLOWED")
	}

	// 6. Paid (High) -> Lower Price: ALLOW
	if ok, _ := IsFallbackPriceCompatible(paidHighEst, paidLowEst, paidHighPol, paidLowPol); !ok {
		t.Error("paid -> lower price should be ALLOWED")
	}

	// 7. Paid (Low) -> Higher Price: DENY
	if ok, _ := IsFallbackPriceCompatible(paidLowEst, paidHighEst, paidLowPol, paidHighPol); ok {
		t.Error("paid -> higher price should be DENIED")
	}

	// 8. Paid -> Unknown: DENY
	if ok, _ := IsFallbackPriceCompatible(paidLowEst, unknownEst, paidLowPol, unknownPol); ok {
		t.Error("paid -> unknown should be DENIED")
	}

	// 9. Any -> Frozen: DENY
	if ok, _ := IsFallbackPriceCompatible(paidLowEst, frozenEst, paidLowPol, frozenPol); ok {
		t.Error("paid -> frozen should be DENIED")
	}
	if ok, _ := IsFallbackPriceCompatible(freeEst, frozenEst, freePol, frozenPol); ok {
		t.Error("free -> frozen should be DENIED")
	}

	// 10. Different billing unit: DENY
	if ok, reason := IsFallbackPriceCompatible(paidLowEst, diffUnitEst, paidLowPol, diffUnitPol); ok {
		t.Errorf("tokens -> audio_seconds should be DENIED, got allowed (%s)", reason)
	}
}
