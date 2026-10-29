package cost

import (
	"math"
	"math/big"
	"strings"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// Calculation status contract constants.
const (
	StatusKnown                  = "known"
	StatusExplicitFree           = "explicit_free"
	StatusUnknownPrice           = "unknown_price"
	StatusMissingQuantity        = "missing_quantity"
	StatusUnsupportedBillingUnit = "unsupported_billing_unit"
	StatusInvalidPolicy          = "invalid_policy"
	StatusInvalidMeasurement     = "invalid_measurement"
	StatusFrozen                 = "frozen"
)

// Calculation reason constants.
const (
	ReasonCalculated              = "calculated"
	ReasonExplicitZeroPrice       = "explicit_zero_price"
	ReasonMissingModelPricePolicy = "missing_model_price_policy"
	ReasonPolicyPriceUnknown      = "policy_price_status_unknown"
	ReasonMeasurementRequired     = "measurement_required"
	ReasonStatusQueryNotBilled    = "status_query_not_billed"
	ReasonModelPriceFrozen        = "model_price_frozen"
	ReasonNegativeTokenCount      = "negative_token_count"
	ReasonNegativeQuantity        = "negative_quantity"
	ReasonNonFinitePrice          = "non_finite_price"
	ReasonUnsupportedBillingUnit  = "unsupported_billing_unit"
	ReasonMissingRequiredPrice    = "missing_required_price"
	ReasonTokenOverflow           = "token_count_overflow"
)

// NanosPerUSD is the scaling factor from USD dollars to nanodollars ($1 = 10^9 nanos).
const NanosPerUSD int64 = 1_000_000_000

// TokenUsage encapsulates mutually exclusive token counts.
type TokenUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	CacheTokens         int64 `json:"cache_tokens"`
	CacheReadTokens     int64 `json:"cache_read_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_tokens"`
}

// Total returns the total non-overlapping tokens.
func (t TokenUsage) Total() int64 {
	return t.PromptTokens + t.CompletionTokens + t.CacheTokens + t.CacheReadTokens + t.CacheCreationTokens
}

// Measurement contains request-level consumption metrics across token and non-token modalities.
type Measurement struct {
	Tokens              *TokenUsage `json:"tokens,omitempty"`
	TokensPresent       bool        `json:"tokens_present"`
	AudioSeconds        float64     `json:"audio_seconds,omitempty"`
	AudioSecondsPresent bool        `json:"audio_seconds_present"`
	Characters          int64       `json:"characters,omitempty"`
	CharactersPresent   bool        `json:"characters_present"`
	ImageCount          int64       `json:"image_count,omitempty"`
	ImageCountPresent   bool        `json:"image_count_present"`
	VideoTasks          int64       `json:"video_tasks,omitempty"`
	VideoTasksPresent   bool        `json:"video_tasks_present"`
	IsVideoStatus       bool        `json:"is_video_status,omitempty"`
	Quantity            float64     `json:"quantity,omitempty"`
	QuantityPresent     bool        `json:"quantity_present"`
}

// FromTokens creates a token measurement.
func FromTokens(prompt, completion, cache, cacheRead, cacheCreation int64) Measurement {
	return Measurement{
		Tokens: &TokenUsage{
			PromptTokens:        prompt,
			CompletionTokens:    completion,
			CacheTokens:         cache,
			CacheReadTokens:     cacheRead,
			CacheCreationTokens: cacheCreation,
		},
		TokensPresent: true,
	}
}

// FromAudioSeconds creates an audio duration measurement.
func FromAudioSeconds(seconds float64) Measurement {
	return Measurement{
		AudioSeconds:        seconds,
		AudioSecondsPresent: true,
	}
}

// FromTextCharacters creates a character measurement from text counting Unicode runes.
func FromTextCharacters(text string) Measurement {
	return Measurement{
		Characters:        int64(utf8.RuneCountInString(text)),
		CharactersPresent: true,
	}
}

// FromCharacterCount creates an explicit character measurement.
func FromCharacterCount(chars int64) Measurement {
	return Measurement{
		Characters:        chars,
		CharactersPresent: true,
	}
}

// FromImageCount creates an image quantity measurement.
func FromImageCount(count int64) Measurement {
	return Measurement{
		ImageCount:        count,
		ImageCountPresent: true,
	}
}

// FromVideoSubmit creates a video generation task measurement.
func FromVideoSubmit(tasks int64) Measurement {
	return Measurement{
		VideoTasks:        tasks,
		VideoTasksPresent: true,
	}
}

// FromVideoStatus creates a video status query measurement.
func FromVideoStatus() Measurement {
	return Measurement{
		IsVideoStatus: true,
	}
}

// CostRequest encapsulates the inputs for pure cost estimation.
type CostRequest struct {
	Provider    string                  `json:"provider,omitempty"`
	Model       string                  `json:"model,omitempty"`
	Policy      *config.ModelCostPolicy `json:"policy,omitempty"`
	Measurement Measurement             `json:"measurement"`
	BillingUnit string                  `json:"billing_unit,omitempty"`
	Currency    string                  `json:"currency,omitempty"`
}

// CostEstimate captures the calculated monetary cost and audit metadata.
type CostEstimate struct {
	Currency       string  `json:"currency"`
	Amount         float64 `json:"amount"`
	AmountNanos    int64   `json:"amount_nanos"`
	AmountKnown    bool    `json:"amount_known"`
	BillingUnit    string  `json:"billing_unit"`
	Quantity       float64 `json:"quantity"`
	QuantityKnown  bool    `json:"quantity_known"`
	Status         string  `json:"status"`
	Reason         string  `json:"reason"`
	PriceStatus    string  `json:"price_status"`
	Provider       string  `json:"provider"`
	Model          string  `json:"model"`
	ExplicitlyFree bool    `json:"explicitly_free"`
	Known          bool    `json:"known"`
}

// ExceedsAmount deterministically checks if the cost exceeds the given limit in USD using nanodollars.
func (c CostEstimate) ExceedsAmount(limit float64) bool {
	if !c.AmountKnown || limit < 0 || math.IsNaN(limit) || math.IsInf(limit, 0) {
		return false
	}
	limitNanos := int64(math.Round(limit * float64(NanosPerUSD)))
	return c.AmountNanos > limitNanos
}

// ExceedsAmountNanos deterministically checks if the cost exceeds the limit in nanodollars.
func (c CostEstimate) ExceedsAmountNanos(limitNanos int64) bool {
	if !c.AmountKnown || limitNanos < 0 {
		return false
	}
	return c.AmountNanos > limitNanos
}

// EstimateCost evaluates the monetary cost of a request purely based on policy and measurement.
func EstimateCost(req CostRequest) CostEstimate {
	currency := strings.TrimSpace(req.Currency)
	if currency == "" {
		currency = config.CurrencyUSD
	}

	est := CostEstimate{
		Currency:    currency,
		Provider:    req.Provider,
		Model:       req.Model,
		PriceStatus: config.PriceStatusUnknown,
	}

	// Handle video status queries: guaranteed 0-cost non-billed request
	if req.Measurement.IsVideoStatus {
		est.BillingUnit = config.BillingUnitVideoStatus
		est.Quantity = 0
		est.QuantityKnown = true
		est.Amount = 0
		est.AmountNanos = 0
		est.AmountKnown = true
		est.ExplicitlyFree = true
		est.Known = true
		est.Status = StatusExplicitFree
		est.Reason = ReasonStatusQueryNotBilled
		if req.Policy != nil && req.Policy.PriceStatus != "" {
			est.PriceStatus = req.Policy.PriceStatus
		} else {
			est.PriceStatus = config.PriceStatusFreeConfirmed
		}
		return est
	}

	// Verify policy existence
	if req.Policy == nil {
		est.Status = StatusUnknownPrice
		est.Reason = ReasonMissingModelPricePolicy
		return est
	}

	policy := req.Policy
	est.PriceStatus = policy.PriceStatus

	// Verify frozen status
	if policy.PriceStatus == config.PriceStatusFrozen {
		est.Status = StatusFrozen
		est.Reason = ReasonModelPriceFrozen
		return est
	}

	// Verify unknown status
	if policy.PriceStatus == config.PriceStatusUnknown || policy.PriceStatus == "" {
		est.Status = StatusUnknownPrice
		est.Reason = ReasonPolicyPriceUnknown
		return est
	}

	// Resolve effective billing unit
	unit := strings.TrimSpace(req.BillingUnit)
	if unit == "" {
		unit = strings.TrimSpace(policy.BillingUnit)
	}
	if unit == "" {
		unit = config.BillingUnitTokens
	}
	est.BillingUnit = unit

	// Check supported billing unit
	switch unit {
	case config.BillingUnitTokens,
		config.BillingUnitAudioSeconds,
		config.BillingUnitCharacters,
		config.BillingUnitImages,
		config.BillingUnitVideoTasks,
		config.BillingUnitVideoStatus:
		// Valid
	default:
		est.Status = StatusUnsupportedBillingUnit
		est.Reason = ReasonUnsupportedBillingUnit
		return est
	}

	// Validate policy numerical constraints (NaN, Inf, negative)
	if !isFiniteNonNegative(policy.PromptPer1M) ||
		!isFiniteNonNegative(policy.CompletionPer1M) ||
		!isFiniteNonNegative(policy.CachePer1M) ||
		!isFiniteNonNegative(policy.CacheReadPer1M) ||
		!isFiniteNonNegative(policy.CacheCreationPer1M) ||
		!isFiniteNonNegative(policy.UnitPrice) {
		est.Status = StatusInvalidPolicy
		est.Reason = ReasonNonFinitePrice
		return est
	}

	// Route by billing unit
	switch unit {
	case config.BillingUnitTokens:
		return estimateTokens(policy, req.Measurement, est)
	case config.BillingUnitAudioSeconds:
		return estimateAudioSeconds(policy, req.Measurement, est)
	case config.BillingUnitCharacters:
		return estimateCharacters(policy, req.Measurement, est)
	case config.BillingUnitImages:
		return estimateImages(policy, req.Measurement, est)
	case config.BillingUnitVideoTasks:
		return estimateVideoTasks(policy, req.Measurement, est)
	case config.BillingUnitVideoStatus:
		est.Quantity = 0
		est.QuantityKnown = true
		est.Amount = 0
		est.AmountNanos = 0
		est.AmountKnown = true
		est.ExplicitlyFree = true
		est.Known = true
		est.Status = StatusExplicitFree
		est.Reason = ReasonStatusQueryNotBilled
		return est
	default:
		est.Status = StatusUnsupportedBillingUnit
		est.Reason = ReasonUnsupportedBillingUnit
		return est
	}
}

func isFiniteNonNegative(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0
}

func estimateTokens(policy *config.ModelCostPolicy, m Measurement, est CostEstimate) CostEstimate {
	if !m.TokensPresent || m.Tokens == nil {
		est.Status = StatusMissingQuantity
		est.Reason = ReasonMeasurementRequired
		return est
	}

	t := m.Tokens
	if t.PromptTokens < 0 || t.CompletionTokens < 0 || t.CacheTokens < 0 || t.CacheReadTokens < 0 || t.CacheCreationTokens < 0 {
		est.Status = StatusInvalidMeasurement
		est.Reason = ReasonNegativeTokenCount
		return est
	}

	totalTokens := t.Total()
	est.Quantity = float64(totalTokens)
	est.QuantityKnown = true

	// Handle explicitly confirmed free model
	if policy.PriceStatus == config.PriceStatusFreeConfirmed {
		est.Amount = 0
		est.AmountNanos = 0
		est.AmountKnown = true
		est.ExplicitlyFree = true
		est.Known = true
		est.Status = StatusExplicitFree
		est.Reason = ReasonExplicitZeroPrice
		return est
	}

	// For paid_confirmed, check whether required token prices are configured
	if t.PromptTokens > 0 && !policy.IsPromptConfigured() {
		est.Status = StatusUnknownPrice
		est.Reason = ReasonMissingRequiredPrice
		return est
	}
	if t.CompletionTokens > 0 && !policy.IsCompletionConfigured() {
		est.Status = StatusUnknownPrice
		est.Reason = ReasonMissingRequiredPrice
		return est
	}

	// Compute token cost using rational arithmetic
	costRat := new(big.Rat)
	oneMillionRat := new(big.Rat).SetInt64(1_000_000)

	type tokenPart struct {
		tokens int64
		price  float64
	}
	parts := []tokenPart{
		{t.PromptTokens, policy.PromptPer1M},
		{t.CompletionTokens, policy.CompletionPer1M},
		{t.CacheTokens, policy.CachePer1M},
		{t.CacheReadTokens, policy.CacheReadPer1M},
		{t.CacheCreationTokens, policy.CacheCreationPer1M},
	}

	for _, p := range parts {
		if p.tokens == 0 || p.price == 0 {
			continue
		}
		tokRat := new(big.Rat).SetInt64(p.tokens)
		priceRat := new(big.Rat).SetFloat64(p.price)
		partCost := new(big.Rat).Mul(tokRat, priceRat)
		partCost.Quo(partCost, oneMillionRat)
		costRat.Add(costRat, partCost)
	}

	amount, _ := costRat.Float64()
	nanosRat := new(big.Rat).Mul(costRat, new(big.Rat).SetInt64(NanosPerUSD))
	nanosInt := roundRatToInt(nanosRat)

	if !nanosInt.IsInt64() {
		est.Status = StatusInvalidMeasurement
		est.Reason = ReasonTokenOverflow
		return est
	}

	est.Amount = amount
	est.AmountNanos = nanosInt.Int64()
	est.AmountKnown = true
	est.Known = true
	est.Status = StatusKnown
	est.Reason = ReasonCalculated
	return est
}

func estimateAudioSeconds(policy *config.ModelCostPolicy, m Measurement, est CostEstimate) CostEstimate {
	var seconds float64
	if m.AudioSecondsPresent {
		seconds = m.AudioSeconds
	} else if m.QuantityPresent {
		seconds = m.Quantity
	} else {
		est.Status = StatusMissingQuantity
		est.Reason = ReasonMeasurementRequired
		return est
	}

	if seconds < 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		est.Status = StatusInvalidMeasurement
		est.Reason = ReasonNegativeQuantity
		return est
	}

	est.Quantity = seconds
	est.QuantityKnown = true

	if policy.PriceStatus == config.PriceStatusFreeConfirmed {
		est.Amount = 0
		est.AmountNanos = 0
		est.AmountKnown = true
		est.ExplicitlyFree = true
		est.Known = true
		est.Status = StatusExplicitFree
		est.Reason = ReasonExplicitZeroPrice
		return est
	}

	if seconds > 0 && !policy.IsUnitPriceConfigured() {
		est.Status = StatusUnknownPrice
		est.Reason = ReasonMissingRequiredPrice
		return est
	}

	costRat := new(big.Rat).Mul(new(big.Rat).SetFloat64(seconds), new(big.Rat).SetFloat64(policy.UnitPrice))
	amount, _ := costRat.Float64()
	nanosRat := new(big.Rat).Mul(costRat, new(big.Rat).SetInt64(NanosPerUSD))
	nanosInt := roundRatToInt(nanosRat)

	if !nanosInt.IsInt64() {
		est.Status = StatusInvalidMeasurement
		est.Reason = ReasonTokenOverflow
		return est
	}

	est.Amount = amount
	est.AmountNanos = nanosInt.Int64()
	est.AmountKnown = true
	est.Known = true
	est.Status = StatusKnown
	est.Reason = ReasonCalculated
	return est
}

func estimateCharacters(policy *config.ModelCostPolicy, m Measurement, est CostEstimate) CostEstimate {
	var chars int64
	if m.CharactersPresent {
		chars = m.Characters
	} else if m.QuantityPresent {
		chars = int64(m.Quantity)
	} else {
		est.Status = StatusMissingQuantity
		est.Reason = ReasonMeasurementRequired
		return est
	}

	if chars < 0 {
		est.Status = StatusInvalidMeasurement
		est.Reason = ReasonNegativeQuantity
		return est
	}

	est.Quantity = float64(chars)
	est.QuantityKnown = true

	if policy.PriceStatus == config.PriceStatusFreeConfirmed {
		est.Amount = 0
		est.AmountNanos = 0
		est.AmountKnown = true
		est.ExplicitlyFree = true
		est.Known = true
		est.Status = StatusExplicitFree
		est.Reason = ReasonExplicitZeroPrice
		return est
	}

	if chars > 0 && !policy.IsUnitPriceConfigured() {
		est.Status = StatusUnknownPrice
		est.Reason = ReasonMissingRequiredPrice
		return est
	}

	costRat := new(big.Rat).Mul(new(big.Rat).SetInt64(chars), new(big.Rat).SetFloat64(policy.UnitPrice))
	amount, _ := costRat.Float64()
	nanosRat := new(big.Rat).Mul(costRat, new(big.Rat).SetInt64(NanosPerUSD))
	nanosInt := roundRatToInt(nanosRat)

	if !nanosInt.IsInt64() {
		est.Status = StatusInvalidMeasurement
		est.Reason = ReasonTokenOverflow
		return est
	}

	est.Amount = amount
	est.AmountNanos = nanosInt.Int64()
	est.AmountKnown = true
	est.Known = true
	est.Status = StatusKnown
	est.Reason = ReasonCalculated
	return est
}

func estimateImages(policy *config.ModelCostPolicy, m Measurement, est CostEstimate) CostEstimate {
	var images int64
	if m.ImageCountPresent {
		images = m.ImageCount
	} else if m.QuantityPresent {
		images = int64(m.Quantity)
	} else {
		est.Status = StatusMissingQuantity
		est.Reason = ReasonMeasurementRequired
		return est
	}

	if images < 0 {
		est.Status = StatusInvalidMeasurement
		est.Reason = ReasonNegativeQuantity
		return est
	}

	est.Quantity = float64(images)
	est.QuantityKnown = true

	if policy.PriceStatus == config.PriceStatusFreeConfirmed {
		est.Amount = 0
		est.AmountNanos = 0
		est.AmountKnown = true
		est.ExplicitlyFree = true
		est.Known = true
		est.Status = StatusExplicitFree
		est.Reason = ReasonExplicitZeroPrice
		return est
	}

	if images > 0 && !policy.IsUnitPriceConfigured() {
		est.Status = StatusUnknownPrice
		est.Reason = ReasonMissingRequiredPrice
		return est
	}

	costRat := new(big.Rat).Mul(new(big.Rat).SetInt64(images), new(big.Rat).SetFloat64(policy.UnitPrice))
	amount, _ := costRat.Float64()
	nanosRat := new(big.Rat).Mul(costRat, new(big.Rat).SetInt64(NanosPerUSD))
	nanosInt := roundRatToInt(nanosRat)

	if !nanosInt.IsInt64() {
		est.Status = StatusInvalidMeasurement
		est.Reason = ReasonTokenOverflow
		return est
	}

	est.Amount = amount
	est.AmountNanos = nanosInt.Int64()
	est.AmountKnown = true
	est.Known = true
	est.Status = StatusKnown
	est.Reason = ReasonCalculated
	return est
}

func estimateVideoTasks(policy *config.ModelCostPolicy, m Measurement, est CostEstimate) CostEstimate {
	var tasks int64
	if m.VideoTasksPresent {
		tasks = m.VideoTasks
	} else if m.QuantityPresent {
		tasks = int64(m.Quantity)
	} else {
		est.Status = StatusMissingQuantity
		est.Reason = ReasonMeasurementRequired
		return est
	}

	if tasks < 0 {
		est.Status = StatusInvalidMeasurement
		est.Reason = ReasonNegativeQuantity
		return est
	}

	est.Quantity = float64(tasks)
	est.QuantityKnown = true

	if policy.PriceStatus == config.PriceStatusFreeConfirmed {
		est.Amount = 0
		est.AmountNanos = 0
		est.AmountKnown = true
		est.ExplicitlyFree = true
		est.Known = true
		est.Status = StatusExplicitFree
		est.Reason = ReasonExplicitZeroPrice
		return est
	}

	if tasks > 0 && !policy.IsUnitPriceConfigured() {
		est.Status = StatusUnknownPrice
		est.Reason = ReasonMissingRequiredPrice
		return est
	}

	costRat := new(big.Rat).Mul(new(big.Rat).SetInt64(tasks), new(big.Rat).SetFloat64(policy.UnitPrice))
	amount, _ := costRat.Float64()
	nanosRat := new(big.Rat).Mul(costRat, new(big.Rat).SetInt64(NanosPerUSD))
	nanosInt := roundRatToInt(nanosRat)

	if !nanosInt.IsInt64() {
		est.Status = StatusInvalidMeasurement
		est.Reason = ReasonTokenOverflow
		return est
	}

	est.Amount = amount
	est.AmountNanos = nanosInt.Int64()
	est.AmountKnown = true
	est.Known = true
	est.Status = StatusKnown
	est.Reason = ReasonCalculated
	return est
}

// roundRatToInt performs exact round-half-up division of a big.Rat to big.Int.
func roundRatToInt(r *big.Rat) *big.Int {
	num := r.Num()
	denom := r.Denom()

	quotient := new(big.Int)
	rem := new(big.Int)
	quotient.QuoRem(num, denom, rem)

	if rem.Sign() > 0 {
		remDoubled := new(big.Int).Lsh(rem, 1)
		if remDoubled.Cmp(denom) >= 0 {
			quotient.Add(quotient, big.NewInt(1))
		}
	}
	return quotient
}
