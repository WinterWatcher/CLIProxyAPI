package cost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/tidwall/gjson"
)

// Decision constants.
const (
	DecisionAllow  = "allow"
	DecisionDeny   = "deny"
	DecisionWarn   = "warn"
	DecisionFreeze = "freeze"
)

// Standard gate error code.
const CodePricePolicyViolation = "price_policy_violation"

// Standard gate reason constants.
const (
	ReasonCostGuardDisabled      = "cost_guard_disabled"
	ReasonUnknownPrice           = "unknown_price"
	ReasonMissingModelPolicy     = "missing_model_price_policy"
	ReasonMissingQuantity        = "missing_quantity"
	ReasonMaxRequestCostExceeded = "max_request_cost_exceeded"
	ReasonProviderBudgetExceeded = "provider_budget_exceeded"
	ReasonGlobalBudgetExceeded   = "global_budget_exceeded"
	ReasonBudgetOverrunFreeze    = "budget_overrun_freeze"
	ReasonUnsupportedUnit        = "unsupported_billing_unit"
	ReasonAllowed                = "allowed"
	ReasonExplicitFree           = "explicit_free"
)

// CostPolicyAudit summarizes the audit data associated with a gate decision.
type CostPolicyAudit struct {
	PolicyEnabled bool   `json:"policy_enabled"`
	PolicyVersion string `json:"policy_version"`
	PriceStatus   string `json:"price_status"`
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	BillingUnit   string `json:"billing_unit"`
	AmountKnown   bool   `json:"amount_known"`
	QuantityKnown bool   `json:"quantity_known"`
	Decision      string `json:"decision"`
	Reason        string `json:"reason"`
}

// GateDecision encapsulates the outcome of a preflight cost policy evaluation.
type GateDecision struct {
	Allowed             bool            `json:"allowed"`
	Decision            string          `json:"decision"`
	Code                string          `json:"code,omitempty"`
	Reason              string          `json:"reason"`
	Provider            string          `json:"provider"`
	Model               string          `json:"model"`
	PriceStatus         string          `json:"price_status"`
	BillingUnit         string          `json:"billing_unit"`
	EstimatedCost       CostEstimate    `json:"estimated_cost"`
	MaxRequestCostNanos int64           `json:"max_request_cost_nanos,omitempty"`
	Audit               CostPolicyAudit `json:"audit"`
}

// GateRequest carries all context required for a preflight cost evaluation.
type GateRequest struct {
	Config      *config.Config
	Provider    string
	Model       string
	Capability  string
	Measurement Measurement
	RawPayload  []byte
	ContentType string
	RequestID   string
	Attempt     int
	BudgetStore BudgetStore
	DryRun      bool
}

// ResolveModelPolicy resolves the provider policy and model policy matching the request.
func ResolveModelPolicy(cfg *config.Config, providerName, modelName, capability string) (*config.ProviderCostPolicy, *config.ModelCostPolicy, string, string) {
	if cfg == nil || len(cfg.OpenAICompatibility) == 0 {
		return nil, nil, providerName, modelName
	}

	modelLower := strings.ToLower(strings.TrimSpace(modelName))
	provLower := strings.ToLower(strings.TrimSpace(providerName))
	capLower := strings.ToLower(strings.TrimSpace(capability))

	// Match helper for model capability
	capMatches := func(m config.OpenAICompatibilityModel) bool {
		if capLower == "" || capLower == "chat" || capLower == "openai" {
			return true
		}
		mCap := strings.ToLower(strings.TrimSpace(m.Capability))
		mType := strings.ToLower(strings.TrimSpace(m.Type))
		return mCap == capLower || mType == capLower ||
			(capLower == "speech" && (mCap == "tts" || mType == "tts")) ||
			(capLower == "tts" && (mCap == "speech" || mType == "speech")) ||
			(capLower == "video_status" && (mCap == "video" || mType == "video")) ||
			(capLower == "video" && (mCap == "video_status" || mType == "video_status"))
	}

	// 1. If providerName is specified, look into that specific provider
	if provLower != "" {
		for i := range cfg.OpenAICompatibility {
			p := &cfg.OpenAICompatibility[i]
			nameLower := strings.ToLower(strings.TrimSpace(p.Name))
			if nameLower == provLower || "openai-compatible-"+nameLower == provLower {
				for j := range p.Models {
					m := &p.Models[j]
					if (strings.ToLower(strings.TrimSpace(m.Alias)) == modelLower || strings.ToLower(strings.TrimSpace(m.Name)) == modelLower) && capMatches(*m) {
						return p.CostPolicy, m.CostPolicy, p.Name, m.Name
					}
				}
				// Provider matched but model not in its list; check provider cost policy
				return p.CostPolicy, nil, p.Name, modelName
			}
		}
	}

	// 2. If providerName not specified or not matched, scan all providers
	for i := range cfg.OpenAICompatibility {
		p := &cfg.OpenAICompatibility[i]
		for j := range p.Models {
			m := &p.Models[j]
			if (strings.ToLower(strings.TrimSpace(m.Alias)) == modelLower || strings.ToLower(strings.TrimSpace(m.Name)) == modelLower) && capMatches(*m) {
				return p.CostPolicy, m.CostPolicy, p.Name, m.Name
			}
		}
	}

	return nil, nil, providerName, modelName
}

// EvaluatePreflight performs pure, deterministic pre-flight cost policy evaluation before any upstream network request.
func EvaluatePreflight(req GateRequest) GateDecision {
	// If cost-guard is disabled or unconfigured, preserve legacy behavior: allow everything unconditionally.
	if req.Config == nil || !req.Config.CostGuard.Enabled {
		return GateDecision{
			Allowed:  true,
			Decision: DecisionAllow,
			Reason:   ReasonCostGuardDisabled,
			Provider: req.Provider,
			Model:    req.Model,
		}
	}

	cfg := req.Config
	provPolicy, modelPolicy, resolvedProvider, resolvedUpstream := ResolveModelPolicy(cfg, req.Provider, req.Model, req.Capability)

	effectiveProvider := resolvedProvider
	if effectiveProvider == "" {
		effectiveProvider = req.Provider
	}

	// Fail-closed Rule 1: Missing model cost policy
	if modelPolicy == nil {
		action := strings.ToLower(strings.TrimSpace(cfg.CostGuard.UnknownPriceAction))
		if action == "" {
			action = config.CostActionDeny
		}
		if action == config.CostActionWarn {
			dec := GateDecision{
				Allowed:     true,
				Decision:    DecisionWarn,
				Reason:      ReasonUnknownPrice,
				Provider:    effectiveProvider,
				Model:       req.Model,
				PriceStatus: config.PriceStatusUnknown,
				Audit: CostPolicyAudit{
					PolicyEnabled: true,
					PriceStatus:   config.PriceStatusUnknown,
					Provider:      effectiveProvider,
					Model:         req.Model,
					Decision:      DecisionWarn,
					Reason:        ReasonUnknownPrice,
				},
			}
			recordGateAudit(dec, resolvedUpstream, req.RequestID, req.Attempt)
			return dec
		}

		dec := GateDecision{
			Allowed:     false,
			Decision:    DecisionDeny,
			Code:        CodePricePolicyViolation,
			Reason:      ReasonUnknownPrice,
			Provider:    effectiveProvider,
			Model:       req.Model,
			PriceStatus: config.PriceStatusUnknown,
			Audit: CostPolicyAudit{
				PolicyEnabled: true,
				PriceStatus:   config.PriceStatusUnknown,
				Provider:      effectiveProvider,
				Model:         req.Model,
				Decision:      DecisionDeny,
				Reason:        ReasonUnknownPrice,
			},
		}
		recordGateAudit(dec, resolvedUpstream, req.RequestID, req.Attempt)
		return dec
	}

	// Fail-closed Rule 2: Explicitly frozen model
	if strings.EqualFold(strings.TrimSpace(modelPolicy.PriceStatus), config.PriceStatusFrozen) {
		dec := GateDecision{
			Allowed:     false,
			Decision:    DecisionFreeze,
			Code:        CodePricePolicyViolation,
			Reason:      ReasonModelPriceFrozen,
			Provider:    effectiveProvider,
			Model:       req.Model,
			PriceStatus: config.PriceStatusFrozen,
			BillingUnit: modelPolicy.BillingUnit,
			Audit: CostPolicyAudit{
				PolicyEnabled: true,
				PriceStatus:   config.PriceStatusFrozen,
				Provider:      effectiveProvider,
				Model:         req.Model,
				BillingUnit:   modelPolicy.BillingUnit,
				Decision:      DecisionFreeze,
				Reason:        ReasonModelPriceFrozen,
			},
		}
		recordGateAudit(dec, resolvedUpstream, req.RequestID, req.Attempt)
		return dec
	}

	// Fail-closed Rule 3: Unknown price status
	if strings.EqualFold(strings.TrimSpace(modelPolicy.PriceStatus), config.PriceStatusUnknown) || modelPolicy.PriceStatus == "" {
		dec := GateDecision{
			Allowed:     false,
			Decision:    DecisionDeny,
			Code:        CodePricePolicyViolation,
			Reason:      ReasonUnknownPrice,
			Provider:    effectiveProvider,
			Model:       req.Model,
			PriceStatus: config.PriceStatusUnknown,
			BillingUnit: modelPolicy.BillingUnit,
			Audit: CostPolicyAudit{
				PolicyEnabled: true,
				PriceStatus:   config.PriceStatusUnknown,
				Provider:      effectiveProvider,
				Model:         req.Model,
				BillingUnit:   modelPolicy.BillingUnit,
				Decision:      DecisionDeny,
				Reason:        ReasonUnknownPrice,
			},
		}
		recordGateAudit(dec, resolvedUpstream, req.RequestID, req.Attempt)
		return dec
	}

	// Evaluate cost estimate using Task 2 deterministic calculator
	measurement := req.Measurement
	if strings.EqualFold(strings.TrimSpace(req.Capability), "video_status") {
		measurement.IsVideoStatus = true
	}
	costReq := CostRequest{
		Provider:    effectiveProvider,
		Model:       req.Model,
		Policy:      modelPolicy,
		Measurement: measurement,
		BillingUnit: modelPolicy.BillingUnit,
		Currency:    cfg.CostGuard.Currency,
	}
	est := EstimateCost(costReq)

	var maxRequestCostNanos int64
	if modelPolicy.MaxRequestCost > 0 {
		maxRequestCostNanos = int64(math.Round(modelPolicy.MaxRequestCost * float64(NanosPerUSD)))
	}

	// Rule 4: Handle Explicitly Free confirmed models (0 price)
	if est.Status == StatusExplicitFree || (est.Known && est.ExplicitlyFree) {
		dec := GateDecision{
			Allowed:             true,
			Decision:            DecisionAllow,
			Reason:              ReasonExplicitFree,
			Provider:            effectiveProvider,
			Model:               req.Model,
			PriceStatus:         est.PriceStatus,
			BillingUnit:         est.BillingUnit,
			EstimatedCost:       est,
			MaxRequestCostNanos: maxRequestCostNanos,
			Audit: CostPolicyAudit{
				PolicyEnabled: true,
				PriceStatus:   est.PriceStatus,
				Provider:      effectiveProvider,
				Model:         req.Model,
				BillingUnit:   est.BillingUnit,
				AmountKnown:   est.AmountKnown,
				QuantityKnown: est.QuantityKnown,
				Decision:      DecisionAllow,
				Reason:        ReasonExplicitFree,
			},
		}
		recordGateAudit(dec, resolvedUpstream, req.RequestID, req.Attempt)
		return dec
	}

	// Fail-closed Rule 5: Non-free model calculation failure
	if est.Status != StatusKnown {
		reason := est.Reason
		if est.Status == StatusMissingQuantity {
			reason = ReasonMissingQuantity
		} else if reason == "" {
			reason = est.Status
		}
		dec := GateDecision{
			Allowed:             false,
			Decision:            DecisionDeny,
			Code:                CodePricePolicyViolation,
			Reason:              reason,
			Provider:            effectiveProvider,
			Model:               req.Model,
			PriceStatus:         est.PriceStatus,
			BillingUnit:         est.BillingUnit,
			EstimatedCost:       est,
			MaxRequestCostNanos: maxRequestCostNanos,
			Audit: CostPolicyAudit{
				PolicyEnabled: true,
				PriceStatus:   est.PriceStatus,
				Provider:      effectiveProvider,
				Model:         req.Model,
				BillingUnit:   est.BillingUnit,
				AmountKnown:   est.AmountKnown,
				QuantityKnown: est.QuantityKnown,
				Decision:      DecisionDeny,
				Reason:        reason,
			},
		}
		recordGateAudit(dec, resolvedUpstream, req.RequestID, req.Attempt)
		return dec
	}

	// Rule 6: Check Single-Request Cost Limit (max-request-cost)
	if modelPolicy.MaxRequestCost > 0 && est.AmountNanos > maxRequestCostNanos {
		dec := GateDecision{
			Allowed:             false,
			Decision:            DecisionDeny,
			Code:                CodePricePolicyViolation,
			Reason:              ReasonMaxRequestCostExceeded,
			Provider:            effectiveProvider,
			Model:               req.Model,
			PriceStatus:         est.PriceStatus,
			BillingUnit:         est.BillingUnit,
			EstimatedCost:       est,
			MaxRequestCostNanos: maxRequestCostNanos,
			Audit: CostPolicyAudit{
				PolicyEnabled: true,
				PriceStatus:   est.PriceStatus,
				Provider:      effectiveProvider,
				Model:         req.Model,
				BillingUnit:   est.BillingUnit,
				AmountKnown:   est.AmountKnown,
				QuantityKnown: est.QuantityKnown,
				Decision:      DecisionDeny,
				Reason:        ReasonMaxRequestCostExceeded,
			},
		}
		recordGateAudit(dec, resolvedUpstream, req.RequestID, req.Attempt)
		return dec
	}

	// Rule 7: max-request-cost == 0 strictly forbids non-zero cost
	if modelPolicy.MaxRequestCost == 0 && est.AmountNanos > 0 && strings.EqualFold(strings.TrimSpace(modelPolicy.OnNonzeroPrice), config.CostActionDeny) {
		dec := GateDecision{
			Allowed:             false,
			Decision:            DecisionDeny,
			Code:                CodePricePolicyViolation,
			Reason:              ReasonMaxRequestCostExceeded,
			Provider:            effectiveProvider,
			Model:               req.Model,
			PriceStatus:         est.PriceStatus,
			BillingUnit:         est.BillingUnit,
			EstimatedCost:       est,
			MaxRequestCostNanos: 0,
			Audit: CostPolicyAudit{
				PolicyEnabled: true,
				PriceStatus:   est.PriceStatus,
				Provider:      effectiveProvider,
				Model:         req.Model,
				BillingUnit:   est.BillingUnit,
				AmountKnown:   est.AmountKnown,
				QuantityKnown: est.QuantityKnown,
				Decision:      DecisionDeny,
				Reason:        ReasonMaxRequestCostExceeded,
			},
		}
		recordGateAudit(dec, resolvedUpstream, req.RequestID, req.Attempt)
		return dec
	}

	// Rule 8: Budget check & reservation (when BudgetStore is configured)
	if req.BudgetStore != nil {
		if frozen, freezeReason := req.BudgetStore.IsFrozen(context.Background(), effectiveProvider, req.Model); frozen {
			return budgetDenyDecision(effectiveProvider, req.Model, resolvedUpstream, est, maxRequestCostNanos, ReasonBudgetOverrunFreeze, freezeReason, req.RequestID, req.Attempt)
		}

		var dailyLimitNanos, monthlyLimitNanos int64
		if provPolicy != nil {
			if provPolicy.ProviderBudget.DailyUSD > 0 {
				dailyLimitNanos = int64(math.Round(provPolicy.ProviderBudget.DailyUSD * float64(NanosPerUSD)))
			}
			if provPolicy.ProviderBudget.MonthlyUSD > 0 {
				monthlyLimitNanos = int64(math.Round(provPolicy.ProviderBudget.MonthlyUSD * float64(NanosPerUSD)))
			}
		}

		var globalDailyLimitNanos, globalMonthlyLimitNanos int64
		if req.Config != nil {
			if req.Config.CostGuard.Budget.DailyUSD > 0 {
				globalDailyLimitNanos = int64(math.Round(req.Config.CostGuard.Budget.DailyUSD * float64(NanosPerUSD)))
			}
			if req.Config.CostGuard.Budget.MonthlyUSD > 0 {
				globalMonthlyLimitNanos = int64(math.Round(req.Config.CostGuard.Budget.MonthlyUSD * float64(NanosPerUSD)))
			}
		}

		if req.DryRun {
			// Dry-run mode for candidate filtering: PURE EVALUATION, NO RESERVATION SIDE EFFECTS
			ctx := context.Background()
			if dailyLimitNanos > 0 {
				provDaily, _ := req.BudgetStore.CurrentProviderDaily(ctx, effectiveProvider)
				if provDaily+est.AmountNanos > dailyLimitNanos {
					return budgetDenyDecision(effectiveProvider, req.Model, resolvedUpstream, est, maxRequestCostNanos, ReasonProviderBudgetExceeded, ReasonProviderBudgetExceeded, req.RequestID, req.Attempt)
				}
			}
			if monthlyLimitNanos > 0 {
				provMonthly, _ := req.BudgetStore.CurrentProviderMonthly(ctx, effectiveProvider)
				if provMonthly+est.AmountNanos > monthlyLimitNanos {
					return budgetDenyDecision(effectiveProvider, req.Model, resolvedUpstream, est, maxRequestCostNanos, ReasonProviderBudgetExceeded, ReasonProviderBudgetExceeded, req.RequestID, req.Attempt)
				}
			}
			if globalDailyLimitNanos > 0 {
				globalDaily, _ := req.BudgetStore.CurrentGlobalDaily(ctx)
				if globalDaily+est.AmountNanos > globalDailyLimitNanos {
					return budgetDenyDecision(effectiveProvider, req.Model, resolvedUpstream, est, maxRequestCostNanos, ReasonGlobalBudgetExceeded, ReasonGlobalBudgetExceeded, req.RequestID, req.Attempt)
				}
			}
			if globalMonthlyLimitNanos > 0 {
				globalMonthly, _ := req.BudgetStore.CurrentGlobalMonthly(ctx)
				if globalMonthly+est.AmountNanos > globalMonthlyLimitNanos {
					return budgetDenyDecision(effectiveProvider, req.Model, resolvedUpstream, est, maxRequestCostNanos, ReasonGlobalBudgetExceeded, ReasonGlobalBudgetExceeded, req.RequestID, req.Attempt)
				}
			}
		} else if dailyLimitNanos > 0 || monthlyLimitNanos > 0 || globalDailyLimitNanos > 0 || globalMonthlyLimitNanos > 0 {
			resKey := req.RequestID
			if resKey == "" {
				resKey = fmt.Sprintf("req-%s-%d", effectiveProvider, time.Now().UnixNano())
			}
			_, errReserve := req.BudgetStore.Reserve(context.Background(), ReservationRequest{
				ReservationKey:          resKey,
				RequestID:               req.RequestID,
				Provider:                effectiveProvider,
				Model:                   req.Model,
				BillingUnit:             est.BillingUnit,
				AmountNanos:             est.AmountNanos,
				DailyLimitNanos:         dailyLimitNanos,
				MonthlyLimitNanos:       monthlyLimitNanos,
				GlobalDailyLimitNanos:   globalDailyLimitNanos,
				GlobalMonthlyLimitNanos: globalMonthlyLimitNanos,
			})
			if errReserve != nil {
				reason := ReasonProviderBudgetExceeded
				if errors.Is(errReserve, ErrGlobalBudgetExceeded) {
					reason = ReasonGlobalBudgetExceeded
				} else if errors.Is(errReserve, ErrBudgetOverrunFrozen) {
					reason = ReasonBudgetOverrunFreeze
				}
				return budgetDenyDecision(effectiveProvider, req.Model, resolvedUpstream, est, maxRequestCostNanos, reason, reason, req.RequestID, req.Attempt)
			}
		}
	}

	// Passed all security and cost checks
	dec := GateDecision{
		Allowed:             true,
		Decision:            DecisionAllow,
		Reason:              ReasonAllowed,
		Provider:            effectiveProvider,
		Model:               req.Model,
		PriceStatus:         est.PriceStatus,
		BillingUnit:         est.BillingUnit,
		EstimatedCost:       est,
		MaxRequestCostNanos: maxRequestCostNanos,
		Audit: CostPolicyAudit{
			PolicyEnabled: true,
			PriceStatus:   est.PriceStatus,
			Provider:      effectiveProvider,
			Model:         req.Model,
			BillingUnit:   est.BillingUnit,
			AmountKnown:   est.AmountKnown,
			QuantityKnown: est.QuantityKnown,
			Decision:      DecisionAllow,
			Reason:        ReasonAllowed,
		},
	}
	recordGateAudit(dec, resolvedUpstream, req.RequestID, req.Attempt)
	return dec
}

func recordGateAudit(dec GateDecision, upstreamModel, requestID string, attempt int) {
	ev := CostAuditEvent{
		Timestamp:            time.Now(),
		Provider:             dec.Provider,
		PublicModel:          dec.Model,
		UpstreamModelHash:    HashUpstreamModel(upstreamModel),
		BillingUnit:          dec.BillingUnit,
		PriceStatus:          dec.PriceStatus,
		AmountKnown:          dec.EstimatedCost.AmountKnown,
		QuantityKnown:        dec.EstimatedCost.QuantityKnown,
		EstimatedAmountNanos: dec.EstimatedCost.AmountNanos,
		MaxRequestCostNanos:  dec.MaxRequestCostNanos,
		Decision:             dec.Decision,
		Reason:               dec.Reason,
		RequestID:            requestID,
		Attempt:              attempt,
	}
	RecordAuditEvent(ev)
}

// BuildGateErrorResponse constructs the standard HTTP 402 JSON error response.
func BuildGateErrorResponse(decision GateDecision) ([]byte, int) {
	errPayload := map[string]any{
		"code":         CodePricePolicyViolation,
		"message":      fmt.Sprintf("Request blocked by the configured model price policy: %s", decision.Reason),
		"reason":       decision.Reason,
		"provider":     decision.Provider,
		"model":        decision.Model,
		"billing_unit": decision.BillingUnit,
		"amount_known": decision.EstimatedCost.AmountKnown,
	}
	if decision.EstimatedCost.AmountKnown {
		errPayload["estimated_cost"] = decision.EstimatedCost.Amount
	}
	if decision.MaxRequestCostNanos > 0 {
		errPayload["max_request_cost"] = float64(decision.MaxRequestCostNanos) / float64(NanosPerUSD)
	}

	resp := map[string]any{
		"error": errPayload,
	}
	bytes, _ := json.Marshal(resp)
	return bytes, http.StatusPaymentRequired
}

func budgetDenyDecision(provider, model, resolvedUpstream string, est CostEstimate, maxRequestCostNanos int64, reason, auditReason, requestID string, attempt int) GateDecision {
	dec := GateDecision{
		Allowed:             false,
		Decision:            DecisionDeny,
		Code:                CodePricePolicyViolation,
		Reason:              reason,
		Provider:            provider,
		Model:               model,
		PriceStatus:         est.PriceStatus,
		BillingUnit:         est.BillingUnit,
		EstimatedCost:       est,
		MaxRequestCostNanos: maxRequestCostNanos,
		Audit: CostPolicyAudit{
			PolicyEnabled: true,
			PriceStatus:   est.PriceStatus,
			Provider:      provider,
			Model:         model,
			BillingUnit:   est.BillingUnit,
			AmountKnown:   est.AmountKnown,
			QuantityKnown: est.QuantityKnown,
			Decision:      DecisionDeny,
			Reason:        auditReason,
		},
	}
	recordGateAudit(dec, resolvedUpstream, requestID, attempt)
	return dec
}

// ExtractNativeMeasurement extracts capability-specific measurements from incoming request bodies.
func ExtractNativeMeasurement(capability string, body []byte, contentType string) Measurement {
	switch strings.ToLower(strings.TrimSpace(capability)) {
	case "tts", "speech":
		input := gjson.GetBytes(body, "input").String()
		if !gjson.GetBytes(body, "input").Exists() {
			return Measurement{}
		}
		chars := int64(utf8.RuneCountInString(input))
		return FromCharacterCount(chars)

	case "image":
		nResult := gjson.GetBytes(body, "n")
		var count int64 = 1
		if nResult.Exists() {
			count = nResult.Int()
		}
		if count < 0 {
			count = 0
		}
		return FromImageCount(count)

	case "video", "video_submit":
		return FromVideoSubmit(1)

	case "video_status":
		return FromVideoStatus()

	case "embedding", "rerank":
		// If input is explicitly an array of integer token IDs (e.g. [[1, 2, 3]])
		inputRes := gjson.GetBytes(body, "input")
		if inputRes.IsArray() {
			array := inputRes.Array()
			if len(array) > 0 && array[0].Type == gjson.Number {
				return FromTokens(int64(len(array)), 0, 0, 0, 0)
			}
		}
		// Text input tokens cannot be reliably known before upstream tokenization
		return Measurement{}

	case "asr":
		// Audio duration cannot be reliably determined from raw audio bytes without decoding
		durationRes := gjson.GetBytes(body, "duration")
		if durationRes.Exists() && durationRes.Float() >= 0 {
			return FromAudioSeconds(durationRes.Float())
		}
		return Measurement{}

	default:
		return Measurement{}
	}
}
