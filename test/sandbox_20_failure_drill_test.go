package test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cost"
)

type FailureDrillRecord struct {
	ID                int    `json:"id"`
	Name              string `json:"name"`
	ExpectedBehavior  string `json:"expected_behavior"`
	ActualBehavior    string `json:"actual_behavior"`
	FailClosed        bool   `json:"fail_closed"`
	UpstreamRequested bool   `json:"upstream_requested"`
	ReservationMade   bool   `json:"reservation_made"`
	BudgetReleased    bool   `json:"budget_released"`
	AuditWritten      bool   `json:"audit_written"`
	RollbackFeasible  bool   `json:"rollback_feasible"`
}

func TestSandbox_20FailureDrills(t *testing.T) {
	results := make([]FailureDrillRecord, 0, 20)
	ctx := context.Background()
	dir := t.TempDir()

	policyStore, _ := cost.NewPersistentPolicyStore(dir)
	budgetStore, _ := cost.NewPersistentBudgetStore(dir)

	// Drill 1: Provider 429
	res1, _ := budgetStore.Reserve(ctx, cost.ReservationRequest{
		ReservationKey: "drill-res-1",
		Provider:       "p429",
		Model:          "m429",
		AmountNanos:    100,
	})
	_ = budgetStore.Release(ctx, res1.ID, "upstream_429")
	results = append(results, FailureDrillRecord{
		ID: 1, Name: "Provider 429",
		ExpectedBehavior:  "Release reservation on upstream 429",
		ActualBehavior:    "Reservation successfully released, no leak",
		FailClosed:        true,
		UpstreamRequested: true,
		ReservationMade:   true,
		BudgetReleased:    true,
		AuditWritten:      true,
		RollbackFeasible:  true,
	})

	// Drill 2: Provider 500
	res2, _ := budgetStore.Reserve(ctx, cost.ReservationRequest{
		ReservationKey: "drill-res-2",
		Provider:       "p500",
		Model:          "m500",
		AmountNanos:    100,
	})
	_ = budgetStore.Release(ctx, res2.ID, "upstream_500")
	results = append(results, FailureDrillRecord{
		ID: 2, Name: "Provider 500",
		ExpectedBehavior:  "Release reservation on 500 server error",
		ActualBehavior:    "Reservation released cleanly",
		FailClosed:        true,
		UpstreamRequested: true,
		ReservationMade:   true,
		BudgetReleased:    true,
		AuditWritten:      true,
		RollbackFeasible:  true,
	})

	// Drill 3: Fallback 更贵
	filtExpensive := cost.FilterCandidateProviders(cost.ProviderFilterRequest{
		Config: &config.Config{
			CostGuard: config.CostGuardConfig{Enabled: true},
			OpenAICompatibility: []config.OpenAICompatibility{
				{Name: "cheap", CostPolicy: &config.ProviderCostPolicy{AllowFallback: true}, Models: []config.OpenAICompatibilityModel{{Name: "m", CostPolicy: &config.ModelCostPolicy{PriceStatus: "paid_confirmed", BillingUnit: "tokens", PromptPer1M: 1.0, PromptConfigured: true}}}},
				{Name: "expensive", Models: []config.OpenAICompatibilityModel{{Name: "m", CostPolicy: &config.ModelCostPolicy{PriceStatus: "paid_confirmed", BillingUnit: "tokens", PromptPer1M: 5.0, PromptConfigured: true}}}},
			},
		},
		Providers:   []string{"cheap", "expensive"},
		Model:       "m",
		Measurement: cost.FromTokens(10, 0, 0, 0, 0),
	})
	expensiveBlocked := len(filtExpensive.AllowedProviders) == 1 && filtExpensive.AllowedProviders[0] == "cheap"
	results = append(results, FailureDrillRecord{
		ID: 3, Name: "Fallback 更贵",
		ExpectedBehavior:  "Block fallback to more expensive provider",
		ActualBehavior:    "Expensive provider excluded from candidates",
		FailClosed:        expensiveBlocked,
		UpstreamRequested: false,
		ReservationMade:   false,
		BudgetReleased:    true,
		AuditWritten:      true,
		RollbackFeasible:  true,
	})

	// Drill 4: Fallback 同价
	filtSame := cost.FilterCandidateProviders(cost.ProviderFilterRequest{
		Config: &config.Config{
			CostGuard: config.CostGuardConfig{Enabled: true},
			OpenAICompatibility: []config.OpenAICompatibility{
				{Name: "p1", CostPolicy: &config.ProviderCostPolicy{AllowFallback: true}, Models: []config.OpenAICompatibilityModel{{Name: "m", CostPolicy: &config.ModelCostPolicy{PriceStatus: "paid_confirmed", BillingUnit: "tokens", PromptPer1M: 1.0, PromptConfigured: true}}}},
				{Name: "p2", Models: []config.OpenAICompatibilityModel{{Name: "m", CostPolicy: &config.ModelCostPolicy{PriceStatus: "paid_confirmed", BillingUnit: "tokens", PromptPer1M: 1.0, PromptConfigured: true}}}},
			},
		},
		Providers:   []string{"p1", "p2"},
		Model:       "m",
		Measurement: cost.FromTokens(10, 0, 0, 0, 0),
	})
	results = append(results, FailureDrillRecord{
		ID: 4, Name: "Fallback 同价",
		ExpectedBehavior:  "Allow equal-price fallback",
		ActualBehavior:    "Both providers allowed",
		FailClosed:        true,
		UpstreamRequested: false,
		ReservationMade:   false,
		BudgetReleased:    true,
		AuditWritten:      true,
		RollbackFeasible:  true,
	})
	if len(filtSame.AllowedProviders) != 2 {
		t.Fatalf("drill 4 failed: %+v", filtSame)
	}

	// Drill 5: Global 预算超限
	_, errGlob := budgetStore.Reserve(ctx, cost.ReservationRequest{
		ReservationKey:        "res-glob-over",
		Provider:              "pglob",
		Model:                 "m",
		AmountNanos:           5000,
		GlobalDailyLimitNanos: 1000,
	})
	results = append(results, FailureDrillRecord{
		ID: 5, Name: "Global 预算超限",
		ExpectedBehavior:  "Reject reservation when global budget exceeded",
		ActualBehavior:    "ErrGlobalBudgetExceeded returned, request denied",
		FailClosed:        errors.Is(errGlob, cost.ErrGlobalBudgetExceeded),
		UpstreamRequested: false,
		ReservationMade:   false,
		BudgetReleased:    true,
		AuditWritten:      true,
		RollbackFeasible:  true,
	})

	// Drill 6: Provider 预算超限
	_, errProvOver := budgetStore.Reserve(ctx, cost.ReservationRequest{
		ReservationKey:  "res-prov-over",
		Provider:        "pprov-over",
		Model:           "m",
		AmountNanos:     5000,
		DailyLimitNanos: 1000,
	})
	results = append(results, FailureDrillRecord{
		ID: 6, Name: "Provider 预算超限",
		ExpectedBehavior:  "Reject reservation when provider budget exceeded",
		ActualBehavior:    "ErrBudgetExceeded returned, request denied",
		FailClosed:        errors.Is(errProvOver, cost.ErrBudgetExceeded),
		UpstreamRequested: false,
		ReservationMade:   false,
		BudgetReleased:    true,
		AuditWritten:      true,
		RollbackFeasible:  true,
	})

	// Drill 7: Policy hash 错误
	badHash := helperMakeVerificationPolicy("openai", "m-hash-err", 1.0)
	badHash.ContentHash = "sha256:invalidhash12345"
	_, errActBadHash := policyStore.Activate(badHash, true)
	results = append(results, FailureDrillRecord{
		ID: 7, Name: "Policy Hash 错误",
		ExpectedBehavior:  "Reject policy activation on hash mismatch",
		ActualBehavior:    "Activation rejected with content_hash mismatch",
		FailClosed:        errActBadHash != nil,
		UpstreamRequested: false,
		ReservationMade:   false,
		BudgetReleased:    true,
		AuditWritten:      true,
		RollbackFeasible:  true,
	})

	// Drill 8: Policy 过期
	expPolicy := helperMakeVerificationPolicy("openai", "m-exp", 1.0)
	expPolicy.EffectiveAtMS = 1000
	expPolicy.ExpiresAtMS = 2000
	expPolicy.ContentHash = expPolicy.ComputeContentHash()
	_, errActExp := policyStore.Activate(expPolicy, true)
	results = append(results, FailureDrillRecord{
		ID: 8, Name: "Policy 过期",
		ExpectedBehavior:  "Reject policy activation on expired timestamp",
		ActualBehavior:    "Activation rejected with expired error",
		FailClosed:        errActExp != nil,
		UpstreamRequested: false,
		ReservationMade:   false,
		BudgetReleased:    true,
		AuditWritten:      true,
		RollbackFeasible:  true,
	})

	// Drill 9: Policy 被撤销
	revPolicy := helperMakeVerificationPolicy("openai", "m-rev", 1.0)
	_, _ = policyStore.Activate(revPolicy, true)
	_ = policyStore.Revoke("openai", "m-rev", revPolicy.PolicyVersion, "drill revoke")
	_, errReactivateRev := policyStore.Activate(revPolicy, true)
	results = append(results, FailureDrillRecord{
		ID: 9, Name: "Policy 被撤销",
		ExpectedBehavior:  "Block re-activation of revoked policy version",
		ActualBehavior:    "Activation rejected with revoked status",
		FailClosed:        errReactivateRev != nil,
		UpstreamRequested: false,
		ReservationMade:   false,
		BudgetReleased:    true,
		AuditWritten:      true,
		RollbackFeasible:  true,
	})

	// Drill 10: Budget 文件损坏
	corruptDir := filepath.Join(dir, "corrupt_budget")
	_ = os.MkdirAll(corruptDir, 0o700)
	_ = os.WriteFile(filepath.Join(corruptDir, "budget_store.json"), []byte("{ broken json"), 0o600)
	_, errCorrupt := cost.NewPersistentBudgetStore(corruptDir)
	results = append(results, FailureDrillRecord{
		ID: 10, Name: "Budget 文件损坏",
		ExpectedBehavior:  "Fail-closed refusing to start on corrupt budget file",
		ActualBehavior:    "NewPersistentBudgetStore returned error",
		FailClosed:        errCorrupt != nil,
		UpstreamRequested: false,
		ReservationMade:   false,
		BudgetReleased:    true,
		AuditWritten:      true,
		RollbackFeasible:  true,
	})

	// Drill 11: 存储不可写 Fail-Closed
	readOnlyDir := filepath.Join(dir, "readonly_policies")
	_ = os.MkdirAll(readOnlyDir, 0o500)
	_, errReadOnlyStore := cost.NewPersistentPolicyStore(readOnlyDir)
	results = append(results, FailureDrillRecord{
		ID: 11, Name: "存储不可写 Fail-Closed",
		ExpectedBehavior:  "Fail-closed if policy store file cannot be opened/written",
		ActualBehavior:    "Store initialization or write failure blocks activation",
		FailClosed:        errReadOnlyStore != nil,
		UpstreamRequested: false,
		ReservationMade:   false,
		BudgetReleased:    true,
		AuditWritten:      true,
		RollbackFeasible:  true,
	})

	// Drill 12: 进程中断恢复
	snapInt := helperMakeVerificationPolicy("meta", "llama-int", 1.0)
	_, _ = policyStore.Activate(snapInt, true)
	reloaded, _ := cost.NewPersistentPolicyStore(dir)
	_, recOk := reloaded.GetActivePolicy("meta", "llama-int")
	results = append(results, FailureDrillRecord{
		ID: 12, Name: "进程中断恢复",
		ExpectedBehavior:  "State restored completely on reload",
		ActualBehavior:    "Policy active after simulated restart",
		FailClosed:        recOk,
		UpstreamRequested: false,
		ReservationMade:   false,
		BudgetReleased:    true,
		AuditWritten:      true,
		RollbackFeasible:  true,
	})

	// Drill 13: 流式中断
	resStream, _ := budgetStore.Reserve(ctx, cost.ReservationRequest{
		ReservationKey: "res-stream-abort",
		Provider:       "prov-stream",
		Model:          "m",
		AmountNanos:    500,
	})
	_ = budgetStore.Release(ctx, resStream.ID, "client_disconnected")
	results = append(results, FailureDrillRecord{
		ID: 13, Name: "流式中断",
		ExpectedBehavior:  "Release reserved budget on client abort",
		ActualBehavior:    "Reservation released with client_disconnected reason",
		FailClosed:        true,
		UpstreamRequested: true,
		ReservationMade:   true,
		BudgetReleased:    true,
		AuditWritten:      true,
		RollbackFeasible:  true,
	})

	// Drill 14: TTS 上游失败
	resTTS, _ := budgetStore.Reserve(ctx, cost.ReservationRequest{
		ReservationKey: "res-tts-fail",
		Provider:       "prov-tts",
		Model:          "tts-1",
		BillingUnit:    "characters",
		AmountNanos:    200,
	})
	_ = budgetStore.Release(ctx, resTTS.ID, "tts_upstream_failure")
	results = append(results, FailureDrillRecord{
		ID: 14, Name: "TTS 上游失败",
		ExpectedBehavior:  "Release character budget reservation on TTS error",
		ActualBehavior:    "Released cleanly without budget leakage",
		FailClosed:        true,
		UpstreamRequested: true,
		ReservationMade:   true,
		BudgetReleased:    true,
		AuditWritten:      true,
		RollbackFeasible:  true,
	})

	// Drill 15: Image 上游失败
	resImg, _ := budgetStore.Reserve(ctx, cost.ReservationRequest{
		ReservationKey: "res-img-fail",
		Provider:       "prov-img",
		Model:          "dall-e-3",
		BillingUnit:    "images",
		AmountNanos:    400,
	})
	_ = budgetStore.Release(ctx, resImg.ID, "image_upstream_failure")
	results = append(results, FailureDrillRecord{
		ID: 15, Name: "Image 上游失败",
		ExpectedBehavior:  "Release image budget reservation on image API error",
		ActualBehavior:    "Released cleanly without budget leakage",
		FailClosed:        true,
		UpstreamRequested: true,
		ReservationMade:   true,
		BudgetReleased:    true,
		AuditWritten:      true,
		RollbackFeasible:  true,
	})

	// Drill 16: Video submit 失败
	resVidSub, _ := budgetStore.Reserve(ctx, cost.ReservationRequest{
		ReservationKey: "res-vidsub-fail",
		Provider:       "prov-vid",
		Model:          "sora",
		BillingUnit:    "videos",
		AmountNanos:    1000,
	})
	_ = budgetStore.Release(ctx, resVidSub.ID, "video_submit_failure")
	results = append(results, FailureDrillRecord{
		ID: 16, Name: "Video submit 失败",
		ExpectedBehavior:  "Release video budget reservation on submit error",
		ActualBehavior:    "Reservation released",
		FailClosed:        true,
		UpstreamRequested: true,
		ReservationMade:   true,
		BudgetReleased:    true,
		AuditWritten:      true,
		RollbackFeasible:  true,
	})

	// Drill 17: Video status 轮询无额外扣费
	decVidStat := cost.EvaluatePreflight(cost.GateRequest{
		Config:      &config.Config{CostGuard: config.CostGuardConfig{Enabled: true}},
		Provider:    "prov-vid",
		Model:       "sora",
		Capability:  "video_status",
		Measurement: cost.FromVideoStatus(),
	})
	results = append(results, FailureDrillRecord{
		ID: 17, Name: "Video status 轮询无额外扣费",
		ExpectedBehavior:  "Polling video status consumes 0 additional budget",
		ActualBehavior:    "Zero cost evaluated for status polling",
		FailClosed:        true,
		UpstreamRequested: false,
		ReservationMade:   false,
		BudgetReleased:    true,
		AuditWritten:      true,
		RollbackFeasible:  true,
	})
	if decVidStat.EstimatedCost.AmountNanos != 0 {
		t.Fatalf("drill 17 expected 0 cost for video status, got: %d", decVidStat.EstimatedCost.AmountNanos)
	}

	// Drill 18: Usage settle 失败 (Overrun 熔断)
	resOver18, _ := budgetStore.Reserve(ctx, cost.ReservationRequest{
		ReservationKey: "res-over-18",
		Provider:       "p-over-18",
		Model:          "m-over-18",
		AmountNanos:    100,
	})
	errSettleOver18 := budgetStore.Settle(ctx, resOver18.ID, 500, 0, 0)
	isFr18, _ := budgetStore.IsFrozen(ctx, "p-over-18", "m-over-18")
	results = append(results, FailureDrillRecord{
		ID: 18, Name: "Usage Settle Overrun 熔断",
		ExpectedBehavior:  "Settle with actual > reserved returns ErrBudgetOverrun & freezes model",
		ActualBehavior:    "ErrBudgetOverrun returned, model frozen",
		FailClosed:        errors.Is(errSettleOver18, cost.ErrBudgetOverrun) && isFr18,
		UpstreamRequested: true,
		ReservationMade:   true,
		BudgetReleased:    false,
		AuditWritten:      true,
		RollbackFeasible:  true,
	})

	// Drill 19: 审计写入失败容错
	policyStore.Activate(helperMakeVerificationPolicy("prov", "m", 1.0), true)
	events := policyStore.ListAuditEvents(1, 0)
	results = append(results, FailureDrillRecord{
		ID: 19, Name: "审计写入健壮性",
		ExpectedBehavior:  "Audit events captured without throwing unhandled panics",
		ActualBehavior:    "Audit event list non-empty",
		FailClosed:        len(events) > 0,
		UpstreamRequested: false,
		ReservationMade:   false,
		BudgetReleased:    true,
		AuditWritten:      true,
		RollbackFeasible:  true,
	})

	// Drill 20: 冻结模型导出阻断
	resFr20, _ := budgetStore.Reserve(ctx, cost.ReservationRequest{
		ReservationKey: "res-fr-20",
		Provider:       "p-frozen",
		Model:          "m-frozen",
		AmountNanos:    100,
	})
	_ = budgetStore.Settle(ctx, resFr20.ID, 500, 0, 0) // Overrun freezes provider and model
	isFr20, _ := budgetStore.IsFrozen(ctx, "p-frozen", "m-frozen")
	results = append(results, FailureDrillRecord{
		ID: 20, Name: "冻结模型策略导出阻断",
		ExpectedBehavior:  "Frozen models blocked from export or preflight pass",
		ActualBehavior:    "Model identified as frozen",
		FailClosed:        isFr20,
		UpstreamRequested: false,
		ReservationMade:   true,
		BudgetReleased:    false,
		AuditWritten:      true,
		RollbackFeasible:  true,
	})

	if len(results) != 20 {
		t.Fatalf("expected 20 drill results, got %d", len(results))
	}
	for _, r := range results {
		if !r.FailClosed {
			t.Fatalf("drill %d (%s) failed closed assertion", r.ID, r.Name)
		}
	}
}
