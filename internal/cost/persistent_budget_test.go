package cost

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPersistentBudgetStoreLifecycle(t *testing.T) {
	dir := t.TempDir()
	store, err := NewPersistentBudgetStore(dir)
	if err != nil {
		t.Fatalf("NewPersistentBudgetStore error: %v", err)
	}

	ctx := context.Background()

	// 27. Reserve成功: $1.00 = 1,000,000,000 nanos
	res, err := store.Reserve(ctx, ReservationRequest{
		ReservationKey:    "req-1-attempt-0",
		RequestID:         "req-1",
		Provider:          "prov-a",
		Model:             "model-1",
		BillingUnit:       "tokens",
		AmountNanos:       1_000_000_000,
		DailyLimitNanos:   5_000_000_000,  // $5.00 daily
		MonthlyLimitNanos: 50_000_000_000, // $50.00 monthly
	})
	if err != nil {
		t.Fatalf("Reserve failed: %v", err)
	}
	if res.AmountNanos != 1_000_000_000 {
		t.Errorf("expected 1B nanos reserved, got %d", res.AmountNanos)
	}

	// 28. 同 key 幂等
	resDup, err := store.Reserve(ctx, ReservationRequest{
		ReservationKey:    "req-1-attempt-0",
		RequestID:         "req-1",
		Provider:          "prov-a",
		AmountNanos:       1_000_000_000,
		DailyLimitNanos:   5_000_000_000,
		MonthlyLimitNanos: 50_000_000_000,
	})
	if err != nil {
		t.Fatalf("idempotent reserve failed: %v", err)
	}
	if resDup.ID != res.ID {
		t.Errorf("expected same reservation ID, got %s vs %s", resDup.ID, res.ID)
	}

	// 29. Provider 日预算检查: 试图再预定 $4.50 (总共 $5.50 > $5.00 limit) -> 超限报错
	_, errOverDaily := store.Reserve(ctx, ReservationRequest{
		ReservationKey:  "req-2",
		Provider:        "prov-a",
		AmountNanos:     4_500_000_000,
		DailyLimitNanos: 5_000_000_000,
	})
	if errOverDaily == nil {
		t.Fatal("expected daily limit exceeded error")
	}

	// 30. Provider 月预算检查: 试图预定超出月度预算
	_, errOverMonthly := store.Reserve(ctx, ReservationRequest{
		ReservationKey:    "req-3",
		Provider:          "prov-a",
		AmountNanos:       2_000_000_000,
		DailyLimitNanos:   100_000_000_000,
		MonthlyLimitNanos: 2_500_000_000, // limit $2.50, current already $1.00, +$2.00 = $3.00 > $2.50
	})
	if errOverMonthly == nil {
		t.Fatal("expected monthly limit exceeded error")
	}

	// 35. 成功 Settle: 实际消费 $0.80 (800,000,000 nanos)
	errSettle := store.Settle(ctx, "req-1-attempt-0", 800_000_000, 5_000_000_000, 50_000_000_000)
	if errSettle != nil {
		t.Fatalf("Settle failed: %v", errSettle)
	}

	// 36. Actual 小于 reserve 释放差额: 当前消费应为 800,000,000，而不是预估的 1,000,000,000
	dailySpent, _ := store.CurrentProviderDailyAt(ctx, "prov-a", time.Now().UTC())
	if dailySpent != 800_000_000 {
		t.Errorf("expected daily spent 800M nanos after settle, got %d", dailySpent)
	}

	// 34. 失败 Release: 新建一个预定并释放
	_, _ = store.Reserve(ctx, ReservationRequest{
		ReservationKey: "req-failed",
		Provider:       "prov-a",
		AmountNanos:    500_000_000,
	})
	errRelease := store.Release(ctx, "req-failed", "upstream_error")
	if errRelease != nil {
		t.Fatalf("Release failed: %v", errRelease)
	}

	// 验证 release 后不计入日常消费
	dailyAfterRelease, _ := store.CurrentProviderDailyAt(ctx, "prov-a", time.Now().UTC())
	if dailyAfterRelease != 800_000_000 {
		t.Errorf("expected daily spent to remain 800M after release, got %d", dailyAfterRelease)
	}

	// 38. 过期 Reservation 回收: 建立一个已超时的预定
	_, _ = store.Reserve(ctx, ReservationRequest{
		ReservationKey: "req-expired",
		Provider:       "prov-a",
		AmountNanos:    300_000_000,
		TTL:            -1 * time.Minute, // already expired
	})
	reaped, errReap := store.ReapExpired(ctx, time.Now().UTC())
	if errReap != nil || reaped != 1 {
		t.Errorf("expected 1 reaped reservation, got %d (err: %v)", reaped, errReap)
	}

	// 39. 重启后数据依然存在
	restartedStore, errRestart := NewPersistentBudgetStore(dir)
	if errRestart != nil {
		t.Fatalf("restart NewPersistentBudgetStore failed: %v", errRestart)
	}
	restartedDaily, _ := restartedStore.CurrentProviderDailyAt(ctx, "prov-a", time.Now().UTC())
	if restartedDaily != 800_000_000 {
		t.Errorf("restarted store lost daily spent: got %d, want 800M", restartedDaily)
	}

	// 43. int64 溢出拒绝
	_, errOverflow := store.Reserve(ctx, ReservationRequest{
		ReservationKey: "overflow",
		Provider:       "prov-a",
		AmountNanos:    math.MaxInt64,
	})
	if errOverflow == nil {
		t.Fatal("expected int64 overflow to be rejected")
	}
}

func TestGlobalBudgetLimits(t *testing.T) {
	// A2: Global budget daily/monthly limits & atomic multi-tier enforcement
	dir := t.TempDir()
	store, err := NewPersistentBudgetStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// 1. Provider pass / Global fail
	_, errProvPassGlobalFail := store.Reserve(ctx, ReservationRequest{
		ReservationKey:        "req-global-over",
		Provider:              "prov-b",
		AmountNanos:           2_000_000_000, // $2.00
		DailyLimitNanos:       10_000_000_000,
		GlobalDailyLimitNanos: 1_000_000_000, // global daily $1.00 max
	})
	if errProvPassGlobalFail == nil || !errors.Is(errProvPassGlobalFail, ErrGlobalBudgetExceeded) {
		t.Fatalf("expected ErrGlobalBudgetExceeded, got %v", errProvPassGlobalFail)
	}

	// 2. Global pass / Provider fail
	_, errGlobalPassProvFail := store.Reserve(ctx, ReservationRequest{
		ReservationKey:        "req-prov-over",
		Provider:              "prov-b",
		AmountNanos:           5_000_000_000, // $5.00
		DailyLimitNanos:       2_000_000_000, // provider daily $2.00 max
		GlobalDailyLimitNanos: 10_000_000_000,
	})
	if errGlobalPassProvFail == nil || !errors.Is(errGlobalPassProvFail, ErrBudgetExceeded) {
		t.Fatalf("expected ErrBudgetExceeded, got %v", errGlobalPassProvFail)
	}

	// 3. Global monthly limit exceeded
	_, errGlobalMonthly := store.Reserve(ctx, ReservationRequest{
		ReservationKey:          "req-global-monthly",
		Provider:                "prov-b",
		AmountNanos:             3_000_000_000,
		GlobalMonthlyLimitNanos: 2_000_000_000, // $2.00 max
	})
	if errGlobalMonthly == nil || !errors.Is(errGlobalMonthly, ErrGlobalBudgetExceeded) {
		t.Fatalf("expected ErrGlobalBudgetExceeded, got %v", errGlobalMonthly)
	}

	// 4. Restart global accounting
	_, errOk := store.Reserve(ctx, ReservationRequest{
		ReservationKey:        "req-global-ok",
		Provider:              "prov-c",
		AmountNanos:           500_000_000,
		GlobalDailyLimitNanos: 10_000_000_000,
	})
	if errOk != nil {
		t.Fatalf("reserve failed: %v", errOk)
	}

	restarted, errRestart := NewPersistentBudgetStore(dir)
	if errRestart != nil {
		t.Fatal(errRestart)
	}
	globalDaily, _ := restarted.CurrentGlobalDaily(ctx)
	if globalDaily != 500_000_000 {
		t.Fatalf("expected global daily 500M, got %d", globalDaily)
	}
}

func TestBudgetOverrunAndFreezeLifecycle(t *testing.T) {
	// A3: Actual > reserved triggers budget overrun, freezes provider/model, and persists
	dir := t.TempDir()
	store, err := NewPersistentBudgetStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// 1. Reserve 1B nanos ($1.00)
	_, errRes := store.Reserve(ctx, ReservationRequest{
		ReservationKey:  "res-overrun-test",
		RequestID:       "req-overrun",
		Provider:        "prov-freeze",
		Model:           "model-freeze",
		BillingUnit:     "tokens",
		AmountNanos:     1_000_000_000,
		DailyLimitNanos: 50_000_000_000,
	})
	if errRes != nil {
		t.Fatal(errRes)
	}

	// 2. Settle with actual = 2B nanos ($2.00 > $1.00 reserved) -> OVERRUN!
	errSettle := store.Settle(ctx, "res-overrun-test", 2_000_000_000, 50_000_000_000, 50_000_000_000)
	if errSettle == nil || !errors.Is(errSettle, ErrBudgetOverrun) {
		t.Fatalf("expected ErrBudgetOverrun, got %v", errSettle)
	}

	// 3. Verify provider and model are frozen
	frozen, reason := store.IsFrozen(ctx, "prov-freeze", "model-freeze")
	if !frozen || reason != "provider_budget_overrun_freeze" {
		t.Fatalf("expected provider to be frozen, got frozen=%v, reason=%s", frozen, reason)
	}

	// 4. Subsequent reserve must be rejected with ErrBudgetOverrunFrozen
	_, errSubsequent := store.Reserve(ctx, ReservationRequest{
		ReservationKey:  "res-after-freeze",
		RequestID:       "req-blocked",
		Provider:        "prov-freeze",
		Model:           "model-freeze",
		AmountNanos:     100_000_000,
		DailyLimitNanos: 50_000_000_000,
	})
	if errSubsequent == nil || !errors.Is(errSubsequent, ErrBudgetOverrunFrozen) {
		t.Fatalf("expected ErrBudgetOverrunFrozen, got %v", errSubsequent)
	}

	// 5. Restart store: frozen status MUST persist!
	restarted, errRestart := NewPersistentBudgetStore(dir)
	if errRestart != nil {
		t.Fatal(errRestart)
	}
	frozenRestart, _ := restarted.IsFrozen(ctx, "prov-freeze", "model-freeze")
	if !frozenRestart {
		t.Fatal("frozen status lost across restart!")
	}

	// 6. Explicit unfreeze
	if errUnfreeze := restarted.UnfreezeProvider(ctx, "prov-freeze"); errUnfreeze != nil {
		t.Fatalf("unfreeze failed: %v", errUnfreeze)
	}
	if errUnfreezeM := restarted.UnfreezeModel(ctx, "prov-freeze", "model-freeze"); errUnfreezeM != nil {
		t.Fatalf("unfreeze model failed: %v", errUnfreezeM)
	}
	frozenAfter, _ := restarted.IsFrozen(ctx, "prov-freeze", "model-freeze")
	if frozenAfter {
		t.Fatal("provider/model still frozen after explicit unfreeze")
	}

	// Negative actual amount is rejected
	_, _ = restarted.Reserve(ctx, ReservationRequest{
		ReservationKey: "res-neg",
		Provider:       "prov-freeze",
		AmountNanos:    100_000_000,
	})
	if errNeg := restarted.Settle(ctx, "res-neg", -500, 0, 0); errNeg == nil || !errors.Is(errNeg, ErrInvalidCost) {
		t.Fatalf("expected ErrInvalidCost on negative settle, got %v", errNeg)
	}
}

func TestReservationStateMachine(t *testing.T) {
	// A4: Reservation lifecycle state machine & terminal transitions
	dir := t.TempDir()
	store, err := NewPersistentBudgetStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// 1. reserved -> settled
	_, _ = store.Reserve(ctx, ReservationRequest{ReservationKey: "res-sm-1", Provider: "prov-sm", AmountNanos: 100})
	if err := store.Settle(ctx, "res-sm-1", 100, 0, 0); err != nil {
		t.Fatalf("settle valid failed: %v", err)
	}
	// settled -> released (INVALID)
	if err := store.Release(ctx, "res-sm-1", "abort"); err == nil || !errors.Is(err, ErrInvalidStateTransition) {
		t.Fatalf("expected ErrInvalidStateTransition when releasing settled reservation, got %v", err)
	}
	// settled -> settled with different amount (INVALID)
	if err := store.Settle(ctx, "res-sm-1", 200, 0, 0); err == nil || !errors.Is(err, ErrInvalidStateTransition) {
		t.Fatalf("expected ErrInvalidStateTransition on different amount resettle, got %v", err)
	}
	// settled -> settled with same amount (IDEMPOTENT)
	if err := store.Settle(ctx, "res-sm-1", 100, 0, 0); err != nil {
		t.Fatalf("expected idempotent settle to succeed, got %v", err)
	}

	// 2. reserved -> released
	_, _ = store.Reserve(ctx, ReservationRequest{ReservationKey: "res-sm-2", Provider: "prov-sm", AmountNanos: 200})
	if err := store.Release(ctx, "res-sm-2", "canceled"); err != nil {
		t.Fatalf("release valid failed: %v", err)
	}
	// released -> settled (INVALID)
	if err := store.Settle(ctx, "res-sm-2", 200, 0, 0); err == nil || !errors.Is(err, ErrInvalidStateTransition) {
		t.Fatalf("expected ErrInvalidStateTransition when settling released reservation, got %v", err)
	}
	// released -> released (IDEMPOTENT)
	if err := store.Release(ctx, "res-sm-2", "canceled"); err != nil {
		t.Fatalf("expected idempotent release to succeed, got %v", err)
	}
}

func TestPersistenceFaultToleranceAndSanitization(t *testing.T) {
	// A5: Corrupt file fail-closed, temp file recovery, and secret sanitization
	dir := t.TempDir()

	// 1. Create a store and save normal data
	store, err := NewPersistentBudgetStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, _ = store.Reserve(ctx, ReservationRequest{
		ReservationKey: "req-clean",
		RequestID:      "id-clean",
		Provider:       "p-clean",
		Model:          "m-clean",
		AmountNanos:    1_000_000,
	})

	// 2. Inspect raw file: MUST NOT contain sensitive fields like api_key or proxy_url
	data, errRead := os.ReadFile(filepath.Join(dir, "budget_store.json"))
	if errRead != nil {
		t.Fatal(errRead)
	}
	dataStr := string(data)
	if containsAny(dataStr, "api_key", "bearer", "password", "proxy_url") {
		t.Fatalf("budget file contains sensitive credentials: %s", dataStr)
	}

	// 3. Corrupt file: write invalid json
	if errCorrupt := os.WriteFile(filepath.Join(dir, "budget_store.json"), []byte("{corrupt-json"), 0o600); errCorrupt != nil {
		t.Fatal(errCorrupt)
	}

	// 4. Starting store on corrupt file MUST FAIL CLOSED!
	_, errCorruptStore := NewPersistentBudgetStore(dir)
	if errCorruptStore == nil {
		t.Fatal("expected NewPersistentBudgetStore to fail closed on corrupt JSON")
	}

	// 5. Abandoned tmp file recovery
	tmpPath := filepath.Join(dir, "budget_store.json.tmp.9999.12345")
	_ = os.WriteFile(tmpPath, []byte("temporary"), 0o600)
	// Repair budget file with empty valid JSON
	_ = os.WriteFile(filepath.Join(dir, "budget_store.json"), []byte("{}"), 0o600)
	recoveredStore, errRecover := NewPersistentBudgetStore(dir)
	if errRecover != nil {
		t.Fatalf("recovery failed: %v", errRecover)
	}
	if _, errStat := os.Stat(tmpPath); !os.IsNotExist(errStat) {
		t.Fatal("expected abandoned tmp file to be cleaned up on store initialization")
	}
	_ = recoveredStore
}

func containsAny(s string, substrings ...string) bool {
	lower := strings.ToLower(s)
	for _, sub := range substrings {
		if sub != "" && strings.Contains(lower, strings.ToLower(sub)) {
			return true
		}
	}
	return false
}

func TestConcurrentReserveNoOverselling(t *testing.T) {
	// 33. 并发不能超卖: 50 个并发 goroutine 试图预订 $1.00，但日限额只有 $10.00
	dir := t.TempDir()
	store, err := NewPersistentBudgetStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	dailyLimit := int64(10_000_000_000)   // $10.00 max
	reserveAmount := int64(1_000_000_000) // $1.00 each

	concurrency := 50
	var wg sync.WaitGroup
	var successfulReservations int64
	var mu sync.Mutex

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, err := store.Reserve(ctx, ReservationRequest{
				ReservationKey:  fmt.Sprintf("concurrency-%d", idx),
				Provider:        "p-concurrent",
				AmountNanos:     reserveAmount,
				DailyLimitNanos: dailyLimit,
			})
			if err == nil {
				mu.Lock()
				successfulReservations++
				mu.Unlock()
			}
		}(i)
	}

	wg.Wait()

	if successfulReservations > 10 {
		t.Fatalf("overselling occurred! expected <= 10 successful reservations, got %d", successfulReservations)
	}

	spent, _ := store.CurrentProviderDailyAt(ctx, "p-concurrent", time.Now().UTC())
	if spent > dailyLimit {
		t.Fatalf("daily spent (%d) exceeded limit (%d)", spent, dailyLimit)
	}
}

func TestUTCDayAndMonthSwitch(t *testing.T) {
	// 44 & 45: UTC 日和月切换
	t1 := time.Date(2026, 10, 29, 23, 59, 0, 0, time.UTC)
	t2 := time.Date(2026, 10, 30, 0, 1, 0, 0, time.UTC)
	if isSameUTCDay(t1, t2) {
		t.Error("23:59 and 00:01 next day should NOT be same UTC day")
	}
	if !isSameUTCMonth(t1, t2) {
		t.Error("Oct 29 and Oct 30 should be same UTC month")
	}

	tNov := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if isSameUTCMonth(t1, tNov) {
		t.Error("Oct and Nov should NOT be same UTC month")
	}
}
