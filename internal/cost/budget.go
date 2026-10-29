package cost

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	// ErrBudgetExceeded indicates the request would exceed the reserved budget.
	ErrBudgetExceeded = errors.New("budget exceeded")
	// ErrReservationNotFound indicates the reservation ID was not found.
	ErrReservationNotFound = errors.New("reservation not found")
)

// Reservation represents a reserved amount of nanodollars against a budget.
type Reservation struct {
	ID          string    `json:"id"`
	Provider    string    `json:"provider"`
	AmountNanos int64     `json:"amount_nanos"`
	ReservedAt  time.Time `json:"reserved_at"`
}

// BudgetStore defines the contract for budget tracking and atomic reservation.
// NOTE: Single-process in-memory implementation is provided for test/local use.
// Distributed persistent storage (e.g. Redis / CPA Manager Plus sync) is designated
// for subsequent tasks (Task 7).
type BudgetStore interface {
	CurrentProviderDaily(ctx context.Context, provider string) (int64, error)
	CurrentProviderMonthly(ctx context.Context, provider string) (int64, error)
	CurrentGlobalDaily(ctx context.Context) (int64, error)
	CurrentGlobalMonthly(ctx context.Context) (int64, error)
	Reserve(ctx context.Context, req ReservationRequest) (Reservation, error)
	Release(ctx context.Context, reservationKey, reason string) error
	Settle(ctx context.Context, reservationKey string, actualAmountNanos int64, dailyLimitNanos, monthlyLimitNanos int64) error
}

// MemoryBudgetStore provides a thread-safe, in-memory implementation of BudgetStore.
type MemoryBudgetStore struct {
	mu             sync.Mutex
	providerDaily  map[string]int64
	providerMonth  map[string]int64
	globalDaily    int64
	globalMonth    int64
	reservations   map[string]Reservation
	reservationSeq int64
}

// NewMemoryBudgetStore creates a new in-memory budget store.
func NewMemoryBudgetStore() *MemoryBudgetStore {
	return &MemoryBudgetStore{
		providerDaily: make(map[string]int64),
		providerMonth: make(map[string]int64),
		reservations:  make(map[string]Reservation),
	}
}

// CurrentProviderDaily returns today's spent/reserved nanodollars for a provider.
func (s *MemoryBudgetStore) CurrentProviderDaily(_ context.Context, provider string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.providerDaily[provider], nil
}

// CurrentProviderMonthly returns this month's spent/reserved nanodollars for a provider.
func (s *MemoryBudgetStore) CurrentProviderMonthly(_ context.Context, provider string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.providerMonth[provider], nil
}

// CurrentGlobalDaily returns today's global spent/reserved nanodollars.
func (s *MemoryBudgetStore) CurrentGlobalDaily(_ context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.globalDaily, nil
}

// CurrentGlobalMonthly returns this month's global spent/reserved nanodollars.
func (s *MemoryBudgetStore) CurrentGlobalMonthly(_ context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.globalMonth, nil
}

// Reserve atomically checks budget limits and holds the requested amount to prevent over-allocation.
func (s *MemoryBudgetStore) Reserve(_ context.Context, req ReservationRequest) (Reservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if req.AmountNanos < 0 {
		return Reservation{}, errors.New("cannot reserve negative amount")
	}

	if existing, exists := s.reservations[req.ReservationKey]; exists {
		return existing, nil
	}

	currentDaily := s.providerDaily[req.Provider]
	if req.DailyLimitNanos > 0 && currentDaily+req.AmountNanos > req.DailyLimitNanos {
		return Reservation{}, fmt.Errorf("%w: provider daily limit exceeded (current=%d, req=%d, limit=%d)", ErrBudgetExceeded, currentDaily, req.AmountNanos, req.DailyLimitNanos)
	}

	currentMonth := s.providerMonth[req.Provider]
	if req.MonthlyLimitNanos > 0 && currentMonth+req.AmountNanos > req.MonthlyLimitNanos {
		return Reservation{}, fmt.Errorf("%w: provider monthly limit exceeded (current=%d, req=%d, limit=%d)", ErrBudgetExceeded, currentMonth, req.AmountNanos, req.MonthlyLimitNanos)
	}

	s.providerDaily[req.Provider] += req.AmountNanos
	s.providerMonth[req.Provider] += req.AmountNanos
	s.globalDaily += req.AmountNanos
	s.globalMonth += req.AmountNanos

	s.reservationSeq++
	resID := req.ReservationKey
	if resID == "" {
		resID = fmt.Sprintf("res-%d", s.reservationSeq)
	}
	res := Reservation{
		ID:          resID,
		Provider:    req.Provider,
		AmountNanos: req.AmountNanos,
		ReservedAt:  time.Now(),
	}
	s.reservations[resID] = res
	return res, nil
}

// Release releases a previously reserved amount.
func (s *MemoryBudgetStore) Release(_ context.Context, reservationKey, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, exists := s.reservations[reservationKey]
	if !exists {
		return nil
	}
	delete(s.reservations, reservationKey)

	s.providerDaily[res.Provider] -= res.AmountNanos
	if s.providerDaily[res.Provider] < 0 {
		s.providerDaily[res.Provider] = 0
	}

	s.providerMonth[res.Provider] -= res.AmountNanos
	if s.providerMonth[res.Provider] < 0 {
		s.providerMonth[res.Provider] = 0
	}

	s.globalDaily -= res.AmountNanos
	if s.globalDaily < 0 {
		s.globalDaily = 0
	}

	s.globalMonth -= res.AmountNanos
	if s.globalMonth < 0 {
		s.globalMonth = 0
	}

	return nil
}

// Settle finalizes a reservation in memory.
func (s *MemoryBudgetStore) Settle(_ context.Context, reservationKey string, actualAmountNanos int64, dailyLimitNanos, monthlyLimitNanos int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, exists := s.reservations[reservationKey]
	if !exists {
		return nil
	}
	diff := actualAmountNanos - res.AmountNanos
	s.providerDaily[res.Provider] += diff
	s.providerMonth[res.Provider] += diff
	s.globalDaily += diff
	s.globalMonth += diff
	return nil
}
