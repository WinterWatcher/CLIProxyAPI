package cost

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"
)

// Audit event type constants.
const (
	AuditEventAllow  = "cost_policy_allow"
	AuditEventDeny   = "cost_policy_deny"
	AuditEventWarn   = "cost_policy_warn"
	AuditEventFreeze = "cost_policy_freeze"
)

// CostAuditEvent records a pricing safety evaluation for auditing.
type CostAuditEvent struct {
	Timestamp            time.Time `json:"timestamp"`
	Provider             string    `json:"provider"`
	PublicModel          string    `json:"public_model"`
	UpstreamModelHash    string    `json:"upstream_model_hash_or_redacted"`
	BillingUnit          string    `json:"billing_unit"`
	PriceStatus          string    `json:"price_status"`
	AmountKnown          bool      `json:"amount_known"`
	QuantityKnown        bool      `json:"quantity_known"`
	EstimatedAmountNanos int64     `json:"estimated_amount_nanos"`
	ReservedAmountNanos  int64     `json:"reserved_amount_nanos,omitempty"`
	ActualAmountNanos    int64     `json:"actual_amount_nanos,omitempty"`
	MaxRequestCostNanos  int64     `json:"max_request_cost_nanos"`
	Decision             string    `json:"decision"`
	Reason               string    `json:"reason"`
	RequestID            string    `json:"request_id,omitempty"`
	Attempt              int       `json:"attempt,omitempty"`
}

// CostAuditSink receives cost policy audit events.
type CostAuditSink interface {
	RecordEvent(event CostAuditEvent)
}

// MemoryCostAuditSink is a thread-safe, in-memory buffer of audit events.
type MemoryCostAuditSink struct {
	mu     sync.RWMutex
	events []CostAuditEvent
	limit  int
}

// NewMemoryCostAuditSink creates an in-memory audit event buffer with the given capacity.
func NewMemoryCostAuditSink(limit int) *MemoryCostAuditSink {
	if limit <= 0 {
		limit = 1000
	}
	return &MemoryCostAuditSink{
		events: make([]CostAuditEvent, 0, limit),
		limit:  limit,
	}
}

// RecordEvent appends an audit event in a thread-safe manner, sanitizing secrets.
func (s *MemoryCostAuditSink) RecordEvent(event CostAuditEvent) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// Ensure sensitive contents are never stored in audit records
	event.Provider = sanitizeAuditField(event.Provider)
	event.PublicModel = sanitizeAuditField(event.PublicModel)
	event.Reason = sanitizeAuditField(event.Reason)

	if len(s.events) >= s.limit {
		s.events = s.events[1:]
	}
	s.events = append(s.events, event)
}

// Events returns a copy of all captured events.
func (s *MemoryCostAuditSink) Events() []CostAuditEvent {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	copied := make([]CostAuditEvent, len(s.events))
	copy(copied, s.events)
	return copied
}

// LastEvent returns the most recently recorded event, or nil if empty.
func (s *MemoryCostAuditSink) LastEvent() *CostAuditEvent {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.events) == 0 {
		return nil
	}
	ev := s.events[len(s.events)-1]
	return &ev
}

// Clear flushes all recorded events.
func (s *MemoryCostAuditSink) Clear() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = s.events[:0]
}

var (
	globalAuditSinkMu sync.RWMutex
	globalAuditSink   CostAuditSink = NewMemoryCostAuditSink(1000)
)

// SetGlobalAuditSink configures the system-wide audit sink.
func SetGlobalAuditSink(sink CostAuditSink) {
	globalAuditSinkMu.Lock()
	defer globalAuditSinkMu.Unlock()
	globalAuditSink = sink
}

// GetGlobalAuditSink returns the current global audit sink.
func GetGlobalAuditSink() CostAuditSink {
	globalAuditSinkMu.RLock()
	defer globalAuditSinkMu.RUnlock()
	return globalAuditSink
}

// RecordAuditEvent logs an event to the global audit sink.
func RecordAuditEvent(event CostAuditEvent) {
	sink := GetGlobalAuditSink()
	if sink != nil {
		sink.RecordEvent(event)
	}
}

// HashUpstreamModel returns a privacy-preserving SHA-256 hash representation of an upstream model identifier.
func HashUpstreamModel(model string) string {
	trimmed := strings.TrimSpace(model)
	if trimmed == "" {
		return ""
	}
	h := sha256.Sum256([]byte(trimmed))
	return hex.EncodeToString(h[:8])
}

func sanitizeAuditField(s string) string {
	// Strip out potential credential markers
	lowered := strings.ToLower(s)
	if strings.Contains(lowered, "bearer ") ||
		strings.Contains(lowered, "sk-") ||
		strings.Contains(lowered, "key=") ||
		strings.Contains(lowered, "password") {
		return "[REDACTED]"
	}
	return s
}
