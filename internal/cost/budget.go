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
	Reserve(ctx context.Context, provider string, amountNanos int64, dailyLimitNanos, monthlyLimitNanos int64) (Reservation, error)
	Release(ctx context.Context, res Reservation) error
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
func (s *MemoryBudgetStore) Reserve(_ context.Context, provider string, amountNanos int64, dailyLimitNanos, monthlyLimitNanos int64) (Reservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if amountNanos < 0 {
		return Reservation{}, errors.New("cannot reserve negative amount")
	}

	currentDaily := s.providerDaily[provider]
	if dailyLimitNanos > 0 && currentDaily+amountNanos > dailyLimitNanos {
		return Reservation{}, fmt.Errorf("%w: provider daily limit exceeded (current=%d, req=%d, limit=%d)", ErrBudgetExceeded, currentDaily, amountNanos, dailyLimitNanos)
	}

	currentMonth := s.providerMonth[provider]
	if monthlyLimitNanos > 0 && currentMonth+amountNanos > monthlyLimitNanos {
		return Reservation{}, fmt.Errorf("%w: provider monthly limit exceeded (current=%d, req=%d, limit=%d)", ErrBudgetExceeded, currentMonth, amountNanos, monthlyLimitNanos)
	}

	s.providerDaily[provider] += amountNanos
	s.providerMonth[provider] += amountNanos
	s.globalDaily += amountNanos
	s.globalMonth += amountNanos

	s.reservationSeq++
	resID := fmt.Sprintf("res-%d", s.reservationSeq)
	res := Reservation{
		ID:          resID,
		Provider:    provider,
		AmountNanos: amountNanos,
		ReservedAt:  time.Now(),
	}
	s.reservations[resID] = res
	return res, nil
}

// Release releases a previously reserved amount.
func (s *MemoryBudgetStore) Release(_ context.Context, res Reservation) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.reservations[res.ID]; !exists {
		return ErrReservationNotFound
	}
	delete(s.reservations, res.ID)

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
