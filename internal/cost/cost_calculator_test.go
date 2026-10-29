package cost

import (
	"math"
	"testing"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestTokenCostZeroTokens(t *testing.T) {
	policy := &config.ModelCostPolicy{
		PriceStatus:     config.PriceStatusPaidConfirmed,
		BillingUnit:     config.BillingUnitTokens,
		PromptPer1M:     1.50,
		CompletionPer1M: 3.00,
	}
	m := FromTokens(0, 0, 0, 0, 0)
	est := EstimateCost(CostRequest{
		Provider:    "test-prov",
		Model:       "test-model",
		Policy:      policy,
		Measurement: m,
	})

	if !est.Known || !est.AmountKnown {
		t.Fatalf("expected known amount for 0 tokens, got: %+v", est)
	}
	if est.Amount != 0 || est.AmountNanos != 0 {
		t.Errorf("expected 0 cost, got %v (%d nanos)", est.Amount, est.AmountNanos)
	}
	if est.Status != StatusKnown {
		t.Errorf("expected status %s, got %s", StatusKnown, est.Status)
	}
	if est.ExplicitlyFree {
		t.Errorf("paid policy with 0 tokens should not be marked ExplicitlyFree")
	}
}

func TestTokenCostOneToken(t *testing.T) {
	policy := &config.ModelCostPolicy{
		PriceStatus: config.PriceStatusPaidConfirmed,
		BillingUnit: config.BillingUnitTokens,
		PromptPer1M: 1.00, // $1 per 1M -> 1 token = $0.000001 (1000 nanos)
	}
	m := FromTokens(1, 0, 0, 0, 0)
	est := EstimateCost(CostRequest{
		Policy:      policy,
		Measurement: m,
	})

	if !est.Known || !est.AmountKnown {
		t.Fatalf("expected known cost: %+v", est)
	}
	if est.AmountNanos != 1000 {
		t.Errorf("expected 1000 nanos, got %d", est.AmountNanos)
	}
	if math.Abs(est.Amount-0.000001) > 1e-12 {
		t.Errorf("expected $0.000001, got %v", est.Amount)
	}
}

func TestTokenCost999999And1000000Tokens(t *testing.T) {
	policy := &config.ModelCostPolicy{
		PriceStatus: config.PriceStatusPaidConfirmed,
		BillingUnit: config.BillingUnitTokens,
		PromptPer1M: 2.50,
	}

	// 1,000,000 tokens at $2.50/1M must be exactly $2.50 and 2,500,000,000 nanos
	m1M := FromTokens(1_000_000, 0, 0, 0, 0)
	est1M := EstimateCost(CostRequest{
		Policy:      policy,
		Measurement: m1M,
	})
	if est1M.AmountNanos != 2_500_000_000 {
		t.Errorf("expected 2,500,000,000 nanos for 1M tokens, got %d", est1M.AmountNanos)
	}
	if est1M.Amount != 2.50 {
		t.Errorf("expected 2.50 USD for 1M tokens, got %v", est1M.Amount)
	}

	// 999,999 tokens at $2.50/1M = $2.4999975 -> 2,499,997,500 nanos
	m999k := FromTokens(999_999, 0, 0, 0, 0)
	est999k := EstimateCost(CostRequest{
		Policy:      policy,
		Measurement: m999k,
	})
	if est999k.AmountNanos != 2_499_997_500 {
		t.Errorf("expected 2,499,997,500 nanos for 999,999 tokens, got %d", est999k.AmountNanos)
	}
}

func TestTokenCostInputAndOutputTogether(t *testing.T) {
	policy := &config.ModelCostPolicy{
		PriceStatus:     config.PriceStatusPaidConfirmed,
		BillingUnit:     config.BillingUnitTokens,
		PromptPer1M:     2.00, // 500k tokens = $1.00
		CompletionPer1M: 8.00, // 250k tokens = $2.00
	}
	m := FromTokens(500_000, 250_000, 0, 0, 0)
	est := EstimateCost(CostRequest{
		Policy:      policy,
		Measurement: m,
	})

	if !est.Known || !est.AmountKnown {
		t.Fatalf("expected known cost: %+v", est)
	}
	expectedNanos := int64(3_000_000_000) // $3.00
	if est.AmountNanos != expectedNanos {
		t.Errorf("expected %d nanos, got %d", expectedNanos, est.AmountNanos)
	}
	if est.Amount != 3.00 {
		t.Errorf("expected 3.00 USD, got %v", est.Amount)
	}
	if est.Quantity != 750_000 {
		t.Errorf("expected 750,000 total tokens quantity, got %v", est.Quantity)
	}
}

func TestTokenCostCacheBreakdown(t *testing.T) {
	policy := &config.ModelCostPolicy{
		PriceStatus:        config.PriceStatusPaidConfirmed,
		BillingUnit:        config.BillingUnitTokens,
		PromptPer1M:        3.00,
		CompletionPer1M:    15.00,
		CachePer1M:         0.75, // legacy cache
		CacheReadPer1M:     0.30,
		CacheCreationPer1M: 3.75,
	}
	// 100k prompt ($0.30), 50k completion ($0.75), 100k cache ($0.075), 200k cacheRead ($0.06), 40k cacheCreate ($0.15)
	// total = 0.30 + 0.75 + 0.075 + 0.06 + 0.15 = 1.335 USD = 1,335,000,000 nanos
	m := FromTokens(100_000, 50_000, 100_000, 200_000, 40_000)
	est := EstimateCost(CostRequest{
		Policy:      policy,
		Measurement: m,
	})

	if !est.Known || !est.AmountKnown {
		t.Fatalf("expected known cost: %+v", est)
	}
	expectedNanos := int64(1_335_000_000)
	if est.AmountNanos != expectedNanos {
		t.Errorf("expected %d nanos, got %d", expectedNanos, est.AmountNanos)
	}
	if math.Abs(est.Amount-1.335) > 1e-9 {
		t.Errorf("expected 1.335 USD, got %v", est.Amount)
	}
}

func TestExplicitZeroFreePrice(t *testing.T) {
	policy := &config.ModelCostPolicy{
		PriceStatus:        config.PriceStatusFreeConfirmed,
		BillingUnit:        config.BillingUnitTokens,
		PromptPer1M:        0,
		CompletionPer1M:    0,
		CachePer1M:         0,
		CacheReadPer1M:     0,
		CacheCreationPer1M: 0,
		UnitPrice:          0,
	}
	m := FromTokens(1_000_000, 1_000_000, 0, 0, 0)
	est := EstimateCost(CostRequest{
		Policy:      policy,
		Measurement: m,
	})

	if !est.Known || !est.AmountKnown {
		t.Fatalf("expected known cost: %+v", est)
	}
	if !est.ExplicitlyFree {
		t.Fatal("expected ExplicitlyFree=true for free_confirmed policy")
	}
	if est.Status != StatusExplicitFree {
		t.Fatalf("expected status %s, got %s", StatusExplicitFree, est.Status)
	}
	if est.Amount != 0 || est.AmountNanos != 0 {
		t.Errorf("expected 0 cost, got %v (%d nanos)", est.Amount, est.AmountNanos)
	}
}

func TestUnknownPricePolicy(t *testing.T) {
	// 1. Nil policy
	m := FromTokens(100, 100, 0, 0, 0)
	estNil := EstimateCost(CostRequest{
		Policy:      nil,
		Measurement: m,
	})
	if estNil.Known || estNil.AmountKnown {
		t.Fatalf("nil policy should not produce known amount: %+v", estNil)
	}
	if estNil.Status != StatusUnknownPrice {
		t.Errorf("expected status %s, got %s", StatusUnknownPrice, estNil.Status)
	}
	if estNil.ExplicitlyFree {
		t.Fatal("nil policy must NEVER be marked ExplicitlyFree")
	}

	// 2. Policy with PriceStatusUnknown
	policyUnknown := &config.ModelCostPolicy{
		PriceStatus: config.PriceStatusUnknown,
		BillingUnit: config.BillingUnitTokens,
	}
	estUnknown := EstimateCost(CostRequest{
		Policy:      policyUnknown,
		Measurement: m,
	})
	if estUnknown.Known || estUnknown.AmountKnown {
		t.Fatalf("unknown price status should not produce known amount: %+v", estUnknown)
	}
	if estUnknown.Status != StatusUnknownPrice {
		t.Errorf("expected status %s, got %s", StatusUnknownPrice, estUnknown.Status)
	}
}

func TestMissingPromptOrCompletionPrice(t *testing.T) {
	// Paid policy where prompt price was never configured (PromptConfigured=false, PromptPer1M=0)
	missingPromptPolicy := &config.ModelCostPolicy{
		PriceStatus:          config.PriceStatusPaidConfirmed,
		BillingUnit:          config.BillingUnitTokens,
		CompletionPer1M:      5.0,
		CompletionConfigured: true,
	}
	m := FromTokens(100, 50, 0, 0, 0)
	est := EstimateCost(CostRequest{
		Policy:      missingPromptPolicy,
		Measurement: m,
	})
	if est.Status != StatusUnknownPrice {
		t.Fatalf("expected status %s for missing prompt price, got %s", StatusUnknownPrice, est.Status)
	}
	if est.Reason != ReasonMissingRequiredPrice {
		t.Errorf("expected reason %s, got %s", ReasonMissingRequiredPrice, est.Reason)
	}

	// Paid policy where completion price was never configured (CompletionConfigured=false, CompletionPer1M=0)
	missingCompletionPolicy := &config.ModelCostPolicy{
		PriceStatus:      config.PriceStatusPaidConfirmed,
		BillingUnit:      config.BillingUnitTokens,
		PromptPer1M:      2.0,
		PromptConfigured: true,
	}
	estComp := EstimateCost(CostRequest{
		Policy:      missingCompletionPolicy,
		Measurement: m,
	})
	if estComp.Status != StatusUnknownPrice {
		t.Fatalf("expected status %s for missing completion price, got %s", StatusUnknownPrice, estComp.Status)
	}
	if estComp.Reason != ReasonMissingRequiredPrice {
		t.Errorf("expected reason %s, got %s", ReasonMissingRequiredPrice, estComp.Reason)
	}
}

func TestExplicitZeroPromptWithPaidCompletion(t *testing.T) {
	// Explicitly configured 0 prompt price (PromptConfigured=true, PromptPer1M=0)
	freeInputPaidOutput := &config.ModelCostPolicy{
		PriceStatus:          config.PriceStatusPaidConfirmed,
		BillingUnit:          config.BillingUnitTokens,
		PromptPer1M:          0,
		PromptConfigured:     true,
		CompletionPer1M:      4.0,
		CompletionConfigured: true,
	}
	m := FromTokens(500_000, 250_000, 0, 0, 0)
	est := EstimateCost(CostRequest{
		Policy:      freeInputPaidOutput,
		Measurement: m,
	})
	if !est.Known || !est.AmountKnown {
		t.Fatalf("expected known cost: %+v", est)
	}
	// 500k prompt @ $0 = $0; 250k completion @ $4/1M = $1.00 = 1,000,000,000 nanos
	if est.AmountNanos != 1_000_000_000 {
		t.Errorf("expected 1,000,000,000 nanos, got %d", est.AmountNanos)
	}
	if est.Amount != 1.00 {
		t.Errorf("expected 1.00 USD, got %v", est.Amount)
	}
}

func TestNegativeTokensRejected(t *testing.T) {
	policy := &config.ModelCostPolicy{
		PriceStatus: config.PriceStatusPaidConfirmed,
		BillingUnit: config.BillingUnitTokens,
		PromptPer1M: 1.0,
	}
	m := FromTokens(-10, 50, 0, 0, 0)
	est := EstimateCost(CostRequest{
		Policy:      policy,
		Measurement: m,
	})

	if est.Known || est.AmountKnown {
		t.Fatalf("negative tokens must not produce known cost: %+v", est)
	}
	if est.Status != StatusInvalidMeasurement {
		t.Errorf("expected status %s, got %s", StatusInvalidMeasurement, est.Status)
	}
}

func TestNaNAndInfPricesRejected(t *testing.T) {
	nanPolicy := &config.ModelCostPolicy{
		PriceStatus: config.PriceStatusPaidConfirmed,
		BillingUnit: config.BillingUnitTokens,
		PromptPer1M: math.NaN(),
	}
	m := FromTokens(100, 100, 0, 0, 0)
	est := EstimateCost(CostRequest{
		Policy:      nanPolicy,
		Measurement: m,
	})
	if est.Status != StatusInvalidPolicy {
		t.Errorf("expected status %s for NaN price, got %s", StatusInvalidPolicy, est.Status)
	}

	infPolicy := &config.ModelCostPolicy{
		PriceStatus: config.PriceStatusPaidConfirmed,
		BillingUnit: config.BillingUnitTokens,
		PromptPer1M: math.Inf(1),
	}
	estInf := EstimateCost(CostRequest{
		Policy:      infPolicy,
		Measurement: m,
	})
	if estInf.Status != StatusInvalidPolicy {
		t.Errorf("expected status %s for Inf price, got %s", StatusInvalidPolicy, estInf.Status)
	}
}

func TestAudioSecondsCalculation(t *testing.T) {
	policy := &config.ModelCostPolicy{
		PriceStatus: config.PriceStatusPaidConfirmed,
		BillingUnit: config.BillingUnitAudioSeconds,
		UnitPrice:   0.006, // $0.006 per second ($0.36 per minute)
	}

	// 1. Normal seconds (60 seconds -> $0.36 = 360,000,000 nanos)
	m := FromAudioSeconds(60.0)
	est := EstimateCost(CostRequest{
		Policy:      policy,
		Measurement: m,
	})
	if !est.Known || !est.AmountKnown {
		t.Fatalf("expected known cost: %+v", est)
	}
	if est.AmountNanos != 360_000_000 {
		t.Errorf("expected 360,000,000 nanos, got %d", est.AmountNanos)
	}
	if math.Abs(est.Amount-0.36) > 1e-9 {
		t.Errorf("expected 0.36 USD, got %v", est.Amount)
	}

	// 2. Missing audio seconds
	mEmpty := Measurement{}
	estEmpty := EstimateCost(CostRequest{
		Policy:      policy,
		Measurement: mEmpty,
	})
	if estEmpty.Known || estEmpty.AmountKnown {
		t.Fatalf("missing audio seconds must not produce known cost: %+v", estEmpty)
	}
	if estEmpty.Status != StatusMissingQuantity {
		t.Errorf("expected status %s, got %s", StatusMissingQuantity, estEmpty.Status)
	}

	// 3. Negative audio seconds
	mNeg := FromAudioSeconds(-5.0)
	estNeg := EstimateCost(CostRequest{
		Policy:      policy,
		Measurement: mNeg,
	})
	if estNeg.Status != StatusInvalidMeasurement {
		t.Errorf("expected status %s for negative seconds, got %s", StatusInvalidMeasurement, estNeg.Status)
	}
}

func TestTTSCharactersCalculation(t *testing.T) {
	policy := &config.ModelCostPolicy{
		PriceStatus: config.PriceStatusPaidConfirmed,
		BillingUnit: config.BillingUnitCharacters,
		UnitPrice:   0.000015, // $15 per 1M characters
	}

	// Test 1: Chinese characters: "你好，世界！" (6 Unicode characters, but 18 UTF-8 bytes)
	chineseText := "你好，世界！"
	if utf8.RuneCountInString(chineseText) != 6 {
		t.Fatalf("sanity check failed: expected 6 runes, got %d", utf8.RuneCountInString(chineseText))
	}
	if len(chineseText) == 6 {
		t.Fatal("byte count should not equal rune count for Chinese text")
	}

	mChinese := FromTextCharacters(chineseText)
	if mChinese.Characters != 6 {
		t.Fatalf("expected 6 runes in measurement, got %d", mChinese.Characters)
	}
	estChinese := EstimateCost(CostRequest{
		Policy:      policy,
		Measurement: mChinese,
	})
	if !estChinese.Known || !estChinese.AmountKnown {
		t.Fatalf("expected known cost: %+v", estChinese)
	}
	if estChinese.Quantity != 6 {
		t.Errorf("expected quantity 6, got %v", estChinese.Quantity)
	}
	// 6 * 0.000015 = 0.000090 USD = 90,000 nanos
	if estChinese.AmountNanos != 90_000 {
		t.Errorf("expected 90,000 nanos, got %d", estChinese.AmountNanos)
	}

	// Test 2: Emoji and unicode combining characters: "👨‍👩‍👧‍👦" (family emoji: 7 runes) and "🎉🚀" (2 runes)
	emojiText := "🎉🚀"
	mEmoji := FromTextCharacters(emojiText)
	if mEmoji.Characters != 2 {
		t.Fatalf("expected 2 runes, got %d", mEmoji.Characters)
	}
	if len(emojiText) == 2 {
		t.Fatal("byte count should not equal rune count for emoji")
	}

	// Test 3: Missing characters measurement
	mEmpty := Measurement{}
	estEmpty := EstimateCost(CostRequest{
		Policy:      policy,
		Measurement: mEmpty,
	})
	if estEmpty.Status != StatusMissingQuantity {
		t.Errorf("expected %s for missing characters, got %s", StatusMissingQuantity, estEmpty.Status)
	}

	// Test 4: Legitimate empty text (0 characters)
	mZeroChars := FromCharacterCount(0)
	estZero := EstimateCost(CostRequest{
		Policy:      policy,
		Measurement: mZeroChars,
	})
	if !estZero.Known || !estZero.AmountKnown {
		t.Fatalf("expected known amount for legitimate 0 characters: %+v", estZero)
	}
	if estZero.Amount != 0 || estZero.AmountNanos != 0 {
		t.Errorf("expected 0 cost, got %v (%d nanos)", estZero.Amount, estZero.AmountNanos)
	}
	if estZero.Quantity != 0 {
		t.Errorf("expected quantity 0, got %v", estZero.Quantity)
	}
}

func TestImageCountCalculation(t *testing.T) {
	policy := &config.ModelCostPolicy{
		PriceStatus: config.PriceStatusPaidConfirmed,
		BillingUnit: config.BillingUnitImages,
		UnitPrice:   0.04, // $0.04 per image
	}

	// 1. Single image (count=1 -> $0.04 = 40,000,000 nanos)
	m1 := FromImageCount(1)
	est1 := EstimateCost(CostRequest{
		Policy:      policy,
		Measurement: m1,
	})
	if !est1.Known || est1.AmountNanos != 40_000_000 {
		t.Errorf("expected 40,000,000 nanos for 1 image, got %d", est1.AmountNanos)
	}

	// 2. Multiple images (count=4 -> $0.16 = 160,000,000 nanos)
	m4 := FromImageCount(4)
	est4 := EstimateCost(CostRequest{
		Policy:      policy,
		Measurement: m4,
	})
	if !est4.Known || est4.AmountNanos != 160_000_000 {
		t.Errorf("expected 160,000,000 nanos for 4 images, got %d", est4.AmountNanos)
	}

	// 3. Missing image count
	mMissing := Measurement{}
	estMissing := EstimateCost(CostRequest{
		Policy:      policy,
		Measurement: mMissing,
	})
	if estMissing.Status != StatusMissingQuantity {
		t.Errorf("expected %s for missing image count, got %s", StatusMissingQuantity, estMissing.Status)
	}
}

func TestVideoSubmitAndStatus(t *testing.T) {
	policy := &config.ModelCostPolicy{
		PriceStatus: config.PriceStatusPaidConfirmed,
		BillingUnit: config.BillingUnitVideoTasks,
		UnitPrice:   0.50, // $0.50 per task
	}

	// 1. Video submit: 1 task -> $0.50 = 500,000,000 nanos
	mSubmit := FromVideoSubmit(1)
	estSubmit := EstimateCost(CostRequest{
		Policy:      policy,
		Measurement: mSubmit,
	})
	if !estSubmit.Known || estSubmit.AmountNanos != 500_000_000 {
		t.Errorf("expected 500,000,000 nanos for video submit, got %d", estSubmit.AmountNanos)
	}
	if estSubmit.BillingUnit != config.BillingUnitVideoTasks {
		t.Errorf("expected billing unit %s, got %s", config.BillingUnitVideoTasks, estSubmit.BillingUnit)
	}

	// 2. Video status query: must not be billed as video task! Must produce zero cost without error.
	mStatus := FromVideoStatus()
	estStatus := EstimateCost(CostRequest{
		Policy:      policy,
		Measurement: mStatus,
	})
	if !estStatus.Known || !estStatus.AmountKnown {
		t.Fatalf("expected known 0 cost for video status: %+v", estStatus)
	}
	if estStatus.Amount != 0 || estStatus.AmountNanos != 0 {
		t.Errorf("expected 0 cost for video status query, got %v (%d nanos)", estStatus.Amount, estStatus.AmountNanos)
	}
	if estStatus.BillingUnit != config.BillingUnitVideoStatus {
		t.Errorf("expected billing unit %s, got %s", config.BillingUnitVideoStatus, estStatus.BillingUnit)
	}
	if estStatus.Reason != ReasonStatusQueryNotBilled {
		t.Errorf("expected reason %s, got %s", ReasonStatusQueryNotBilled, estStatus.Reason)
	}
}

func TestFrozenPriceStatus(t *testing.T) {
	policy := &config.ModelCostPolicy{
		PriceStatus: config.PriceStatusFrozen,
		BillingUnit: config.BillingUnitTokens,
	}
	m := FromTokens(100, 100, 0, 0, 0)
	est := EstimateCost(CostRequest{
		Policy:      policy,
		Measurement: m,
	})
	if est.Status != StatusFrozen {
		t.Errorf("expected status %s for frozen model, got %s", StatusFrozen, est.Status)
	}
	if est.Known || est.AmountKnown {
		t.Fatalf("frozen model must not produce normal known amount: %+v", est)
	}
}

func TestUnsupportedBillingUnit(t *testing.T) {
	policy := &config.ModelCostPolicy{
		PriceStatus: config.PriceStatusPaidConfirmed,
		BillingUnit: "custom_unsupported_unit",
	}
	m := FromTokens(100, 100, 0, 0, 0)
	est := EstimateCost(CostRequest{
		Policy:      policy,
		Measurement: m,
	})
	if est.Status != StatusUnsupportedBillingUnit {
		t.Errorf("expected status %s, got %s", StatusUnsupportedBillingUnit, est.Status)
	}
	if est.Known || est.AmountKnown {
		t.Fatalf("unsupported billing unit must not produce known cost: %+v", est)
	}
}

func TestLargeTokenCountOverflowProtection(t *testing.T) {
	policy := &config.ModelCostPolicy{
		PriceStatus: config.PriceStatusPaidConfirmed,
		BillingUnit: config.BillingUnitTokens,
		PromptPer1M: 1000.0, // expensive model
	}
	// 100 billion tokens (100,000,000,000 tokens)
	largeTokens := int64(100_000_000_000)
	m := FromTokens(largeTokens, 0, 0, 0, 0)
	est := EstimateCost(CostRequest{
		Policy:      policy,
		Measurement: m,
	})
	if !est.Known || !est.AmountKnown {
		t.Fatalf("expected calculation to succeed without overflow: %+v", est)
	}
	// 100B tokens * $1000 / 1M = $100,000,000 USD = 100,000,000,000,000,000 nanos
	expectedNanos := int64(100_000_000_000_000_000)
	if est.AmountNanos != expectedNanos {
		t.Errorf("expected %d nanos, got %d", expectedNanos, est.AmountNanos)
	}
}

func TestSmallAmountBudgetPrecision(t *testing.T) {
	policy := &config.ModelCostPolicy{
		PriceStatus: config.PriceStatusPaidConfirmed,
		BillingUnit: config.BillingUnitTokens,
		PromptPer1M: 0.15, // $0.15 / 1M = 150 nanos per token
	}
	m := FromTokens(1, 0, 0, 0, 0)
	est := EstimateCost(CostRequest{
		Policy:      policy,
		Measurement: m,
	})
	if est.AmountNanos != 150 {
		t.Errorf("expected 150 nanos for 1 token at $0.15/1M, got %d", est.AmountNanos)
	}

	// Budget boundary check using deterministic methods:
	// Limit of $0.00000014 (140 nanos) should be exceeded by 150 nanos
	if !est.ExceedsAmount(0.00000014) {
		t.Error("expected 150 nanos to exceed 140 nanos limit")
	}
	// Limit of $0.00000015 (150 nanos) should NOT be exceeded
	if est.ExceedsAmount(0.00000015) {
		t.Error("expected 150 nanos to not exceed 150 nanos limit")
	}
	// Limit of $0.00000016 (160 nanos) should NOT be exceeded
	if est.ExceedsAmount(0.00000016) {
		t.Error("expected 150 nanos to not exceed 160 nanos limit")
	}
}

func TestExplicitZeroDifferentFromUnknown(t *testing.T) {
	freePolicy := &config.ModelCostPolicy{
		PriceStatus: config.PriceStatusFreeConfirmed,
		BillingUnit: config.BillingUnitTokens,
		PromptPer1M: 0,
	}
	m := FromTokens(100, 100, 0, 0, 0)
	estFree := EstimateCost(CostRequest{
		Policy:      freePolicy,
		Measurement: m,
	})

	estUnknown := EstimateCost(CostRequest{
		Policy:      nil,
		Measurement: m,
	})

	if estFree.Status == estUnknown.Status {
		t.Fatalf("free status (%s) should not equal unknown status (%s)", estFree.Status, estUnknown.Status)
	}
	if estFree.AmountKnown == estUnknown.AmountKnown {
		t.Fatalf("free AmountKnown (%v) should not equal unknown AmountKnown (%v)", estFree.AmountKnown, estUnknown.AmountKnown)
	}
	if estFree.ExplicitlyFree == estUnknown.ExplicitlyFree {
		t.Fatalf("free ExplicitlyFree (%v) should not equal unknown ExplicitlyFree (%v)", estFree.ExplicitlyFree, estUnknown.ExplicitlyFree)
	}
}

func TestMissingQuantityDifferentFromValidZeroQuantity(t *testing.T) {
	policy := &config.ModelCostPolicy{
		PriceStatus: config.PriceStatusPaidConfirmed,
		BillingUnit: config.BillingUnitCharacters,
		UnitPrice:   0.0001,
	}

	// Legitimate zero characters (e.g. Empty prompt)
	mValidZero := FromCharacterCount(0)
	estZero := EstimateCost(CostRequest{
		Policy:      policy,
		Measurement: mValidZero,
	})

	// Missing character measurement
	mMissing := Measurement{}
	estMissing := EstimateCost(CostRequest{
		Policy:      policy,
		Measurement: mMissing,
	})

	if estZero.Status == estMissing.Status {
		t.Fatalf("valid zero status (%s) should not equal missing quantity status (%s)", estZero.Status, estMissing.Status)
	}
	if !estZero.AmountKnown {
		t.Errorf("valid zero quantity should produce AmountKnown=true")
	}
	if estMissing.AmountKnown {
		t.Errorf("missing quantity must produce AmountKnown=false")
	}
}
