package logging

import (
	"testing"
	"time"
)

func TestWarnThrottled(t *testing.T) {
	ResetWarnThrottling()

	key := "test-key-warn"
	interval := 50 * time.Millisecond

	// First call should log (not throttled)
	if !WarnThrottled(key, interval, "first warning") {
		t.Fatal("first call should not be throttled")
	}

	// Immediate subsequent calls should be throttled
	for i := 0; i < 5; i++ {
		if WarnThrottled(key, interval, "repeated warning") {
			t.Fatalf("call %d within interval should be throttled", i)
		}
	}

	// Wait for interval to elapse
	time.Sleep(60 * time.Millisecond)

	// Next call after interval should log
	if !WarnThrottled(key, interval, "after interval warning") {
		t.Fatal("call after interval should not be throttled")
	}
}
