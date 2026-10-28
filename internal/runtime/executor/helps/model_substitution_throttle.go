package helps

import (
	"strings"
	"sync"
	"time"
)

const (
	DefaultModelSubstitutionWarnWindow = 30 * time.Minute
	defaultModelSubstitutionMaxEntries = 10000
)

type modelSubstitutionKey struct {
	provider  string
	requested string
	served    string
	authIndex string
}

type modelSubstitutionEntry struct {
	lastWarn   time.Time
	suppressed int
}

type ModelSubstitutionThrottle struct {
	mu      sync.Mutex
	nowFunc func() time.Time
	window  time.Duration
	entries map[modelSubstitutionKey]*modelSubstitutionEntry
}

func NewModelSubstitutionThrottle(nowFunc func() time.Time, window time.Duration) *ModelSubstitutionThrottle {
	if nowFunc == nil {
		nowFunc = time.Now
	}
	if window <= 0 {
		window = DefaultModelSubstitutionWarnWindow
	}
	return &ModelSubstitutionThrottle{
		nowFunc: nowFunc,
		window:  window,
		entries: make(map[modelSubstitutionKey]*modelSubstitutionEntry),
	}
}

// Record checks whether a warning should be emitted for the given key at current time.
// Returns (shouldWarn, suppressedCount).
// If shouldWarn is true, the caller should log a warning.
// If suppressedCount > 0, it should be appended to the warning as suppressed_repeats=N.
func (t *ModelSubstitutionThrottle) Record(provider, requested, served, authIndex string) (bool, int) {
	if t == nil {
		return true, 0
	}
	key := modelSubstitutionKey{
		provider:  strings.TrimSpace(provider),
		requested: strings.TrimSpace(requested),
		served:    strings.TrimSpace(served),
		authIndex: strings.TrimSpace(authIndex),
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.nowFunc()
	entry, exists := t.entries[key]
	if !exists {
		// First occurrence
		if len(t.entries) >= defaultModelSubstitutionMaxEntries {
			for k, v := range t.entries {
				if now.Sub(v.lastWarn) >= t.window {
					delete(t.entries, k)
				}
			}
			if len(t.entries) >= defaultModelSubstitutionMaxEntries {
				clear(t.entries)
			}
		}
		t.entries[key] = &modelSubstitutionEntry{
			lastWarn:   now,
			suppressed: 0,
		}
		return true, 0
	}

	elapsed := now.Sub(entry.lastWarn)
	if elapsed < t.window {
		// Inside suppression window, increment count and suppress
		entry.suppressed++
		return false, 0
	}

	// Window elapsed: emit new warning and report suppressed repeats
	suppressed := entry.suppressed
	entry.lastWarn = now
	entry.suppressed = 0
	return true, suppressed
}

func (t *ModelSubstitutionThrottle) Reset() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries = make(map[modelSubstitutionKey]*modelSubstitutionEntry)
}

func (t *ModelSubstitutionThrottle) SetClock(nowFunc func() time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if nowFunc == nil {
		nowFunc = time.Now
	}
	t.nowFunc = nowFunc
}

func (t *ModelSubstitutionThrottle) SetWindow(w time.Duration) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if w <= 0 {
		w = DefaultModelSubstitutionWarnWindow
	}
	t.window = w
}

var globalModelSubstitutionThrottle = NewModelSubstitutionThrottle(time.Now, DefaultModelSubstitutionWarnWindow)

// For testing purposes
func ResetGlobalModelSubstitutionThrottle() {
	globalModelSubstitutionThrottle.Reset()
}

func SetGlobalModelSubstitutionThrottleClock(nowFunc func() time.Time) {
	globalModelSubstitutionThrottle.SetClock(nowFunc)
}

func SetGlobalModelSubstitutionThrottleWindow(w time.Duration) {
	globalModelSubstitutionThrottle.SetWindow(w)
}
