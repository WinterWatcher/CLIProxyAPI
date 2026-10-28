package logging

import (
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

var (
	warnThrottleMu   sync.Mutex
	warnThrottleMap  = make(map[string]time.Time)
	warnThrottleDrop = make(map[string]int)
)

// WarnThrottled logs a warning message at most once per interval for the same key.
func WarnThrottled(key string, interval time.Duration, msg string) bool {
	warnThrottleMu.Lock()
	last, exists := warnThrottleMap[key]
	now := time.Now()
	if exists && now.Sub(last) < interval {
		warnThrottleDrop[key]++
		warnThrottleMu.Unlock()
		return false
	}
	dropped := warnThrottleDrop[key]
	warnThrottleDrop[key] = 0
	warnThrottleMap[key] = now
	warnThrottleMu.Unlock()

	if dropped > 0 {
		log.Warnf("%s (suppressed %d similar warnings in last %v)", msg, dropped, interval)
	} else {
		log.Warn(msg)
	}
	return true
}

// ResetWarnThrottling clears the warning throttle cache (primarily for tests).
func ResetWarnThrottling() {
	warnThrottleMu.Lock()
	defer warnThrottleMu.Unlock()
	warnThrottleMap = make(map[string]time.Time)
	warnThrottleDrop = make(map[string]int)
}
