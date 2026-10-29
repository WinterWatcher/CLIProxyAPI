package test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cost"
)

func helperMakeVerificationPolicy(prov, model string, price float64) *cost.PolicySnapshot {
	p := &cost.PolicySnapshot{
		SchemaVersion:            "cost-policy/v1",
		PolicyVersion:            "cpamp-" + prov + "-" + model + "-r1",
		Provider:                 prov,
		PublicModel:              model,
		UpstreamModelHash:        "upstream-hash-" + model,
		PriceStatus:              "paid_confirmed",
		BillingUnit:              "tokens",
		PromptPer1M:              price,
		CompletionPer1M:          price * 2,
		CachePer1M:               price * 0.5,
		UnitPrice:                0.0,
		MaxRequestCostUSD:        1.0,
		ProviderDailyBudgetUSD:   100.0,
		ProviderMonthlyBudgetUSD: 2000.0,
		GlobalDailyBudgetUSD:     500.0,
		GlobalMonthlyBudgetUSD:   10000.0,
		EffectiveAtMS:            time.Now().UnixMilli(),
		Source:                   "cpamp-approved",
	}
	if price == 0 {
		p.PriceStatus = "free_confirmed"
		p.CompletionPer1M = 0
		p.CachePer1M = 0
	}
	p.ContentHash = p.ComputeContentHash()
	return p
}

func TestSandbox_30VerificationChecks(t *testing.T) {
	dir := t.TempDir()
	policyStore, err := cost.NewPersistentPolicyStore(dir)
	if err != nil {
		t.Fatalf("policy store: %v", err)
	}
	budgetStore, err := cost.NewPersistentBudgetStore(dir)
	if err != nil {
		t.Fatalf("budget store: %v", err)
	}
	usageRecorder := NewMockUsageRecorder()

	mockFree := NewMockProviderServer("mock-free-provider", 200, `{"choices":[{"message":{"content":"free-ok"}}]}`)
	defer mockFree.Close()
	mockPaid := NewMockProviderServer("mock-paid-provider", 200, `{"choices":[{"message":{"content":"paid-ok"}}]}`)
	defer mockPaid.Close()
	mockExpensive := NewMockProviderServer("mock-expensive-provider", 200, `{"choices":[{"message":{"content":"expensive-ok"}}]}`)
	defer mockExpensive.Close()

	ctx := context.Background()

	// 1. 免费模型正常调用
	freeSnap := helperMakeVerificationPolicy("mock-free", "free-model", 0.0)
	_, _ = policyStore.Activate(freeSnap, false)
	cfg := &config.Config{CostGuard: config.CostGuardConfig{Enabled: true}}
	dec1 := cost.EvaluatePreflight(cost.GateRequest{
		Config:      cfg,
		Provider:    "mock-free",
		Model:       "free-model",
		Measurement: cost.FromTokens(100, 50, 0, 0, 0),
		PolicyStore: policyStore,
		BudgetStore: budgetStore,
	})
	if !dec1.Allowed || dec1.EstimatedCost.AmountNanos != 0 {
		t.Fatalf("1. expected free normal call allowed with 0 cost, got: %+v", dec1)
	}

	// 2. free -> paid 阻断
	freeToPaid := helperMakeVerificationPolicy("mock-free", "free-model", 1.0)
	freeToPaid.PolicyVersion = "v2"
	freeToPaid.ContentHash = freeToPaid.ComputeContentHash()
	dr2, _ := policyStore.DryRun(freeToPaid)
	if dr2.Allowed || dr2.Decision != "deny" || dr2.RiskLevel != "critical" {
		t.Fatalf("2. expected free to paid blocked, got: %+v", dr2)
	}

	// 3. unknown 阻断
	dec3 := cost.EvaluatePreflight(cost.GateRequest{
		Config:      cfg,
		Provider:    "mock-unknown",
		Model:       "unpriced-model",
		PolicyStore: policyStore,
	})
	if dec3.Allowed || dec3.Decision != cost.DecisionDeny {
		t.Fatalf("3. expected unknown blocked, got: %+v", dec3)
	}

	// 4 & 5. paid 同价 fallback 与 paid 更贵 fallback 阻断
	fallbackCfg := &config.Config{
		CostGuard: config.CostGuardConfig{Enabled: true},
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name:       "prov-a",
				CostPolicy: &config.ProviderCostPolicy{AllowFallback: true},
				Models: []config.OpenAICompatibilityModel{
					{Name: "shared", CostPolicy: &config.ModelCostPolicy{PriceStatus: "paid_confirmed", BillingUnit: "tokens", PromptPer1M: 1.0, CompletionPer1M: 2.0, PromptConfigured: true, CompletionConfigured: true}},
				},
			},
			{
				Name: "prov-same",
				Models: []config.OpenAICompatibilityModel{
					{Name: "shared", CostPolicy: &config.ModelCostPolicy{PriceStatus: "paid_confirmed", BillingUnit: "tokens", PromptPer1M: 1.0, CompletionPer1M: 2.0, PromptConfigured: true, CompletionConfigured: true}},
				},
			},
			{
				Name: "prov-expensive",
				Models: []config.OpenAICompatibilityModel{
					{Name: "shared", CostPolicy: &config.ModelCostPolicy{PriceStatus: "paid_confirmed", BillingUnit: "tokens", PromptPer1M: 10.0, CompletionPer1M: 20.0, PromptConfigured: true, CompletionConfigured: true}},
				},
			},
		},
	}
	filtRes := cost.FilterCandidateProviders(cost.ProviderFilterRequest{
		Config:      fallbackCfg,
		Providers:   []string{"prov-a", "prov-same", "prov-expensive"},
		Model:       "shared",
		Measurement: cost.FromTokens(100, 50, 0, 0, 0),
	})
	if len(filtRes.AllowedProviders) != 2 || filtRes.AllowedProviders[0] != "prov-a" || filtRes.AllowedProviders[1] != "prov-same" {
		t.Fatalf("4 & 5. expected same-price allowed and expensive blocked, got: %+v", filtRes.AllowedProviders)
	}

	// 6. CPAMP 审批导出
	exportedSnap := helperMakeVerificationPolicy("openai", "gpt-4o", 2.5)
	if exportedSnap.Source != "cpamp-approved" || exportedSnap.ContentHash == "" {
		t.Fatalf("6. invalid exported snapshot: %+v", exportedSnap)
	}

	// 7. CLIProxy 策略 Dry-run
	drReport, err := policyStore.DryRun(exportedSnap)
	if err != nil || !drReport.Allowed {
		t.Fatalf("7. dry run failed: %v, %+v", err, drReport)
	}

	// 8. CLIProxy 策略激活
	actSnap, err := policyStore.Activate(exportedSnap, false)
	if err != nil || actSnap.ContentHash != exportedSnap.ContentHash {
		t.Fatalf("8. activate failed: %v", err)
	}

	// 9. Hash 错误阻断
	badHashSnap := helperMakeVerificationPolicy("openai", "gpt-4o", 5.0)
	badHashSnap.ContentHash = exportedSnap.ContentHash // mismatch
	_, errBadHash := policyStore.Activate(badHashSnap, true)
	if errBadHash == nil || !strings.Contains(errBadHash.Error(), "mismatch") {
		t.Fatalf("9. expected hash mismatch error, got: %v", errBadHash)
	}

	// 10. 签名错误阻断
	sigSnap := helperMakeVerificationPolicy("openai", "gpt-4o", 2.5)
	sigSnap.Signature = "invalid-sig"
	_, errSig := policyStore.Activate(sigSnap, true)
	if errSig == nil || !strings.Contains(errSig.Error(), "signature") {
		t.Fatalf("10. expected signature error, got: %v", errSig)
	}

	// 11. 过期策略阻断
	expiredSnap := helperMakeVerificationPolicy("openai", "expired-m", 1.0)
	expiredSnap.EffectiveAtMS = 1000
	expiredSnap.ExpiresAtMS = 2000
	expiredSnap.ContentHash = expiredSnap.ComputeContentHash()
	_, errExpired := policyStore.Activate(expiredSnap, true)
	if errExpired == nil || !strings.Contains(errExpired.Error(), "expired") {
		t.Fatalf("11. expected expired error, got: %v", errExpired)
	}

	// 12. 策略回滚
	v2Snap := helperMakeVerificationPolicy("openai", "gpt-4o", 3.0)
	v2Snap.PolicyVersion = "v2"
	v2Snap.EffectiveAtMS = time.Now().UnixMilli() + 1000
	v2Snap.ContentHash = v2Snap.ComputeContentHash()
	_, _ = policyStore.Activate(v2Snap, true)
	rolled, errRollback := policyStore.Rollback("openai", "gpt-4o", exportedSnap.PolicyVersion)
	if errRollback != nil || rolled.PromptPer1M != exportedSnap.PromptPer1M {
		t.Fatalf("12. rollback failed: %v", errRollback)
	}

	// 13. Provider 日预算
	resProv, errProv := budgetStore.Reserve(ctx, cost.ReservationRequest{
		ReservationKey:  "res-prov-1",
		Provider:        "prov-budget-test",
		Model:           "m1",
		AmountNanos:     500,
		DailyLimitNanos: 1000,
	})
	if errProv != nil || resProv.ID == "" {
		t.Fatalf("13. reserve under provider daily budget failed: %v", errProv)
	}

	// 14. Global 日预算
	_, errGlobalDaily := budgetStore.Reserve(ctx, cost.ReservationRequest{
		ReservationKey:        "res-global-daily-fail",
		Provider:              "prov-budget-test",
		Model:                 "m1",
		AmountNanos:           2000,
		GlobalDailyLimitNanos: 1000,
	})
	if !errors.Is(errGlobalDaily, cost.ErrGlobalBudgetExceeded) {
		t.Fatalf("14. expected global daily budget exceeded, got: %v", errGlobalDaily)
	}

	// 15. Global 月预算
	_, errGlobalMonthly := budgetStore.Reserve(ctx, cost.ReservationRequest{
		ReservationKey:          "res-global-monthly-fail",
		Provider:                "prov-budget-test",
		Model:                   "m1",
		AmountNanos:             2000,
		GlobalMonthlyLimitNanos: 1000,
	})
	if !errors.Is(errGlobalMonthly, cost.ErrGlobalBudgetExceeded) {
		t.Fatalf("15. expected global monthly budget exceeded, got: %v", errGlobalMonthly)
	}

	// 16. Reservation 状态机
	resSM, _ := budgetStore.Reserve(ctx, cost.ReservationRequest{
		ReservationKey: "res-sm-1",
		Provider:       "prov-sm",
		Model:          "m1",
		AmountNanos:    500,
	})
	if err := budgetStore.Release(ctx, resSM.ID, "test"); err != nil {
		t.Fatalf("16. release failed: %v", err)
	}
	errSettleReleased := budgetStore.Settle(ctx, resSM.ID, 500, 0, 0)
	if errSettleReleased == nil || !strings.Contains(errSettleReleased.Error(), "state transition") {
		t.Fatalf("16. expected invalid state transition error, got: %v", errSettleReleased)
	}

	// 17. Settle 结算
	resSettle, _ := budgetStore.Reserve(ctx, cost.ReservationRequest{
		ReservationKey: "res-settle-1",
		Provider:       "prov-sm",
		Model:          "m1",
		AmountNanos:    500,
	})
	if err := budgetStore.Settle(ctx, resSettle.ID, 400, 0, 0); err != nil {
		t.Fatalf("17. settle failed: %v", err)
	}

	// 18. Release 释放
	resRel, _ := budgetStore.Reserve(ctx, cost.ReservationRequest{
		ReservationKey: "res-rel-1",
		Provider:       "prov-sm",
		Model:          "m1",
		AmountNanos:    500,
	})
	if err := budgetStore.Release(ctx, resRel.ID, "user_cancel"); err != nil {
		t.Fatalf("18. release failed: %v", err)
	}

	// 19. Overrun 冻结
	resOverrun, _ := budgetStore.Reserve(ctx, cost.ReservationRequest{
		ReservationKey: "res-overrun-1",
		Provider:       "prov-overrun",
		Model:          "m-overrun",
		AmountNanos:    100,
	})
	errOverrun := budgetStore.Settle(ctx, resOverrun.ID, 300, 0, 0)
	if !errors.Is(errOverrun, cost.ErrBudgetOverrun) {
		t.Fatalf("19. expected overrun error, got: %v", errOverrun)
	}
	isFrozen, _ := budgetStore.IsFrozen(ctx, "prov-overrun", "m-overrun")
	if !isFrozen {
		t.Fatal("19. expected frozen after overrun")
	}

	// 20. 显式 Unfreeze
	_ = budgetStore.UnfreezeProvider(ctx, "prov-overrun")
	_ = budgetStore.UnfreezeModel(ctx, "prov-overrun", "m-overrun")
	isFrozenAfter, _ := budgetStore.IsFrozen(ctx, "prov-overrun", "m-overrun")
	if isFrozenAfter {
		t.Fatal("20. expected unfrozen after explicit unfreeze")
	}

	// 21. 服务重启恢复
	restartedStore, errRestart := cost.NewPersistentPolicyStore(dir)
	if errRestart != nil {
		t.Fatalf("21. restart policy store failed: %v", errRestart)
	}
	recovered, ok := restartedStore.GetActivePolicy("openai", "gpt-4o")
	if !ok || recovered == nil {
		t.Fatal("21. active policy missing after restart")
	}

	// 22. ASR 带 duration
	asrDur := cost.ExtractNativeMeasurement("asr", []byte(`{"duration":15.0}`), "application/json")
	if asrDur.AudioSeconds != 15.0 {
		t.Fatalf("22. expected 15.0 audio seconds, got %f", asrDur.AudioSeconds)
	}

	// 23. ASR 不带 duration
	asrNoDur := cost.ExtractNativeMeasurement("asr", []byte(`{}`), "application/json")
	if asrNoDur.AudioSecondsPresent {
		t.Fatal("23. expected duration not present for empty ASR body")
	}

	// 24. TTS 按 Unicode 字符
	tts := cost.ExtractNativeMeasurement("tts", []byte(`{"input":"价格安全防线"}`), "application/json")
	if tts.Characters != 6 {
		t.Fatalf("24. expected 6 unicode chars, got %d", tts.Characters)
	}

	// 25. Image 按图片张数
	img := cost.ExtractNativeMeasurement("image", []byte(`{"n":4}`), "application/json")
	if img.ImageCount != 4 {
		t.Fatalf("25. expected 4 images, got %d", img.ImageCount)
	}

	// 26. Video submit 任务提交
	vidSub := cost.ExtractNativeMeasurement("video", []byte(`{"prompt":"car"}`), "application/json")
	if vidSub.VideoTasks != 1 || vidSub.IsVideoStatus {
		t.Fatalf("26. expected 1 video task, got: %+v", vidSub)
	}

	// 27. Video status 轮询不重复计费
	vidStat := cost.ExtractNativeMeasurement("video_status", []byte(`{}`), "application/json")
	if !vidStat.IsVideoStatus || vidStat.VideoTasks != 0 {
		t.Fatalf("27. expected video status without video task increment, got: %+v", vidStat)
	}

	// 28. Usage 快照
	usageRecorder.Record(map[string]any{
		"event":          "chat_completion",
		"model":          "gpt-4o",
		"policy_version": exportedSnap.PolicyVersion,
		"content_hash":   exportedSnap.ContentHash,
		"billing_unit":   "tokens",
		"cost_usd":       0.000375,
	})
	if usageRecorder.Count() != 1 {
		t.Fatal("28. usage recording failed")
	}

	// 29. 历史价格不变
	lastEvent := usageRecorder.LastEvent()
	if lastEvent["content_hash"] != exportedSnap.ContentHash {
		t.Fatalf("29. expected historical event to preserve hash %s", exportedSnap.ContentHash)
	}

	// 30. 审计脱敏
	events := policyStore.ListAuditEvents(10, 0)
	for _, e := range events {
		raw, _ := json.Marshal(e)
		if strings.Contains(string(raw), "sk-") || strings.Contains(string(raw), "Bearer") {
			t.Fatalf("30. audit event leaked secret: %s", string(raw))
		}
	}

	// 31. HTTP 错误脱敏
	errBody, status := cost.BuildGateErrorResponse(cost.GateDecision{
		Allowed:  false,
		Decision: cost.DecisionDeny,
		Code:     cost.CodePricePolicyViolation,
		Reason:   cost.ReasonMaxRequestCostExceeded,
		Provider: "openai",
		Model:    "gpt-4o",
	})
	if status != 402 || strings.Contains(string(errBody), "internal") || strings.Contains(string(errBody), "/home/") {
		t.Fatalf("31. HTTP error leaked internal info: %s", string(errBody))
	}
}
