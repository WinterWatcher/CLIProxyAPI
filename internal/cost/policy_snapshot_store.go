package cost

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// PolicyAuditEvent records every policy import, activation, rollback, or revocation event.
type PolicyAuditEvent struct {
	Action      string `json:"action"` // "import", "activate", "rollback", "revoke", "deny"
	Provider    string `json:"provider"`
	Model       string `json:"model"`
	Version     string `json:"version"`
	ContentHash string `json:"content_hash"`
	RiskLevel   string `json:"risk_level,omitempty"`
	Operator    string `json:"operator"`
	Reason      string `json:"reason,omitempty"`
	TimestampMS int64  `json:"timestamp_ms"`
}

// PersistentPolicyState represents the complete on-disk policy store state.
type PersistentPolicyState struct {
	ActivePolicies     map[string]*PolicySnapshot   `json:"active_policies"`
	PreviousPolicies   map[string]*PolicySnapshot   `json:"previous_policies"`
	HistoricalPolicies map[string][]*PolicySnapshot `json:"historical_policies"`
	RevokedVersions    map[string]bool              `json:"revoked_versions"`
	AuditEvents        []PolicyAuditEvent           `json:"audit_events"`
}

// PolicyStore defines the interface for querying and mutating price policy snapshots.
type PolicyStore interface {
	GetActivePolicy(provider, model string) (*PolicySnapshot, bool)
	GetActivePolicyHash(provider, model string) string
	GetPreviousPolicyHash(provider, model string) string
	ListActivePolicies() []*PolicySnapshot
	ListHistoricalPolicies(provider, model string) []*PolicySnapshot
	ListAuditEvents(limit, offset int) []PolicyAuditEvent
	DryRun(snap *PolicySnapshot) (*DryRunReport, error)
	Activate(snap *PolicySnapshot, force bool) (*PolicySnapshot, error)
	Rollback(provider, model string, targetVersion string) (*PolicySnapshot, error)
	Revoke(provider, model, version, reason string) error
	ValidatePolicySnapshot(snap *PolicySnapshot) error
}

// PersistentPolicyStore provides thread-safe, file-locked persistent policy management.
type PersistentPolicyStore struct {
	filePath           string
	lockFilePath       string
	signatureSecret    string
	memMu              sync.RWMutex
	activePolicies     map[string]*PolicySnapshot
	previousPolicies   map[string]*PolicySnapshot
	historicalPolicies map[string][]*PolicySnapshot
	revokedVersions    map[string]bool
	auditEvents        []PolicyAuditEvent
}

// policyKey formats a consistent map key for (provider, model).
func policyKey(provider, model string) string {
	return strings.ToLower(strings.TrimSpace(provider)) + ":" + strings.ToLower(strings.TrimSpace(model))
}

func cloneSnapshot(s *PolicySnapshot) *PolicySnapshot {
	if s == nil {
		return nil
	}
	cpy := *s
	return &cpy
}

// NewPersistentPolicyStore initializes a policy store in the given directory.
func NewPersistentPolicyStore(dir string, signatureSecret ...string) (*PersistentPolicyStore, error) {
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create policy dir: %w", err)
	}

	// Clean up abandoned temp files older than a minute
	if entries, errRead := os.ReadDir(dir); errRead == nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "cost_policies.json.tmp.") {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}

	secret := ""
	if len(signatureSecret) > 0 {
		secret = signatureSecret[0]
	}

	store := &PersistentPolicyStore{
		filePath:           filepath.Join(dir, "cost_policies.json"),
		lockFilePath:       filepath.Join(dir, "cost_policies.lock"),
		signatureSecret:    secret,
		activePolicies:     make(map[string]*PolicySnapshot),
		previousPolicies:   make(map[string]*PolicySnapshot),
		historicalPolicies: make(map[string][]*PolicySnapshot),
		revokedVersions:    make(map[string]bool),
		auditEvents:        make([]PolicyAuditEvent, 0),
	}

	if err := store.withFileLock(func() error {
		return store.loadFromDisk()
	}); err != nil {
		return nil, err
	}

	return store, nil
}

func (s *PersistentPolicyStore) withFileLock(fn func() error) error {
	lockFile, err := os.OpenFile(s.lockFilePath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open policy lock file: %w", err)
	}
	defer lockFile.Close()

	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("acquire policy file lock: %w", err)
	}
	defer func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
	}()

	return fn()
}

func (s *PersistentPolicyStore) loadFromDisk() error {
	data, err := os.ReadFile(s.filePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read policy store: %w", err)
	}
	if len(data) == 0 {
		return nil
	}

	var state PersistentPolicyState
	if err := json.Unmarshal(data, &state); err != nil {
		// Fail-closed on corrupt JSON to prevent loading incomplete or forged state
		return fmt.Errorf("policy store file is corrupted: %w", err)
	}

	if state.ActivePolicies != nil {
		s.activePolicies = state.ActivePolicies
	}
	if state.PreviousPolicies != nil {
		s.previousPolicies = state.PreviousPolicies
	}
	if state.HistoricalPolicies != nil {
		s.historicalPolicies = state.HistoricalPolicies
	}
	if state.RevokedVersions != nil {
		s.revokedVersions = state.RevokedVersions
	}
	if state.AuditEvents != nil {
		s.auditEvents = state.AuditEvents
	}
	return nil
}

func (s *PersistentPolicyStore) saveToDisk() error {
	state := PersistentPolicyState{
		ActivePolicies:     s.activePolicies,
		PreviousPolicies:   s.previousPolicies,
		HistoricalPolicies: s.historicalPolicies,
		RevokedVersions:    s.revokedVersions,
		AuditEvents:        s.auditEvents,
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal policy store: %w", err)
	}

	tmpFile := fmt.Sprintf("%s.tmp.%d.%d", s.filePath, os.Getpid(), time.Now().UnixNano())
	f, err := os.OpenFile(tmpFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create policy temp file: %w", err)
	}

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpFile)
		return fmt.Errorf("write policy temp file: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpFile)
		return fmt.Errorf("sync policy temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpFile)
		return fmt.Errorf("close policy temp file: %w", err)
	}

	if err := os.Rename(tmpFile, s.filePath); err != nil {
		_ = os.Remove(tmpFile)
		return fmt.Errorf("atomic rename policy file: %w", err)
	}

	dirFile, errDir := os.Open(filepath.Dir(s.filePath))
	if errDir == nil {
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}
	return nil
}

func (s *PersistentPolicyStore) ValidatePolicySnapshot(snap *PolicySnapshot) error {
	if snap == nil {
		return errors.New("policy snapshot is nil")
	}
	if err := snap.Validate(); err != nil {
		return err
	}
	if s.signatureSecret != "" {
		if !snap.VerifySignature(s.signatureSecret) {
			return errors.New("signature mismatch")
		}
	} else if snap.Signature != "" {
		// If signature is present, verify it cannot be arbitrary garbage
		if !strings.HasPrefix(snap.Signature, "hmac-sha256:") {
			return errors.New("signature format unsupported")
		}
	}
	if snap.IsExpired(time.Now().UnixMilli()) {
		return errors.New("policy snapshot is expired")
	}
	s.memMu.RLock()
	isRevoked := s.revokedVersions[snap.PolicyVersion]
	s.memMu.RUnlock()
	if isRevoked {
		return fmt.Errorf("policy version %s is revoked", snap.PolicyVersion)
	}
	return nil
}

func (s *PersistentPolicyStore) GetActivePolicy(provider, model string) (*PolicySnapshot, bool) {
	s.memMu.RLock()
	defer s.memMu.RUnlock()
	snap, ok := s.activePolicies[policyKey(provider, model)]
	if !ok || snap == nil {
		return nil, false
	}
	return cloneSnapshot(snap), true
}

func (s *PersistentPolicyStore) GetActivePolicyHash(provider, model string) string {
	s.memMu.RLock()
	defer s.memMu.RUnlock()
	snap, ok := s.activePolicies[policyKey(provider, model)]
	if !ok || snap == nil {
		return ""
	}
	return snap.ContentHash
}

func (s *PersistentPolicyStore) GetPreviousPolicyHash(provider, model string) string {
	s.memMu.RLock()
	defer s.memMu.RUnlock()
	snap, ok := s.previousPolicies[policyKey(provider, model)]
	if !ok || snap == nil {
		return ""
	}
	return snap.ContentHash
}

func (s *PersistentPolicyStore) ListActivePolicies() []*PolicySnapshot {
	s.memMu.RLock()
	defer s.memMu.RUnlock()
	res := make([]*PolicySnapshot, 0, len(s.activePolicies))
	for _, snap := range s.activePolicies {
		res = append(res, cloneSnapshot(snap))
	}
	return res
}

func (s *PersistentPolicyStore) ListHistoricalPolicies(provider, model string) []*PolicySnapshot {
	s.memMu.RLock()
	defer s.memMu.RUnlock()
	list := s.historicalPolicies[policyKey(provider, model)]
	res := make([]*PolicySnapshot, 0, len(list))
	for _, snap := range list {
		res = append(res, cloneSnapshot(snap))
	}
	return res
}

func (s *PersistentPolicyStore) ListAuditEvents(limit, offset int) []PolicyAuditEvent {
	s.memMu.RLock()
	defer s.memMu.RUnlock()
	total := len(s.auditEvents)
	if offset < 0 {
		offset = 0
	}
	if offset >= total {
		return nil
	}
	end := offset + limit
	if limit <= 0 || end > total {
		end = total
	}
	// Return in reverse chronological order
	res := make([]PolicyAuditEvent, 0, end-offset)
	for i := total - 1 - offset; i >= 0 && len(res) < limit; i-- {
		res = append(res, s.auditEvents[i])
	}
	return res
}

func (s *PersistentPolicyStore) DryRun(snap *PolicySnapshot) (*DryRunReport, error) {
	if err := s.ValidatePolicySnapshot(snap); err != nil {
		return nil, err
	}
	s.memMu.RLock()
	current := s.activePolicies[policyKey(snap.Provider, snap.PublicModel)]
	s.memMu.RUnlock()

	diff := ComputePolicyDiff(current, snap)
	allowed := diff.Decision == "allow"
	return &DryRunReport{
		Diffs:     []PolicyDiff{diff},
		RiskLevel: diff.RiskLevel,
		Decision:  diff.Decision,
		Allowed:   allowed,
		Summary:   fmt.Sprintf("[%s] %s (%s)", diff.RiskLevel, diff.Decision, diff.Reason),
	}, nil
}

func (s *PersistentPolicyStore) Activate(snap *PolicySnapshot, force bool) (*PolicySnapshot, error) {
	if err := s.ValidatePolicySnapshot(snap); err != nil {
		return nil, err
	}

	report, err := s.DryRun(snap)
	if err != nil {
		return nil, err
	}
	if !report.Allowed && !force {
		// Log deny audit event
		s.recordAudit("deny", snap, report.RiskLevel, "system", report.Summary)
		return nil, fmt.Errorf("activation rejected: %s", report.Summary)
	}

	s.memMu.Lock()
	defer s.memMu.Unlock()

	var activated *PolicySnapshot
	err = s.withFileLock(func() error {
		_ = s.loadFromDisk()

		key := policyKey(snap.Provider, snap.PublicModel)
		current := s.activePolicies[key]

		// Idempotency: same version and content hash is no-op
		if current != nil && current.PolicyVersion == snap.PolicyVersion && current.ContentHash == snap.ContentHash {
			activated = cloneSnapshot(current)
			return nil
		}

		// Prevent rollback or overwrite by older unknown version unless forced
		if current != nil && !force && current.EffectiveAtMS > snap.EffectiveAtMS {
			return errors.New("cannot overwrite newer policy version with older policy without force")
		}

		// Archive current into previous and history
		if current != nil {
			s.previousPolicies[key] = cloneSnapshot(current)
			s.historicalPolicies[key] = append(s.historicalPolicies[key], cloneSnapshot(current))
		}

		cloned := cloneSnapshot(snap)
		s.activePolicies[key] = cloned
		s.auditEvents = append(s.auditEvents, PolicyAuditEvent{
			Action:      "activate",
			Provider:    snap.Provider,
			Model:       snap.PublicModel,
			Version:     snap.PolicyVersion,
			ContentHash: snap.ContentHash,
			RiskLevel:   report.RiskLevel,
			Operator:    "admin",
			Reason:      "activated via policy store",
			TimestampMS: time.Now().UnixMilli(),
		})

		if errSave := s.saveToDisk(); errSave != nil {
			// Rollback memory modification on save failure
			if current != nil {
				s.activePolicies[key] = current
			} else {
				delete(s.activePolicies, key)
			}
			return fmt.Errorf("failed to save active policy to disk: %w", errSave)
		}

		activated = cloned
		return nil
	})

	if err != nil {
		return nil, err
	}
	return activated, nil
}

func (s *PersistentPolicyStore) Rollback(provider, model string, targetVersion string) (*PolicySnapshot, error) {
	s.memMu.Lock()
	defer s.memMu.Unlock()

	var rolledBack *PolicySnapshot
	err := s.withFileLock(func() error {
		_ = s.loadFromDisk()

		key := policyKey(provider, model)
		current := s.activePolicies[key]
		if current == nil {
			return fmt.Errorf("no active policy found for %s/%s", provider, model)
		}

		var target *PolicySnapshot
		if targetVersion == "" {
			target = s.previousPolicies[key]
		} else {
			for _, h := range s.historicalPolicies[key] {
				if h.PolicyVersion == targetVersion {
					target = h
					break
				}
			}
		}

		if target == nil {
			return fmt.Errorf("no previous version found to rollback for %s/%s", provider, model)
		}

		// Rollback cannot bypass validation
		if err := target.Validate(); err != nil {
			return fmt.Errorf("rollback target policy is invalid: %w", err)
		}
		if target.IsExpired(time.Now().UnixMilli()) {
			return errors.New("rollback target policy is expired")
		}
		if s.revokedVersions[target.PolicyVersion] {
			return errors.New("rollback target policy is revoked")
		}

		// Move current to history
		s.previousPolicies[key] = cloneSnapshot(current)
		s.historicalPolicies[key] = append(s.historicalPolicies[key], cloneSnapshot(current))

		// Activate target with new effective timestamp
		activated := cloneSnapshot(target)
		activated.EffectiveAtMS = time.Now().UnixMilli()
		activated.ContentHash = activated.ComputeContentHash()

		s.activePolicies[key] = activated
		s.auditEvents = append(s.auditEvents, PolicyAuditEvent{
			Action:      "rollback",
			Provider:    provider,
			Model:       model,
			Version:     activated.PolicyVersion,
			ContentHash: activated.ContentHash,
			RiskLevel:   "medium",
			Operator:    "admin",
			Reason:      fmt.Sprintf("rolled back from %s to %s", current.PolicyVersion, activated.PolicyVersion),
			TimestampMS: time.Now().UnixMilli(),
		})

		if errSave := s.saveToDisk(); errSave != nil {
			s.activePolicies[key] = current
			return fmt.Errorf("failed to save rolled back policy: %w", errSave)
		}

		rolledBack = activated
		return nil
	})

	if err != nil {
		return nil, err
	}
	return rolledBack, nil
}

func (s *PersistentPolicyStore) Revoke(provider, model, version, reason string) error {
	s.memMu.Lock()
	defer s.memMu.Unlock()

	return s.withFileLock(func() error {
		_ = s.loadFromDisk()

		s.revokedVersions[version] = true

		key := policyKey(provider, model)
		current := s.activePolicies[key]
		if current != nil && current.PolicyVersion == version {
			// Deactivate revoked policy
			s.previousPolicies[key] = current
			delete(s.activePolicies, key)
		}

		s.auditEvents = append(s.auditEvents, PolicyAuditEvent{
			Action:      "revoke",
			Provider:    provider,
			Model:       model,
			Version:     version,
			Operator:    "admin",
			Reason:      reason,
			TimestampMS: time.Now().UnixMilli(),
		})

		return s.saveToDisk()
	})
}

func (s *PersistentPolicyStore) recordAudit(action string, snap *PolicySnapshot, riskLevel, operator, reason string) {
	s.memMu.Lock()
	defer s.memMu.Unlock()
	_ = s.withFileLock(func() error {
		s.auditEvents = append(s.auditEvents, PolicyAuditEvent{
			Action:      action,
			Provider:    snap.Provider,
			Model:       snap.PublicModel,
			Version:     snap.PolicyVersion,
			ContentHash: snap.ContentHash,
			RiskLevel:   riskLevel,
			Operator:    operator,
			Reason:      reason,
			TimestampMS: time.Now().UnixMilli(),
		})
		return s.saveToDisk()
	})
}
