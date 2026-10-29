package cost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
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

// PersistentBudgetStore provides persistent, restart-durable, cross-process file-locked budget accounting.
type PersistentBudgetStore struct {
	filePath     string
	lockFilePath string
	memMu        sync.Mutex
	records      map[string]*PersistentReservationRecord
}

// NewPersistentBudgetStore initializes and loads a persistent budget store from disk.
func NewPersistentBudgetStore(dir string) (*PersistentBudgetStore, error) {
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create budget dir: %w", err)
	}

	filePath := filepath.Join(dir, "budget_store.json")
	lockFilePath := filepath.Join(dir, "budget_store.lock")

	store := &PersistentBudgetStore{
		filePath:     filePath,
		lockFilePath: lockFilePath,
		records:      make(map[string]*PersistentReservationRecord),
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

	// Acquire cross-process exclusive advisory lock
	if errFlock := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); errFlock != nil {
		return fmt.Errorf("acquire file lock: %w", errFlock)
	}
	defer func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
	}()

	// Reload latest state from disk in case another process modified it
	_ = s.loadFromDisk()

	if err := fn(); err != nil {
		return err
	}

	// Persist state atomically
	return s.saveToDiskAtomic()
}

func (s *PersistentBudgetStore) loadFromDisk() error {
	data, err := os.ReadFile(s.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(data) == 0 {
		return nil
	}

	var loaded map[string]*PersistentReservationRecord
	if errUnmarshal := json.Unmarshal(data, &loaded); errUnmarshal != nil {
		return errUnmarshal
	}
	s.records = loaded
	return nil
}

func (s *PersistentBudgetStore) saveToDiskAtomic() error {
	data, err := json.MarshalIndent(s.records, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal budget store: %w", err)
	}

	tmpPath := s.filePath + ".tmp"
	if errWrite := os.WriteFile(tmpPath, data, 0o600); errWrite != nil {
		return fmt.Errorf("write tmp budget store: %w", errWrite)
	}

	if errRename := os.Rename(tmpPath, s.filePath); errRename != nil {
		return fmt.Errorf("atomic rename budget store: %w", errRename)
	}

	return nil
}

// ReservationRequest defines input parameters for Reserve.
type ReservationRequest struct {
	ReservationKey    string
	RequestID         string
	Provider          string
	Model             string
	BillingUnit       string
	AmountNanos       int64
	DailyLimitNanos   int64
	MonthlyLimitNanos int64
	TTL               time.Duration
}

// Reserve atomically reserves budget nanos, verifying daily and monthly limits.
func (s *PersistentBudgetStore) Reserve(ctx context.Context, req ReservationRequest) (Reservation, error) {
	if req.AmountNanos < 0 {
		return Reservation{}, errors.New("cannot reserve negative amount")
	}
	if req.AmountNanos == math.MaxInt64 {
		return Reservation{}, errors.New("int64 overflow in reservation amount")
	}

	var reserved Reservation
	err := s.withFileLock(func() error {
		now := time.Now().UTC()

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

		// 3. Create persistent reservation
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

// Settle finalizes a reservation with actual usage. If actual exceeds reservation, limits are re-checked.
func (s *PersistentBudgetStore) Settle(ctx context.Context, reservationKey string, actualAmountNanos int64, dailyLimitNanos, monthlyLimitNanos int64) error {
	if actualAmountNanos < 0 {
		return errors.New("cannot settle negative amount")
	}

	return s.withFileLock(func() error {
		now := time.Now().UTC()
		rec, exists := s.records[reservationKey]
		if !exists || rec == nil {
			return ErrReservationNotFound
		}
		if rec.Status == ReservationStatusSettled {
			return nil // Idempotent
		}

		diff := actualAmountNanos - rec.AmountNanos
		if diff > 0 {
			// Actual cost was greater than reserved: re-check limits
			dailySpent := s.calculateProviderDaily(rec.Provider, now)
			if dailyLimitNanos > 0 && dailySpent+diff > dailyLimitNanos {
				RecordAuditEvent(CostAuditEvent{
					Timestamp:           now,
					Provider:            rec.Provider,
					PublicModel:         rec.Model,
					BillingUnit:         rec.BillingUnit,
					ReservedAmountNanos: rec.AmountNanos,
					ActualAmountNanos:   actualAmountNanos,
					Decision:            DecisionWarn,
					Reason:              "actual_cost_over_reservation_budget_exceeded",
					RequestID:           rec.RequestID,
				})
			}
		}

		rec.ActualAmountNanos = actualAmountNanos
		rec.Status = ReservationStatusSettled
		rec.SettledAtMS = now.UnixMilli()

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
		if rec.Status == ReservationStatusReleased {
			return nil // Idempotent
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
