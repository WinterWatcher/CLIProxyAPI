package helps

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

// 1. 第一次替换输出一条WARN
func TestModelSubstitutionThrottle_FirstWarning(t *testing.T) {
	hook := setupResponseModelLoggerHook(t)
	now := time.Unix(1_700_000_000, 0)
	SetGlobalModelSubstitutionThrottleClock(func() time.Time { return now })
	ResetGlobalModelSubstitutionThrottle()
	t.Cleanup(func() {
		SetGlobalModelSubstitutionThrottleClock(time.Now)
		ResetGlobalModelSubstitutionThrottle()
	})

	ctx := context.Background()
	auth := &cliproxyauth.Auth{ID: "auth-1", Index: "auth-index-1", Provider: "antigravity"}
	reporter := NewExecutorUsageReporter(ctx, mockTestExecutor{provider: "antigravity"}, "gemini-3.8-flash-high", auth)
	reporter.ObserveResponseModel([]byte(`{"model":"gemini-3.8-flash-n"}`))
	reporter.Publish(ctx, usage.Detail{TotalTokens: 10})

	warnings := substitutionWarnings(hook)
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %#v", len(warnings), warnings)
	}
	if strings.Contains(warnings[0], "suppressed_repeats") {
		t.Fatalf("first warning must NOT contain suppressed_repeats: %q", warnings[0])
	}
	if !strings.Contains(warnings[0], "gemini-3.8-flash-n") || !strings.Contains(warnings[0], "gemini-3.8-flash-high") {
		t.Fatalf("warning missing model info: %q", warnings[0])
	}
	if !strings.Contains(warnings[0], "auth_index=auth-index-1") {
		t.Fatalf("warning missing auth_index: %q", warnings[0])
	}
}

// 2. 相同Key连续重复只输出一条，重复次数正确累计
func TestModelSubstitutionThrottle_RepeatedWithinWindowSuppressed(t *testing.T) {
	hook := setupResponseModelLoggerHook(t)
	now := time.Unix(1_700_000_000, 0)
	SetGlobalModelSubstitutionThrottleClock(func() time.Time { return now })
	ResetGlobalModelSubstitutionThrottle()
	t.Cleanup(func() {
		SetGlobalModelSubstitutionThrottleClock(time.Now)
		ResetGlobalModelSubstitutionThrottle()
	})

	ctx := context.Background()
	auth := &cliproxyauth.Auth{ID: "auth-1", Index: "auth-index-1", Provider: "antigravity"}

	for i := 0; i < 5; i++ {
		reporter := NewExecutorUsageReporter(ctx, mockTestExecutor{provider: "antigravity"}, "gemini-3.8-flash-high", auth)
		reporter.ObserveResponseModel([]byte(`{"model":"gemini-3.8-flash-n"}`))
		reporter.Publish(ctx, usage.Detail{TotalTokens: 10})
	}

	warnings := substitutionWarnings(hook)
	if len(warnings) != 1 {
		t.Fatalf("expected exactly 1 warning within window, got %d: %#v", len(warnings), warnings)
	}
}

// 3. 时间窗口结束后再次输出，第二次输出包含suppressed_repeats
func TestModelSubstitutionThrottle_WindowElapsesEmitsSuppressedRepeats(t *testing.T) {
	hook := setupResponseModelLoggerHook(t)
	now := time.Unix(1_700_000_000, 0)
	currentNow := now
	SetGlobalModelSubstitutionThrottleClock(func() time.Time { return currentNow })
	ResetGlobalModelSubstitutionThrottle()
	t.Cleanup(func() {
		SetGlobalModelSubstitutionThrottleClock(time.Now)
		ResetGlobalModelSubstitutionThrottle()
	})

	ctx := context.Background()
	auth := &cliproxyauth.Auth{ID: "auth-1", Index: "auth-index-1", Provider: "antigravity"}

	// First call at T=0
	r1 := NewExecutorUsageReporter(ctx, mockTestExecutor{provider: "antigravity"}, "gemini-3.8-flash-high", auth)
	r1.ObserveResponseModel([]byte(`{"model":"gemini-3.8-flash-n"}`))
	r1.Publish(ctx, usage.Detail{TotalTokens: 10})

	// 3 repeated calls within window at T=5m
	currentNow = now.Add(5 * time.Minute)
	for i := 0; i < 3; i++ {
		r := NewExecutorUsageReporter(ctx, mockTestExecutor{provider: "antigravity"}, "gemini-3.8-flash-high", auth)
		r.ObserveResponseModel([]byte(`{"model":"gemini-3.8-flash-n"}`))
		r.Publish(ctx, usage.Detail{TotalTokens: 10})
	}

	if len(substitutionWarnings(hook)) != 1 {
		t.Fatalf("expected 1 warning before window elapsed, got %d", len(substitutionWarnings(hook)))
	}

	// Advance clock past 30-minute window to T=31m
	currentNow = now.Add(31 * time.Minute)
	r2 := NewExecutorUsageReporter(ctx, mockTestExecutor{provider: "antigravity"}, "gemini-3.8-flash-high", auth)
	r2.ObserveResponseModel([]byte(`{"model":"gemini-3.8-flash-n"}`))
	r2.Publish(ctx, usage.Detail{TotalTokens: 10})

	warnings := substitutionWarnings(hook)
	if len(warnings) != 2 {
		t.Fatalf("expected 2 warnings after window elapsed, got %d: %#v", len(warnings), warnings)
	}
	secondWarning := warnings[1]
	if !strings.Contains(secondWarning, "suppressed_repeats=3") {
		t.Fatalf("second warning must contain suppressed_repeats=3: %q", secondWarning)
	}
}

// 4. 如果N为0，窗口结束后再次输出不要输出suppressed_repeats
func TestModelSubstitutionThrottle_WindowElapsesWithoutRepeatsNoSuppressed(t *testing.T) {
	hook := setupResponseModelLoggerHook(t)
	now := time.Unix(1_700_000_000, 0)
	currentNow := now
	SetGlobalModelSubstitutionThrottleClock(func() time.Time { return currentNow })
	ResetGlobalModelSubstitutionThrottle()
	t.Cleanup(func() {
		SetGlobalModelSubstitutionThrottleClock(time.Now)
		ResetGlobalModelSubstitutionThrottle()
	})

	ctx := context.Background()
	auth := &cliproxyauth.Auth{ID: "auth-1", Index: "auth-index-1", Provider: "antigravity"}

	// First call at T=0
	r1 := NewExecutorUsageReporter(ctx, mockTestExecutor{provider: "antigravity"}, "gemini-3.8-flash-high", auth)
	r1.ObserveResponseModel([]byte(`{"model":"gemini-3.8-flash-n"}`))
	r1.Publish(ctx, usage.Detail{TotalTokens: 10})

	// No calls during window. Jump to T=35m
	currentNow = now.Add(35 * time.Minute)
	r2 := NewExecutorUsageReporter(ctx, mockTestExecutor{provider: "antigravity"}, "gemini-3.8-flash-high", auth)
	r2.ObserveResponseModel([]byte(`{"model":"gemini-3.8-flash-n"}`))
	r2.Publish(ctx, usage.Detail{TotalTokens: 10})

	warnings := substitutionWarnings(hook)
	if len(warnings) != 2 {
		t.Fatalf("expected 2 warnings, got %d: %#v", len(warnings), warnings)
	}
	if strings.Contains(warnings[1], "suppressed_repeats") {
		t.Fatalf("second warning without repeats must NOT contain suppressed_repeats: %q", warnings[1])
	}
}

// 5. 不同的auth_index、served model、requested model、provider分别统计，不能互相抑制
func TestModelSubstitutionThrottle_DistinctKeysNotSuppressed(t *testing.T) {
	hook := setupResponseModelLoggerHook(t)
	now := time.Unix(1_700_000_000, 0)
	SetGlobalModelSubstitutionThrottleClock(func() time.Time { return now })
	ResetGlobalModelSubstitutionThrottle()
	t.Cleanup(func() {
		SetGlobalModelSubstitutionThrottleClock(time.Now)
		ResetGlobalModelSubstitutionThrottle()
	})

	ctx := context.Background()

	// Base
	auth1 := &cliproxyauth.Auth{ID: "auth-1", Index: "auth-index-1", Provider: "antigravity"}
	r1 := NewExecutorUsageReporter(ctx, mockTestExecutor{provider: "antigravity"}, "gemini-3.8-flash-high", auth1)
	r1.ObserveResponseModel([]byte(`{"model":"gemini-3.8-flash-n"}`))
	r1.Publish(ctx, usage.Detail{TotalTokens: 1})

	// Different auth_index
	auth2 := &cliproxyauth.Auth{ID: "auth-2", Index: "auth-index-2", Provider: "antigravity"}
	r2 := NewExecutorUsageReporter(ctx, mockTestExecutor{provider: "antigravity"}, "gemini-3.8-flash-high", auth2)
	r2.ObserveResponseModel([]byte(`{"model":"gemini-3.8-flash-n"}`))
	r2.Publish(ctx, usage.Detail{TotalTokens: 1})

	// Different served model
	r3 := NewExecutorUsageReporter(ctx, mockTestExecutor{provider: "antigravity"}, "gemini-3.8-flash-high", auth1)
	r3.ObserveResponseModel([]byte(`{"model":"gemini-3.8-flash-other"}`))
	r3.Publish(ctx, usage.Detail{TotalTokens: 1})

	// Different requested model
	r4 := NewExecutorUsageReporter(ctx, mockTestExecutor{provider: "antigravity"}, "gemini-3.8-pro", auth1)
	r4.ObserveResponseModel([]byte(`{"model":"gemini-3.8-flash-n"}`))
	r4.Publish(ctx, usage.Detail{TotalTokens: 1})

	// Different provider
	r5 := NewExecutorUsageReporter(ctx, mockTestExecutor{provider: "other-provider"}, "gemini-3.8-flash-high", auth1)
	r5.ObserveResponseModel([]byte(`{"model":"gemini-3.8-flash-n"}`))
	r5.Publish(ctx, usage.Detail{TotalTokens: 1})

	warnings := substitutionWarnings(hook)
	if len(warnings) != 5 {
		t.Fatalf("expected 5 distinct warnings, got %d: %#v", len(warnings), warnings)
	}
}

// 6. 已知正常alias映射不输出WARN
func TestModelSubstitutionThrottle_KnownAliasDoesNotWarn(t *testing.T) {
	hook := setupResponseModelLoggerHook(t)
	now := time.Unix(1_700_000_000, 0)
	SetGlobalModelSubstitutionThrottleClock(func() time.Time { return now })
	ResetGlobalModelSubstitutionThrottle()
	t.Cleanup(func() {
		SetGlobalModelSubstitutionThrottleClock(time.Now)
		ResetGlobalModelSubstitutionThrottle()
	})

	ctx := context.Background()
	auth := &cliproxyauth.Auth{ID: "auth-1", Index: "auth-index-1", Provider: "antigravity"}

	// 6.1 Dated alias
	r1 := NewExecutorUsageReporter(ctx, mockTestExecutor{provider: "antigravity"}, "gemini-2.5-pro(high)", auth)
	r1.ObserveResponseModel([]byte(`{"model":"gemini-2.5-pro-2026-05-14"}`))
	r1.Publish(ctx, usage.Detail{TotalTokens: 1})

	// 6.2 -latest alias
	r2 := NewExecutorUsageReporter(ctx, mockTestExecutor{provider: "antigravity"}, "gemini-2.5-pro-latest", auth)
	r2.ObserveResponseModel([]byte(`{"model":"gemini-2.5-pro"}`))
	r2.Publish(ctx, usage.Detail{TotalTokens: 1})

	warnings := substitutionWarnings(hook)
	if len(warnings) != 0 {
		t.Fatalf("known alias must not emit warning, got: %#v", warnings)
	}
}

// 7. 并发调用不会产生data race
func TestModelSubstitutionThrottle_Concurrency(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	throttle := NewModelSubstitutionThrottle(func() time.Time { return now }, 30*time.Minute)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			provider := "p1"
			if id%2 == 0 {
				provider = "p2"
			}
			throttle.Record(provider, "m-req", "m-srv", "idx-1")
		}(i)
	}
	wg.Wait()
}

type mockTestExecutor struct {
	provider string
}

func (m mockTestExecutor) Identifier() string {
	return m.provider
}
