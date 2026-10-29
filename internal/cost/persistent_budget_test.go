package cost

import (
	"context"
	"fmt"
	"math"
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
