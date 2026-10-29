package cost

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

// Audit event constants for fallback and provider filtering.
const (
	AuditEventProviderEvaluated  = "provider_candidate_evaluated"
	AuditEventProviderFiltered   = "provider_candidate_filtered"
	AuditEventFallbackAllowed    = "fallback_allowed"
	AuditEventFallbackBlocked    = "fallback_blocked"
	AuditEventAliasModelFiltered = "alias_pool_model_filtered"
)

// ProviderDecision encapsulates the gate outcome for a single provider.
type ProviderDecision struct {
	Provider            string       `json:"provider"`
	Allowed             bool         `json:"allowed"`
	Decision            string       `json:"decision"`
	Code                string       `json:"code,omitempty"`
	Reason              string       `json:"reason"`
	PriceStatus         string       `json:"price_status"`
	BillingUnit         string       `json:"billing_unit"`
	EstimatedCost       CostEstimate `json:"estimated_cost"`
	MaxRequestCostNanos int64        `json:"max_request_cost_nanos,omitempty"`
}

// ProviderFilterResult holds the filtered list of safe providers and individual decisions.
type ProviderFilterResult struct {
	AllowedProviders []string           `json:"allowed_providers"`
	Decisions        []ProviderDecision `json:"decisions"`
	PrimaryDecision  ProviderDecision   `json:"primary_decision"`
}

// ProviderFilterRequest provides parameters to filter candidate providers.
type ProviderFilterRequest struct {
	Config      *config.Config
	Providers   []string
	Model       string
	Capability  string
	Measurement Measurement
	RawPayload  []byte
	ContentType string
	RequestID   string
	Attempt     int
	BudgetStore BudgetStore
	PolicyStore PolicyStore
}

// EvaluateProviderCandidate evaluates a single provider candidate against preflight price and budget rules without reserving budget.
func EvaluateProviderCandidate(req GateRequest) GateDecision {
	req.DryRun = true
	return EvaluatePreflight(req)
}

// ReserveFinalProvider reserves budget for the chosen provider once candidate filtering has completed.
// Only the single final provider selected for actual execution will have budget reserved.
func ReserveFinalProvider(
	ctx context.Context,
	store BudgetStore,
	cfg *config.Config,
	provider, model, capability string,
	est CostEstimate,
	reqID string,
) (Reservation, error) {
	if store == nil || cfg == nil || !cfg.CostGuard.Enabled {
		return Reservation{}, nil
	}

	provPolicy, _, effectiveProvider, _ := ResolveModelPolicy(cfg, provider, model, capability)
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
	if cfg.CostGuard.Budget.DailyUSD > 0 {
		globalDailyLimitNanos = int64(math.Round(cfg.CostGuard.Budget.DailyUSD * float64(NanosPerUSD)))
	}
	if cfg.CostGuard.Budget.MonthlyUSD > 0 {
		globalMonthlyLimitNanos = int64(math.Round(cfg.CostGuard.Budget.MonthlyUSD * float64(NanosPerUSD)))
	}

	if dailyLimitNanos <= 0 && monthlyLimitNanos <= 0 && globalDailyLimitNanos <= 0 && globalMonthlyLimitNanos <= 0 {
		return Reservation{}, nil
	}

	resKey := reqID
	if resKey == "" {
		resKey = fmt.Sprintf("req-%s-%d", effectiveProvider, time.Now().UnixNano())
	}

	return store.Reserve(ctx, ReservationRequest{
		ReservationKey:          resKey,
		RequestID:               reqID,
		Provider:                effectiveProvider,
		Model:                   model,
		BillingUnit:             est.BillingUnit,
		AmountNanos:             est.AmountNanos,
		DailyLimitNanos:         dailyLimitNanos,
		MonthlyLimitNanos:       monthlyLimitNanos,
		GlobalDailyLimitNanos:   globalDailyLimitNanos,
		GlobalMonthlyLimitNanos: globalMonthlyLimitNanos,
	})
}

// FilterCandidateProviders evaluates all candidate providers against the price policy,
// budget limits, and fallback compatibility matrix, returning only safe providers.
// Provider filtering is a pure decision function and does NOT create reservations.
func FilterCandidateProviders(req ProviderFilterRequest) ProviderFilterResult {
	if req.Config == nil || !req.Config.CostGuard.Enabled {
		// When cost guard is disabled, pass all providers through unchanged.
		return ProviderFilterResult{
			AllowedProviders: append([]string(nil), req.Providers...),
		}
	}

	result := ProviderFilterResult{
		AllowedProviders: make([]string, 0, len(req.Providers)),
		Decisions:        make([]ProviderDecision, 0, len(req.Providers)),
	}

	var primaryDecision *ProviderDecision
	var primaryModelPolicy *config.ModelCostPolicy

	for idx, prov := range req.Providers {
		// 1. Evaluate this candidate provider (dryRun = true: NO RESERVATIONS CREATED!)
		provPolicy, modelPolicy, resolvedProv, resolvedUpstream := ResolveModelPolicy(req.Config, prov, req.Model, req.Capability)
		gateReq := GateRequest{
			Config:      req.Config,
			Provider:    prov,
			Model:       req.Model,
			Capability:  req.Capability,
			Measurement: req.Measurement,
			RawPayload:  req.RawPayload,
			ContentType: req.ContentType,
			RequestID:   req.RequestID,
			Attempt:     req.Attempt,
			BudgetStore: req.BudgetStore,
			PolicyStore: req.PolicyStore,
			DryRun:      true,
		}
		gateDec := EvaluateProviderCandidate(gateReq)

		pDec := ProviderDecision{
			Provider:            resolvedProv,
			Allowed:             gateDec.Allowed,
			Decision:            gateDec.Decision,
			Code:                gateDec.Code,
			Reason:              gateDec.Reason,
			PriceStatus:         gateDec.PriceStatus,
			BillingUnit:         gateDec.BillingUnit,
			EstimatedCost:       gateDec.EstimatedCost,
			MaxRequestCostNanos: gateDec.MaxRequestCostNanos,
		}
		result.Decisions = append(result.Decisions, pDec)

		// Audit evaluation of candidate
		RecordAuditEvent(CostAuditEvent{
			Timestamp:            time.Now(),
			Provider:             pDec.Provider,
			PublicModel:          req.Model,
			UpstreamModelHash:    HashUpstreamModel(resolvedUpstream),
			BillingUnit:          pDec.BillingUnit,
			PriceStatus:          pDec.PriceStatus,
			AmountKnown:          pDec.EstimatedCost.AmountKnown,
			QuantityKnown:        pDec.EstimatedCost.QuantityKnown,
			EstimatedAmountNanos: pDec.EstimatedCost.AmountNanos,
			MaxRequestCostNanos:  pDec.MaxRequestCostNanos,
			Decision:             pDec.Decision,
			Reason:               pDec.Reason,
			RequestID:            req.RequestID,
			Attempt:              idx,
		})

		if !gateDec.Allowed {
			// Candidate violated its own price policy or budget
			RecordAuditEvent(CostAuditEvent{
				Timestamp:            time.Now(),
				Provider:             pDec.Provider,
				PublicModel:          req.Model,
				UpstreamModelHash:    HashUpstreamModel(resolvedUpstream),
				BillingUnit:          pDec.BillingUnit,
				PriceStatus:          pDec.PriceStatus,
				AmountKnown:          pDec.EstimatedCost.AmountKnown,
				QuantityKnown:        pDec.EstimatedCost.QuantityKnown,
				EstimatedAmountNanos: pDec.EstimatedCost.AmountNanos,
				MaxRequestCostNanos:  pDec.MaxRequestCostNanos,
				Decision:             DecisionDeny,
				Reason:               AuditEventProviderFiltered,
				RequestID:            req.RequestID,
				Attempt:              idx,
			})
			continue
		}

		// 2. If this is the first allowed provider, it becomes the primary baseline
		if primaryDecision == nil {
			primaryDecision = &pDec
			primaryModelPolicy = modelPolicy
			result.PrimaryDecision = pDec
			result.AllowedProviders = append(result.AllowedProviders, prov)
			continue
		}

		// 3. For secondary/fallback providers, verify price compatibility against primary
		compatible, compatReason := IsFallbackPriceCompatible(primaryDecision.EstimatedCost, pDec.EstimatedCost, primaryModelPolicy, modelPolicy)
		if !compatible {
			pDec.Allowed = false
			pDec.Decision = DecisionDeny
			pDec.Reason = compatReason

			RecordAuditEvent(CostAuditEvent{
				Timestamp:            time.Now(),
				Provider:             pDec.Provider,
				PublicModel:          req.Model,
				UpstreamModelHash:    HashUpstreamModel(resolvedUpstream),
				BillingUnit:          pDec.BillingUnit,
				PriceStatus:          pDec.PriceStatus,
				AmountKnown:          pDec.EstimatedCost.AmountKnown,
				QuantityKnown:        pDec.EstimatedCost.QuantityKnown,
				EstimatedAmountNanos: pDec.EstimatedCost.AmountNanos,
				MaxRequestCostNanos:  pDec.MaxRequestCostNanos,
				Decision:             DecisionDeny,
				Reason:               compatReason,
				RequestID:            req.RequestID,
				Attempt:              idx,
			})
			continue
		}

		// Fallback allowed
		RecordAuditEvent(CostAuditEvent{
			Timestamp:            time.Now(),
			Provider:             pDec.Provider,
			PublicModel:          req.Model,
			UpstreamModelHash:    HashUpstreamModel(resolvedUpstream),
			BillingUnit:          pDec.BillingUnit,
			PriceStatus:          pDec.PriceStatus,
			AmountKnown:          pDec.EstimatedCost.AmountKnown,
			QuantityKnown:        pDec.EstimatedCost.QuantityKnown,
			EstimatedAmountNanos: pDec.EstimatedCost.AmountNanos,
			MaxRequestCostNanos:  pDec.MaxRequestCostNanos,
			Decision:             DecisionAllow,
			Reason:               AuditEventFallbackAllowed,
			RequestID:            req.RequestID,
			Attempt:              idx,
		})
		result.AllowedProviders = append(result.AllowedProviders, prov)
		_ = provPolicy
	}

	if primaryDecision == nil && len(result.Decisions) > 0 {
		result.PrimaryDecision = result.Decisions[0]
	}

	return result
}

// FallbackGuard encapsulates active fallback constraints during execution and retry.
type FallbackGuard struct {
	Config           *config.Config
	PrimaryProvider  string
	PrimaryEstimate  CostEstimate
	PrimaryPolicy    *config.ModelCostPolicy
	AllowedProviders map[string]struct{}
	Model            string
	Capability       string
	Measurement      Measurement
	RequestID        string
}

// NewFallbackGuard creates a FallbackGuard from a successful primary evaluation.
func NewFallbackGuard(cfg *config.Config, primaryProvider, model, capability string, est CostEstimate, primaryPolicy *config.ModelCostPolicy, allowed []string, m Measurement, reqID string) *FallbackGuard {
	allowedMap := make(map[string]struct{}, len(allowed))
	for _, p := range allowed {
		allowedMap[strings.ToLower(strings.TrimSpace(p))] = struct{}{}
		allowedMap[util.OpenAICompatibleProviderKey(strings.ToLower(strings.TrimSpace(p)))] = struct{}{}
	}
	return &FallbackGuard{
		Config:           cfg,
		PrimaryProvider:  primaryProvider,
		PrimaryEstimate:  est,
		PrimaryPolicy:    primaryPolicy,
		AllowedProviders: allowedMap,
		Model:            model,
		Capability:       capability,
		Measurement:      m,
		RequestID:        reqID,
	}
}

type fallbackGuardContextKey struct{}

// WithFallbackGuard attaches a FallbackGuard to the context.
func WithFallbackGuard(ctx context.Context, guard *FallbackGuard) context.Context {
	if ctx == nil || guard == nil {
		return ctx
	}
	return context.WithValue(ctx, fallbackGuardContextKey{}, guard)
}

// FallbackGuardFromContext extracts the active FallbackGuard from context if present.
func FallbackGuardFromContext(ctx context.Context) *FallbackGuard {
	if ctx == nil {
		return nil
	}
	guard, _ := ctx.Value(fallbackGuardContextKey{}).(*FallbackGuard)
	return guard
}

// IsProviderAllowed checks if a candidate provider is in the pre-filtered allowed set.
func (g *FallbackGuard) IsProviderAllowed(provider string) bool {
	if g == nil || g.Config == nil || !g.Config.CostGuard.Enabled {
		return true
	}
	norm := strings.ToLower(strings.TrimSpace(provider))
	if _, ok := g.AllowedProviders[norm]; ok {
		return true
	}
	trimmed := strings.TrimPrefix(norm, "openai-compatible-")
	if _, ok := g.AllowedProviders[trimmed]; ok {
		return true
	}
	if _, ok := g.AllowedProviders[util.OpenAICompatibleProviderKey(norm)]; ok {
		return true
	}
	return false
}

// ValidateCandidateAuth checks whether picking this candidate auth/provider is price-compatible.
func (g *FallbackGuard) ValidateCandidateAuth(candidateProvider, upstreamModel string) (bool, string) {
	if g == nil || g.Config == nil || !g.Config.CostGuard.Enabled {
		return true, ""
	}

	// 1. Must be in the allowed provider set
	if !g.IsProviderAllowed(candidateProvider) {
		return false, "provider_not_in_allowed_set"
	}

	// 2. Resolve candidate model policy
	_, candPolicy, _, _ := ResolveModelPolicy(g.Config, candidateProvider, upstreamModel, g.Capability)
	if candPolicy == nil {
		// Try resolving with public model
		_, candPolicy, _, _ = ResolveModelPolicy(g.Config, candidateProvider, g.Model, g.Capability)
	}

	candReq := CostRequest{
		Provider:    candidateProvider,
		Model:       upstreamModel,
		Policy:      candPolicy,
		Measurement: g.Measurement,
		Currency:    g.Config.CostGuard.Currency,
	}
	candEst := EstimateCost(candReq)

	// Check fallback compatibility
	compatible, reason := IsFallbackPriceCompatible(g.PrimaryEstimate, candEst, g.PrimaryPolicy, candPolicy)
	if !compatible {
		RecordAuditEvent(CostAuditEvent{
			Timestamp:            time.Now(),
			Provider:             candidateProvider,
			PublicModel:          g.Model,
			UpstreamModelHash:    HashUpstreamModel(upstreamModel),
			BillingUnit:          candEst.BillingUnit,
			PriceStatus:          candEst.PriceStatus,
			AmountKnown:          candEst.AmountKnown,
			QuantityKnown:        candEst.QuantityKnown,
			EstimatedAmountNanos: candEst.AmountNanos,
			Decision:             DecisionDeny,
			Reason:               reason,
			RequestID:            g.RequestID,
		})
		return false, reason
	}

	return true, ""
}

// ValidateUpstreamModelInPool checks whether switching to another model inside an alias pool is price-compatible.
func (g *FallbackGuard) ValidateUpstreamModelInPool(provider, currentUpstream, nextUpstream string) (bool, string) {
	if g == nil || g.Config == nil || !g.Config.CostGuard.Enabled {
		return true, ""
	}

	// Resolve policy for current and next upstream
	_, curPolicy, _, _ := ResolveModelPolicy(g.Config, provider, currentUpstream, g.Capability)
	_, nextPolicy, _, _ := ResolveModelPolicy(g.Config, provider, nextUpstream, g.Capability)

	curEst := EstimateCost(CostRequest{
		Provider:    provider,
		Model:       currentUpstream,
		Policy:      curPolicy,
		Measurement: g.Measurement,
	})
	nextEst := EstimateCost(CostRequest{
		Provider:    provider,
		Model:       nextUpstream,
		Policy:      nextPolicy,
		Measurement: g.Measurement,
	})

	compatible, reason := IsFallbackPriceCompatible(curEst, nextEst, curPolicy, nextPolicy)
	if !compatible {
		RecordAuditEvent(CostAuditEvent{
			Timestamp:            time.Now(),
			Provider:             provider,
			PublicModel:          g.Model,
			UpstreamModelHash:    HashUpstreamModel(nextUpstream),
			BillingUnit:          nextEst.BillingUnit,
			PriceStatus:          nextEst.PriceStatus,
			AmountKnown:          nextEst.AmountKnown,
			QuantityKnown:        nextEst.QuantityKnown,
			EstimatedAmountNanos: nextEst.AmountNanos,
			Decision:             DecisionDeny,
			Reason:               AuditEventAliasModelFiltered,
			RequestID:            g.RequestID,
		})
		return false, reason
	}

	return true, ""
}
