package kvkk

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
	"uuid"
)

// MemRecords is the in-memory RecordStore of service, wiring and matrix
// tests (05 §13.2). It mirrors the CHECK constraints of 000089–000093 that
// the service relies on and has no transactions (Lock* are plain reads).
// Policies start empty: tests seed them with PutPolicy.
type MemRecords struct {
	mu       sync.Mutex
	now      func() time.Time
	policies map[string]Policy
	logs     []DestructionLog
	breaches map[uuid.UUID]Breach
	subs     map[uuid.UUID]Subprocessor
}

// NewMemRecords returns an empty store; now stamps created_at/updated_at.
func NewMemRecords(now func() time.Time) *MemRecords {
	return &MemRecords{now: now, policies: map[string]Policy{}, breaches: map[uuid.UUID]Breach{}, subs: map[uuid.UUID]Subprocessor{}}
}

var _ RecordStore = (*MemRecords)(nil)

// PutPolicy seeds or replaces a policy as it is (tests; the 000090 seed).
func (m *MemRecords) PutPolicy(p Policy) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = m.now().UTC()
	}
	if p.UpdatedAt.IsZero() {
		p.UpdatedAt = p.CreatedAt
	}
	m.policies[p.Code] = p
}

// Policies implements RecordStore.
func (m *MemRecords) Policies(context.Context) ([]Policy, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := slices.Collect(maps.Values(m.policies))
	slices.SortFunc(out, func(a, b Policy) int { return strings.Compare(a.Code, b.Code) })
	return out, nil
}

// Policy implements RecordStore.
func (m *MemRecords) Policy(_ context.Context, code string) (Policy, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.policies[code]
	if !ok {
		return Policy{}, ErrNotFound
	}
	return p, nil
}

// LockPolicy implements RecordStore.
func (m *MemRecords) LockPolicy(ctx context.Context, code string) (Policy, error) {
	return m.Policy(ctx, code)
}

// UpdatePolicy implements RecordStore (retention_policies_approval_chk).
func (m *MemRecords) UpdatePolicy(_ context.Context, p Policy) error {
	if p.LegalApproved && p.ApprovedRef == "" {
		return ErrInvalidRequest
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.policies[p.Code]
	if !ok {
		return ErrNotFound
	}
	cur.Period, cur.Method, cur.Enabled, cur.LegalApproved, cur.ApprovedRef, cur.UpdatedBy =
		p.Period, p.Method, p.Enabled, p.LegalApproved, p.ApprovedRef, p.UpdatedBy
	cur.UpdatedAt = m.later(cur.UpdatedAt)
	m.policies[p.Code] = cur
	return nil
}

// later returns now, or 1µs after prev when the clock has not moved (the
// ETag must change on every write).
func (m *MemRecords) later(prev time.Time) time.Time {
	now := m.now().UTC().Truncate(time.Microsecond)
	if !now.After(prev) {
		now = prev.Add(time.Microsecond)
	}
	return now
}

// InsertDestructionLog implements RecordStore (destruction_logs FK and
// CHECKs).
func (m *MemRecords) InsertDestructionLog(_ context.Context, l DestructionLog) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.policies[l.PolicyCode]; !ok || l.RecordCount < 0 || !slices.Contains(policyMethods, l.Method) {
		return ErrInvalidRequest
	}
	l.CreatedAt = m.now().UTC().Truncate(time.Microsecond)
	l.Details = maps.Clone(l.Details)
	m.logs = append(m.logs, l)
	return nil
}

// DestructionLogs implements RecordStore.
func (m *MemRecords) DestructionLogs(_ context.Context, f DestructionFilter, after *After, limit int) ([]DestructionLog, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := slices.Clone(m.logs)
	slices.SortFunc(rows, func(a, b DestructionLog) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(b.ID.String(), a.ID.String())
	})
	var out []DestructionLog
	for _, l := range rows {
		switch {
		case f.PolicyCode != "" && l.PolicyCode != f.PolicyCode,
			f.From != nil && l.CreatedAt.Before(*f.From),
			f.To != nil && !l.CreatedAt.Before(*f.To),
			after != nil && (l.CreatedAt.After(after.At) || (l.CreatedAt.Equal(after.At) && l.ID.String() >= after.ID.String())):
			continue
		}
		out = append(out, l)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// DestructionLogsOfRun implements RecordStore.
func (m *MemRecords) DestructionLogsOfRun(_ context.Context, runID uuid.UUID) ([]DestructionLog, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []DestructionLog
	for _, l := range m.logs {
		if l.RunID == runID {
			out = append(out, l)
		}
	}
	return out, nil
}

// checkBreach mirrors the 000092 CHECKs.
func checkBreach(b Breach) error {
	switch {
	case !b.Status.Valid(), (b.Status == BreachClosed) != (b.ClosedAt != nil),
		b.AffectedEstimate != nil && *b.AffectedEstimate < 0:
		return ErrInvalidRequest
	}
	return nil
}

// InsertBreach implements RecordStore.
func (m *MemRecords) InsertBreach(_ context.Context, b Breach) error {
	if err := checkBreach(b); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.breaches[b.ID]; dup {
		return ErrState
	}
	b = b.clone()
	b.CreatedAt = m.now().UTC().Truncate(time.Microsecond)
	b.UpdatedAt = b.CreatedAt
	m.breaches[b.ID] = b
	return nil
}

// Breach implements RecordStore.
func (m *MemRecords) Breach(_ context.Context, id uuid.UUID) (Breach, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.breaches[id]
	if !ok {
		return Breach{}, ErrNotFound
	}
	return b.clone(), nil
}

// LockBreach implements RecordStore.
func (m *MemRecords) LockBreach(ctx context.Context, id uuid.UUID) (Breach, error) {
	return m.Breach(ctx, id)
}

// UpdateBreach implements RecordStore.
func (m *MemRecords) UpdateBreach(_ context.Context, b Breach) error {
	if err := checkBreach(b); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.breaches[b.ID]
	if !ok {
		return ErrNotFound
	}
	b = b.clone()
	b.CreatedAt, b.UpdatedAt = cur.CreatedAt, m.later(cur.UpdatedAt)
	m.breaches[b.ID] = b
	return nil
}

// Breaches implements RecordStore.
func (m *MemRecords) Breaches(_ context.Context, status *BreachStatus, after *After, limit int) ([]Breach, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	newer := func(a, b Breach) int { // detected_at DESC, id DESC
		if c := b.DetectedAt.Compare(a.DetectedAt); c != 0 {
			return c
		}
		return strings.Compare(b.ID.String(), a.ID.String())
	}
	var out []Breach
	for _, b := range m.breaches {
		if (status == nil || b.Status == *status) && (after == nil || newer(Breach{DetectedAt: after.At, ID: after.ID}, b) < 0) {
			out = append(out, b.clone())
		}
	}
	slices.SortFunc(out, newer)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// checkSubprocessor mirrors the 000093 CHECKs and the unique name.
func (m *MemRecords) checkSubprocessor(s Subprocessor) error {
	switch {
	case !slices.Contains(transferMechanisms, s.TransferMechanism), len(s.CountryCode) != 2,
		s.CountryCode != "TR" && s.TransferMechanism == "domestic":
		return ErrInvalidRequest
	}
	for _, o := range m.subs {
		if o.ID != s.ID && strings.EqualFold(o.Name, s.Name) {
			return ErrDuplicateSubprocessor
		}
	}
	return nil
}

// InsertSubprocessor implements RecordStore.
func (m *MemRecords) InsertSubprocessor(_ context.Context, s Subprocessor) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.checkSubprocessor(s); err != nil {
		return err
	}
	s = s.clone()
	s.CreatedAt = m.now().UTC().Truncate(time.Microsecond)
	s.UpdatedAt = s.CreatedAt
	m.subs[s.ID] = s
	return nil
}

// Subprocessor implements RecordStore.
func (m *MemRecords) Subprocessor(_ context.Context, id uuid.UUID) (Subprocessor, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.subs[id]
	if !ok {
		return Subprocessor{}, ErrNotFound
	}
	return s.clone(), nil
}

// LockSubprocessor implements RecordStore.
func (m *MemRecords) LockSubprocessor(ctx context.Context, id uuid.UUID) (Subprocessor, error) {
	return m.Subprocessor(ctx, id)
}

// UpdateSubprocessor implements RecordStore.
func (m *MemRecords) UpdateSubprocessor(_ context.Context, s Subprocessor) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.subs[s.ID]
	if !ok {
		return ErrNotFound
	}
	if err := m.checkSubprocessor(s); err != nil {
		return err
	}
	s = s.clone()
	s.CreatedAt, s.UpdatedAt = cur.CreatedAt, m.later(cur.UpdatedAt)
	m.subs[s.ID] = s
	return nil
}

// Subprocessors implements RecordStore.
func (m *MemRecords) Subprocessors(_ context.Context, active *bool) ([]Subprocessor, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Subprocessor
	for _, s := range m.subs {
		if active == nil || s.Active == *active {
			out = append(out, s.clone())
		}
	}
	slices.SortFunc(out, func(a, b Subprocessor) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// PutBreach seeds a breach as it is, timestamps included (matrix fixtures:
// a fixed updated_at gives a fixed ETag).
func (m *MemRecords) PutBreach(b Breach) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.breaches[b.ID] = b.clone()
}

// PutSubprocessor seeds a sub-processor as it is, timestamps included.
func (m *MemRecords) PutSubprocessor(s Subprocessor) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.subs[s.ID] = s.clone()
}

// PurgeDestructionLogs implements RecordStore with the rules of 000121's
// destruction_logs_purge: the cut-off never passes now − the policy's
// period, and a real purge needs the policy enabled and legally approved.
func (m *MemRecords) PurgeDestructionLogs(_ context.Context, cutoff time.Time, limit int, dryRun bool) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.policies["destruction_log"]
	switch {
	case !ok:
		return 0, errors.New("kvkk: retention purge: no retention policy destruction_log")
	case !dryRun && (!p.Enabled || !p.LegalApproved):
		return 0, errors.New("kvkk: retention purge: policy destruction_log is not enabled and legally approved (dry run only)")
	}
	if c := p.Period.Before(m.now()); c.Before(cutoff) {
		cutoff = c
	}
	var old []DestructionLog
	for _, l := range m.logs {
		if l.CreatedAt.Before(cutoff) {
			old = append(old, l)
		}
	}
	if dryRun {
		return len(old), nil
	}
	slices.SortFunc(old, func(a, b DestructionLog) int { return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), a.ID.Compare(b.ID)) })
	doomed := map[uuid.UUID]bool{}
	for _, l := range old[:min(len(old), max(limit, 0))] {
		doomed[l.ID] = true
	}
	m.logs = slices.DeleteFunc(m.logs, func(l DestructionLog) bool { return doomed[l.ID] })
	return len(doomed), nil
}

// PurgeBreaches implements RecordStore.
func (m *MemRecords) PurgeBreaches(_ context.Context, cutoff time.Time, limit int, dryRun bool) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var old []Breach
	for _, b := range m.breaches {
		if b.Status == BreachClosed && b.ClosedAt != nil && b.ClosedAt.Before(cutoff) {
			old = append(old, b)
		}
	}
	if dryRun {
		return len(old), nil
	}
	slices.SortFunc(old, func(a, b Breach) int { return cmp.Or(a.ClosedAt.Compare(*b.ClosedAt), a.ID.Compare(b.ID)) })
	old = old[:min(len(old), max(limit, 0))]
	for _, b := range old {
		delete(m.breaches, b.ID)
	}
	return len(old), nil
}
