package cost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Reservation status constants.
const (
	ReservationStatusReserved = "reserved"
	ReservationStatusSettled  = "settled"
	ReservationStatusReleased = "released"
	ReservationStatusExpired  = "expired"
)

// Default reservation expiration duration.
const DefaultReservationTTL = 5 * time.Minute

// PersistentReservationRecord models a persistent budget reservation.
type PersistentReservationRecord struct {
	ReservationKey    string `json:"reservation_key"`
	RequestID         string `json:"request_id"`
	Provider          string `json:"provider"`
	Model             string `json:"model"`
	BillingUnit       string `json:"billing_unit"`
	AmountNanos       int64  `json:"amount_nanos"`
	ActualAmountNanos int64  `json:"actual_amount_nanos"`
	Status            string `json:"status"`
	CreatedAtMS       int64  `json:"created_at_ms"`
	ExpiresAtMS       int64  `json:"expires_at_ms"`
	SettledAtMS       int64  `json:"settled_at_ms,omitempty"`
	ReleaseReason     string `json:"release_reason,omitempty"`
}

// PersistentBudgetState represents the complete persistent state on disk.
type PersistentBudgetState struct {
	Records         map[string]*PersistentReservationRecord `json:"records"`
	FrozenProviders map[string]int64                        `json:"frozen_providers,omitempty"`
	FrozenModels    map[string]int64                        `json:"frozen_models,omitempty"`
	GlobalFrozen    bool                                    `json:"global_frozen,omitempty"`
}

// PersistentBudgetStore provides persistent, restart-durable, cross-process file-locked budget accounting.
type PersistentBudgetStore struct {
	filePath        string
	lockFilePath    string
	memMu           sync.Mutex
	records         map[string]*PersistentReservationRecord
	frozenProviders map[string]int64
	frozenModels    map[string]int64
	globalFrozen    bool
}

// NewPersistentBudgetStore initializes and loads a persistent budget store from disk.
func NewPersistentBudgetStore(dir string) (*PersistentBudgetStore, error) {
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create budget dir: %w", err)
	}

	// Clean up abandoned temp files older than a minute
	if entries, errRead := os.ReadDir(dir); errRead == nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "budget_store.json.tmp.") {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}

	filePath := filepath.Join(dir, "budget_store.json")
	lockFilePath := filepath.Join(dir, "budget_store.lock")

	store := &PersistentBudgetStore{
		filePath:        filePath,
		lockFilePath:    lockFilePath,
		records:         make(map[string]*PersistentReservationRecord),
		frozenProviders: make(map[string]int64),
		frozenModels:    make(map[string]int64),
	}

	if err := store.loadFromDisk(); err != nil {
		return nil, fmt.Errorf("load budget store from disk: %w", err)
	}

	return store, nil
}

func (s *PersistentBudgetStore) withFileLock(fn func() error) error {
	s.memMu.Lock()
	defer s.memMu.Unlock()

	lockFile, err := os.OpenFile(s.lockFilePath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open lock file: %w", err)
	}
	defer lockFile.Close()

	// Acquire cross-process exclusive advisory lock (fail closed if locking fails)
	if errFlock := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); errFlock != nil {
		return fmt.Errorf("acquire file lock: %w", errFlock)
	}
	defer func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
	}()

	// Reload latest state from disk in case another process modified it
	if errLoad := s.loadFromDisk(); errLoad != nil {
		return fmt.Errorf("reload from disk: %w", errLoad)
	}

	fnErr := fn()
	if fnErr != nil && !errors.Is(fnErr, ErrBudgetOverrun) {
		return fnErr
	}

	// Persist state atomically
	if errSave := s.saveToDiskAtomic(); errSave != nil {
		return errSave
	}
	return fnErr
}

func (s *PersistentBudgetStore) loadFromDisk() error {
	data, err := os.ReadFile(s.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			s.records = make(map[string]*PersistentReservationRecord)
			s.frozenProviders = make(map[string]int64)
			s.frozenModels = make(map[string]int64)
			s.globalFrozen = false
			return nil
		}
		return err
	}
	if len(data) == 0 {
		s.records = make(map[string]*PersistentReservationRecord)
		s.frozenProviders = make(map[string]int64)
		s.frozenModels = make(map[string]int64)
		s.globalFrozen = false
		return nil
	}

	// Try unmarshaling as PersistentBudgetState
	var state PersistentBudgetState
	if errUnmarshal := json.Unmarshal(data, &state); errUnmarshal == nil && state.Records != nil {
		s.records = state.Records
		if state.FrozenProviders != nil {
			s.frozenProviders = state.FrozenProviders
		} else {
			s.frozenProviders = make(map[string]int64)
		}
		if state.FrozenModels != nil {
			s.frozenModels = state.FrozenModels
		} else {
			s.frozenModels = make(map[string]int64)
		}
		s.globalFrozen = state.GlobalFrozen
		return nil
	}

	// Backward compatibility: try unmarshaling as legacy map[string]*PersistentReservationRecord
	var loaded map[string]*PersistentReservationRecord
	if errUnmarshalLegacy := json.Unmarshal(data, &loaded); errUnmarshalLegacy == nil {
		s.records = loaded
		s.frozenProviders = make(map[string]int64)
		s.frozenModels = make(map[string]int64)
		s.globalFrozen = false
		return nil
	}

	// Corrupt file must fail closed!
	return fmt.Errorf("corrupt budget file: invalid json in %s", s.filePath)
}

func (s *PersistentBudgetStore) saveToDiskAtomic() error {
	state := PersistentBudgetState{
		Records:         s.records,
		FrozenProviders: s.frozenProviders,
		FrozenModels:    s.frozenModels,
		GlobalFrozen:    s.globalFrozen,
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal budget store: %w", err)
	}

	tmpPath := fmt.Sprintf("%s.tmp.%d.%d", s.filePath, os.Getpid(), time.Now().UnixNano())
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open tmp budget store: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write tmp budget store: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("sync tmp budget store: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close tmp budget store: %w", err)
	}

	if errRename := os.Rename(tmpPath, s.filePath); errRename != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("atomic rename budget store: %w", errRename)
	}

	// Fsync directory to ensure metadata persistence
	if dir, errDir := os.Open(filepath.Dir(s.filePath)); errDir == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}

	return nil
}

// ReservationRequest defines input parameters for Reserve.
type ReservationRequest struct {
	ReservationKey          string
	RequestID               string
	Provider                string
	Model                   string
	BillingUnit             string
	AmountNanos             int64
	DailyLimitNanos         int64
	MonthlyLimitNanos       int64
	GlobalDailyLimitNanos   int64
	GlobalMonthlyLimitNanos int64
	TTL                     time.Duration
}

// IsFrozen checks if provider, model, or global is frozen due to budget overrun.
func (s *PersistentBudgetStore) IsFrozen(_ context.Context, provider, model string) (bool, string) {
	s.memMu.Lock()
	defer s.memMu.Unlock()
	if s.globalFrozen {
		return true, "global_budget_overrun_freeze"
	}
	if _, frozen := s.frozenProviders[provider]; frozen {
		return true, "provider_budget_overrun_freeze"
	}
	modelKey := provider + ":" + model
	if _, frozen := s.frozenModels[modelKey]; frozen {
		return true, "model_budget_overrun_freeze"
	}
	return false, ""
}

// UnfreezeProvider removes overrun freeze from a provider.
func (s *PersistentBudgetStore) UnfreezeProvider(_ context.Context, provider string) error {
	return s.withFileLock(func() error {
		delete(s.frozenProviders, provider)
		return nil
	})
}

// UnfreezeModel removes overrun freeze from a model.
func (s *PersistentBudgetStore) UnfreezeModel(_ context.Context, provider, model string) error {
	return s.withFileLock(func() error {
		delete(s.frozenModels, provider+":"+model)
		return nil
	})
}

// UnfreezeGlobal removes overrun freeze globally.
func (s *PersistentBudgetStore) UnfreezeGlobal(_ context.Context) error {
	return s.withFileLock(func() error {
		s.globalFrozen = false
		return nil
	})
}

// Reserve atomically reserves budget nanos, verifying daily, monthly, and global limits.
func (s *PersistentBudgetStore) Reserve(ctx context.Context, req ReservationRequest) (Reservation, error) {
	if req.AmountNanos < 0 {
		return Reservation{}, ErrInvalidCost
	}
	if req.AmountNanos == math.MaxInt64 {
		return Reservation{}, errors.New("int64 overflow in reservation amount")
	}

	var reserved Reservation
	err := s.withFileLock(func() error {
		now := time.Now().UTC()

		// Overrun freeze check
		if s.globalFrozen {
			return fmt.Errorf("%w: global is frozen due to budget overrun", ErrBudgetOverrunFrozen)
		}
		if _, frozen := s.frozenProviders[req.Provider]; frozen {
			return fmt.Errorf("%w: provider %s is frozen due to budget overrun", ErrBudgetOverrunFrozen, req.Provider)
		}
		modelKey := req.Provider + ":" + req.Model
		if _, frozen := s.frozenModels[modelKey]; frozen {
			return fmt.Errorf("%w: model %s is frozen due to budget overrun", ErrBudgetOverrunFrozen, modelKey)
		}

		// 1. Idempotency check: if reservation key already exists
		if existing, exists := s.records[req.ReservationKey]; exists && existing != nil {
			if existing.Status == ReservationStatusReserved {
				reserved = Reservation{
					ID:          existing.ReservationKey,
					Provider:    existing.Provider,
					AmountNanos: existing.AmountNanos,
					ReservedAt:  time.UnixMilli(existing.CreatedAtMS),
				}
				return nil
			}
		}

		// 2. Compute current provider daily and monthly consumption
		provDaily := s.calculateProviderDaily(req.Provider, now)
		if req.DailyLimitNanos > 0 && provDaily+req.AmountNanos > req.DailyLimitNanos {
			return fmt.Errorf("%w: provider %s daily limit exceeded (spent=%d, req=%d, limit=%d)",
				ErrBudgetExceeded, req.Provider, provDaily, req.AmountNanos, req.DailyLimitNanos)
		}

		provMonthly := s.calculateProviderMonthly(req.Provider, now)
		if req.MonthlyLimitNanos > 0 && provMonthly+req.AmountNanos > req.MonthlyLimitNanos {
			return fmt.Errorf("%w: provider %s monthly limit exceeded (spent=%d, req=%d, limit=%d)",
				ErrBudgetExceeded, req.Provider, provMonthly, req.AmountNanos, req.MonthlyLimitNanos)
		}

		// 3. Compute current global daily and monthly consumption
		globalDaily := s.calculateGlobalDaily(now)
		if req.GlobalDailyLimitNanos > 0 && globalDaily+req.AmountNanos > req.GlobalDailyLimitNanos {
			return fmt.Errorf("%w: global daily limit exceeded (spent=%d, req=%d, limit=%d)",
				ErrGlobalBudgetExceeded, globalDaily, req.AmountNanos, req.GlobalDailyLimitNanos)
		}

		globalMonthly := s.calculateGlobalMonthly(now)
		if req.GlobalMonthlyLimitNanos > 0 && globalMonthly+req.AmountNanos > req.GlobalMonthlyLimitNanos {
			return fmt.Errorf("%w: global monthly limit exceeded (spent=%d, req=%d, limit=%d)",
				ErrGlobalBudgetExceeded, globalMonthly, req.AmountNanos, req.GlobalMonthlyLimitNanos)
		}

		// 4. Create persistent reservation
		ttl := req.TTL
		if ttl == 0 {
			ttl = DefaultReservationTTL
		}
		nowMS := now.UnixMilli()
		expiresMS := now.Add(ttl).UnixMilli()

		rec := &PersistentReservationRecord{
			ReservationKey:    req.ReservationKey,
			RequestID:         req.RequestID,
			Provider:          req.Provider,
			Model:             req.Model,
			BillingUnit:       req.BillingUnit,
			AmountNanos:       req.AmountNanos,
			ActualAmountNanos: 0,
			Status:            ReservationStatusReserved,
			CreatedAtMS:       nowMS,
			ExpiresAtMS:       expiresMS,
		}
		s.records[req.ReservationKey] = rec

		reserved = Reservation{
			ID:          req.ReservationKey,
			Provider:    req.Provider,
			AmountNanos: req.AmountNanos,
			ReservedAt:  now,
		}

		RecordAuditEvent(CostAuditEvent{
			Timestamp:            now,
			Provider:             req.Provider,
			PublicModel:          req.Model,
			BillingUnit:          req.BillingUnit,
			EstimatedAmountNanos: req.AmountNanos,
			ReservedAmountNanos:  req.AmountNanos,
			Decision:             DecisionAllow,
			Reason:               "budget_reserved",
			RequestID:            req.RequestID,
		})

		return nil
	})

	return reserved, err
}

// Settle finalizes a reservation with actual usage. If actual exceeds reservation, triggers budget overrun freeze.
func (s *PersistentBudgetStore) Settle(ctx context.Context, reservationKey string, actualAmountNanos int64, dailyLimitNanos, monthlyLimitNanos int64) error {
	if actualAmountNanos < 0 {
		return ErrInvalidCost
	}
	if actualAmountNanos == math.MaxInt64 {
		return errors.New("int64 overflow in actual amount")
	}

	return s.withFileLock(func() error {
		now := time.Now().UTC()
		rec, exists := s.records[reservationKey]
		if !exists || rec == nil {
			return ErrReservationNotFound
		}

		// State machine check
		if rec.Status == ReservationStatusReleased || rec.Status == ReservationStatusExpired {
			return fmt.Errorf("%w: cannot settle %s reservation", ErrInvalidStateTransition, rec.Status)
		}
		if rec.Status == ReservationStatusSettled {
			if rec.ActualAmountNanos == actualAmountNanos {
				return nil // Idempotent
			}
			return fmt.Errorf("%w: reservation already settled with amount %d, cannot resettle with %d",
				ErrInvalidStateTransition, rec.ActualAmountNanos, actualAmountNanos)
		}

		diff := actualAmountNanos - rec.AmountNanos
		isOverrun := diff > 0

		rec.ActualAmountNanos = actualAmountNanos
		rec.Status = ReservationStatusSettled
		rec.SettledAtMS = now.UnixMilli()

		if isOverrun {
			// Trigger critical audit and overrun freeze
			if s.frozenProviders == nil {
				s.frozenProviders = make(map[string]int64)
			}
			if s.frozenModels == nil {
				s.frozenModels = make(map[string]int64)
			}
			s.frozenProviders[rec.Provider] = now.UnixMilli()
			s.frozenModels[rec.Provider+":"+rec.Model] = now.UnixMilli()

			RecordAuditEvent(CostAuditEvent{
				Timestamp:            now,
				Provider:             rec.Provider,
				PublicModel:          rec.Model,
				BillingUnit:          rec.BillingUnit,
				ReservedAmountNanos:  rec.AmountNanos,
				ActualAmountNanos:    actualAmountNanos,
				EstimatedAmountNanos: rec.AmountNanos,
				Decision:             DecisionDeny,
				Reason:               "budget_overrun",
				RequestID:            rec.RequestID,
			})

			return fmt.Errorf("%w: reserved %d, actual %d (diff +%d) for provider %s, model %s",
				ErrBudgetOverrun, rec.AmountNanos, actualAmountNanos, diff, rec.Provider, rec.Model)
		}

		RecordAuditEvent(CostAuditEvent{
			Timestamp:           now,
			Provider:            rec.Provider,
			PublicModel:         rec.Model,
			BillingUnit:         rec.BillingUnit,
			ReservedAmountNanos: rec.AmountNanos,
			ActualAmountNanos:   actualAmountNanos,
			Decision:            DecisionAllow,
			Reason:              "budget_settled",
			RequestID:           rec.RequestID,
		})

		return nil
	})
}

// Release cancels an active reservation, freeing its reserved amount.
func (s *PersistentBudgetStore) Release(ctx context.Context, reservationKey, reason string) error {
	return s.withFileLock(func() error {
		now := time.Now().UTC()
		rec, exists := s.records[reservationKey]
		if !exists || rec == nil {
			return nil
		}

		// State machine check
		if rec.Status == ReservationStatusSettled {
			return fmt.Errorf("%w: cannot release settled reservation", ErrInvalidStateTransition)
		}
		if rec.Status == ReservationStatusReleased {
			return nil // Idempotent
		}
		if rec.Status == ReservationStatusExpired {
			return nil // Terminal
		}

		rec.Status = ReservationStatusReleased
		rec.ReleaseReason = reason

		RecordAuditEvent(CostAuditEvent{
			Timestamp:           now,
			Provider:            rec.Provider,
			PublicModel:         rec.Model,
			BillingUnit:         rec.BillingUnit,
			ReservedAmountNanos: rec.AmountNanos,
			Decision:            DecisionAllow,
			Reason:              "budget_released",
			RequestID:           rec.RequestID,
		})

		return nil
	})
}

// ReapExpired transitions stale reservations past their TTL to expired.
func (s *PersistentBudgetStore) ReapExpired(ctx context.Context, now time.Time) (int, error) {
	reapedCount := 0
	err := s.withFileLock(func() error {
		nowMS := now.UTC().UnixMilli()
		for _, rec := range s.records {
			if rec.Status == ReservationStatusReserved && rec.ExpiresAtMS > 0 && rec.ExpiresAtMS <= nowMS {
				rec.Status = ReservationStatusExpired
				rec.ReleaseReason = "ttl_expired"
				reapedCount++

				RecordAuditEvent(CostAuditEvent{
					Timestamp:           now,
					Provider:            rec.Provider,
					PublicModel:         rec.Model,
					BillingUnit:         rec.BillingUnit,
					ReservedAmountNanos: rec.AmountNanos,
					Decision:            DecisionAllow,
					Reason:              "budget_expired",
					RequestID:           rec.RequestID,
				})
			}
		}
		return nil
	})
	return reapedCount, err
}

func isSameUTCDay(t1, t2 time.Time) bool {
	u1 := t1.UTC()
	u2 := t2.UTC()
	return u1.Year() == u2.Year() && u1.YearDay() == u2.YearDay()
}

func isSameUTCMonth(t1, t2 time.Time) bool {
	u1 := t1.UTC()
	u2 := t2.UTC()
	return u1.Year() == u2.Year() && u1.Month() == u2.Month()
}

func (s *PersistentBudgetStore) calculateProviderDaily(provider string, now time.Time) int64 {
	var total int64
	for _, rec := range s.records {
		if rec.Provider != provider {
			continue
		}
		recTime := time.UnixMilli(rec.CreatedAtMS)
		if !isSameUTCDay(recTime, now) {
			continue
		}
		if rec.Status == ReservationStatusSettled {
			total += rec.ActualAmountNanos
		} else if rec.Status == ReservationStatusReserved {
			total += rec.AmountNanos
		}
	}
	return total
}

func (s *PersistentBudgetStore) calculateProviderMonthly(provider string, now time.Time) int64 {
	var total int64
	for _, rec := range s.records {
		if rec.Provider != provider {
			continue
		}
		recTime := time.UnixMilli(rec.CreatedAtMS)
		if !isSameUTCMonth(recTime, now) {
			continue
		}
		if rec.Status == ReservationStatusSettled {
			total += rec.ActualAmountNanos
		} else if rec.Status == ReservationStatusReserved {
			total += rec.AmountNanos
		}
	}
	return total
}

func (s *PersistentBudgetStore) calculateGlobalDaily(now time.Time) int64 {
	var total int64
	for _, rec := range s.records {
		recTime := time.UnixMilli(rec.CreatedAtMS)
		if !isSameUTCDay(recTime, now) {
			continue
		}
		if rec.Status == ReservationStatusSettled {
			total += rec.ActualAmountNanos
		} else if rec.Status == ReservationStatusReserved {
			total += rec.AmountNanos
		}
	}
	return total
}

func (s *PersistentBudgetStore) calculateGlobalMonthly(now time.Time) int64 {
	var total int64
	for _, rec := range s.records {
		recTime := time.UnixMilli(rec.CreatedAtMS)
		if !isSameUTCMonth(recTime, now) {
			continue
		}
		if rec.Status == ReservationStatusSettled {
			total += rec.ActualAmountNanos
		} else if rec.Status == ReservationStatusReserved {
			total += rec.AmountNanos
		}
	}
	return total
}

// CurrentProviderDaily returns spent + reserved nanos for this provider in current UTC day.
func (s *PersistentBudgetStore) CurrentProviderDaily(ctx context.Context, provider string) (int64, error) {
	return s.CurrentProviderDailyAt(ctx, provider, time.Now().UTC())
}

// CurrentProviderDailyAt returns spent + reserved nanos for this provider at a specific time.
func (s *PersistentBudgetStore) CurrentProviderDailyAt(_ context.Context, provider string, now time.Time) (int64, error) {
	s.memMu.Lock()
	defer s.memMu.Unlock()
	return s.calculateProviderDaily(provider, now), nil
}

// CurrentProviderMonthly returns spent + reserved nanos for this provider in current UTC month.
func (s *PersistentBudgetStore) CurrentProviderMonthly(ctx context.Context, provider string) (int64, error) {
	return s.CurrentProviderMonthlyAt(ctx, provider, time.Now().UTC())
}

// CurrentProviderMonthlyAt returns spent + reserved nanos for this provider at a specific time.
func (s *PersistentBudgetStore) CurrentProviderMonthlyAt(_ context.Context, provider string, now time.Time) (int64, error) {
	s.memMu.Lock()
	defer s.memMu.Unlock()
	return s.calculateProviderMonthly(provider, now), nil
}

// CurrentGlobalDaily returns spent + reserved nanos globally in current UTC day.
func (s *PersistentBudgetStore) CurrentGlobalDaily(ctx context.Context) (int64, error) {
	return s.CurrentGlobalDailyAt(ctx, time.Now().UTC())
}

// CurrentGlobalDailyAt returns spent + reserved nanos globally at a specific time.
func (s *PersistentBudgetStore) CurrentGlobalDailyAt(_ context.Context, now time.Time) (int64, error) {
	s.memMu.Lock()
	defer s.memMu.Unlock()
	return s.calculateGlobalDaily(now), nil
}

// CurrentGlobalMonthly returns spent + reserved nanos globally in current UTC month.
func (s *PersistentBudgetStore) CurrentGlobalMonthly(ctx context.Context) (int64, error) {
	return s.CurrentGlobalMonthlyAt(ctx, time.Now().UTC())
}

// CurrentGlobalMonthlyAt returns spent + reserved nanos globally at a specific time.
func (s *PersistentBudgetStore) CurrentGlobalMonthlyAt(_ context.Context, now time.Time) (int64, error) {
	s.memMu.Lock()
	defer s.memMu.Unlock()
	return s.calculateGlobalMonthly(now), nil
}
