package kvkk

import (
	"cmp"
	"context"
	"slices"
	"sync"
	"time"
	"uuid"
)

// MemStore is the in-memory Store of service, wiring and matrix tests
// (05 §13.2). It follows the GORM store's contract, CHECK constraints of
// 000088 included, and has no transactions (db.NopTx); Lock is a plain
// read.
type MemStore struct {
	mu   sync.Mutex
	now  func() time.Time
	rows map[uuid.UUID]DSR
}

// NewMemStore returns an empty store; now stamps updated_at.
func NewMemStore(now func() time.Time) *MemStore {
	return &MemStore{now: now, rows: map[uuid.UUID]DSR{}}
}

var _ Store = (*MemStore)(nil)

// checkRow mirrors the 000088 CHECK constraints.
func checkRow(d DSR) error {
	switch {
	case !d.Type.Valid() || !d.Channel.Valid() || !d.Status.Valid():
		return ErrInvalidRequest
	case d.UserID == nil && d.RequesterEmail == "":
		return ErrInvalidRequest
	case d.SecondApprover != nil && (d.FirstApprover == nil || *d.SecondApprover == *d.FirstApprover):
		return ErrInvalidRequest
	case d.Type == TypeProfilingAccess && d.Status == StatusCompleted && d.SecondApprover == nil:
		return ErrInvalidRequest
	case (d.Status == StatusCompleted || d.Status == StatusRejected) != (d.CompletedAt != nil):
		return ErrInvalidRequest
	case d.Status == StatusRejected && d.RejectionReason == "":
		return ErrInvalidRequest
	}
	return nil
}

// Insert implements Store.
func (m *MemStore) Insert(_ context.Context, d DSR) error {
	if err := checkRow(d); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.rows[d.ID]; dup {
		return ErrState
	}
	d = d.clone()
	d.CreatedAt, d.UpdatedAt = d.ReceivedAt, d.ReceivedAt
	m.rows[d.ID] = d
	return nil
}

// Get implements Store.
func (m *MemStore) Get(_ context.Context, id uuid.UUID) (DSR, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.rows[id]
	if !ok {
		return DSR{}, ErrNotFound
	}
	return d.clone(), nil
}

// GetOwn implements Store.
func (m *MemStore) GetOwn(ctx context.Context, id, userID uuid.UUID) (DSR, error) {
	d, err := m.Get(ctx, id)
	if err != nil || d.UserID == nil || *d.UserID != userID {
		return DSR{}, ErrNotFound
	}
	return d, nil
}

// Lock implements Store.
func (m *MemStore) Lock(ctx context.Context, id uuid.UUID) (DSR, error) { return m.Get(ctx, id) }

// Update implements Store.
func (m *MemStore) Update(_ context.Context, d DSR) error {
	if err := checkRow(d); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.rows[d.ID]
	if !ok {
		return ErrNotFound
	}
	next := d.clone()
	// Immutable columns keep their stored value (the GORM store never
	// writes them).
	next.UserID, next.RequesterName, next.RequesterEmail = cur.UserID, cur.RequesterName, cur.RequesterEmail
	next.Type, next.Channel, next.Details = cur.Type, cur.Channel, cur.Details
	next.ReceivedAt, next.DueAt, next.CreatedAt = cur.ReceivedAt, cur.DueAt, cur.CreatedAt
	// updated_at strictly increases, as the trigger's transaction time
	// does across transactions (ETag).
	at := m.now().UTC().Truncate(time.Microsecond)
	if !at.After(cur.UpdatedAt) {
		at = cur.UpdatedAt.Add(time.Microsecond)
	}
	next.UpdatedAt = at
	m.rows[d.ID] = next
	return nil
}

func (m *MemStore) all(keep func(DSR) bool) []DSR {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []DSR
	for _, d := range m.rows {
		if keep(d) {
			out = append(out, d.clone())
		}
	}
	return out
}

func byReceivedDesc(a, b DSR) int {
	if c := b.ReceivedAt.Compare(a.ReceivedAt); c != 0 {
		return c
	}
	return b.ID.Compare(a.ID)
}

func byDueAsc(a, b DSR) int {
	if c := a.DueAt.Compare(b.DueAt); c != 0 {
		return c
	}
	return a.ID.Compare(b.ID)
}

func limitTo(rows []DSR, limit int) []DSR {
	if limit > 0 && len(rows) > limit {
		return rows[:limit]
	}
	return rows
}

// ListOwn implements Store.
func (m *MemStore) ListOwn(_ context.Context, userID uuid.UUID, after *After, limit int) ([]DSR, error) {
	rows := m.all(func(d DSR) bool {
		if d.UserID == nil || *d.UserID != userID {
			return false
		}
		return after == nil || byReceivedDesc(d, DSR{ReceivedAt: after.At, ID: after.ID}) > 0
	})
	slices.SortFunc(rows, byReceivedDesc)
	return limitTo(rows, limit), nil
}

// List implements Store.
func (m *MemStore) List(_ context.Context, f AdminFilter, after *After, limit int) ([]DSR, error) {
	rows := m.all(func(d DSR) bool {
		switch {
		case len(f.Statuses) > 0 && !slices.Contains(f.Statuses, d.Status):
			return false
		case f.Type != nil && d.Type != *f.Type:
			return false
		case f.DueBefore != nil && !d.DueAt.Before(*f.DueBefore):
			return false
		}
		return after == nil || byDueAsc(d, DSR{DueAt: after.At, ID: after.ID}) > 0
	})
	slices.SortFunc(rows, byDueAsc)
	return limitTo(rows, limit), nil
}

// HasOpen implements Store.
func (m *MemStore) HasOpen(_ context.Context, userID uuid.UUID, t Type) (bool, error) {
	return len(m.all(func(d DSR) bool {
		return d.UserID != nil && *d.UserID == userID && d.Type == t && d.Status.Open()
	})) > 0, nil
}

// AllOf implements Store.
func (m *MemStore) AllOf(_ context.Context, userID uuid.UUID) ([]DSR, error) {
	rows := m.all(func(d DSR) bool { return d.UserID != nil && *d.UserID == userID })
	slices.SortFunc(rows, func(a, b DSR) int { return -byReceivedDesc(a, b) })
	return rows, nil
}

// DueBy implements Store.
func (m *MemStore) DueBy(_ context.Context, cutoff time.Time, after *After, limit int) ([]DSR, error) {
	rows := m.all(func(d DSR) bool {
		return d.Status.Open() && !d.DueAt.After(cutoff) &&
			(after == nil || byDueAsc(d, DSR{DueAt: after.At, ID: after.ID}) > 0)
	})
	slices.SortFunc(rows, byDueAsc)
	return limitTo(rows, limit), nil
}

// ExportsCompletedBefore implements Store.
func (m *MemStore) ExportsCompletedBefore(_ context.Context, cutoff time.Time, afterID uuid.UUID, limit int) ([]DSR, error) {
	rows := m.all(func(d DSR) bool {
		return d.ExportFileID != nil && d.CompletedAt != nil && !d.CompletedAt.After(cutoff) && d.ID.Compare(afterID) > 0
	})
	slices.SortFunc(rows, func(a, b DSR) int { return a.ID.Compare(b.ID) })
	return limitTo(rows, limit), nil
}

// PurgeClosed implements Store.
func (m *MemStore) PurgeClosed(_ context.Context, cutoff time.Time, limit int, dryRun bool) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	closedAt := func(d DSR) time.Time {
		if d.CompletedAt != nil {
			return *d.CompletedAt
		}
		return d.UpdatedAt
	}
	var rows []DSR
	for _, d := range m.rows {
		if !d.Status.Open() && d.ExportFileID == nil && closedAt(d).Before(cutoff) {
			rows = append(rows, d)
		}
	}
	if dryRun {
		return len(rows), nil
	}
	slices.SortFunc(rows, func(a, b DSR) int { return cmp.Or(closedAt(a).Compare(closedAt(b)), a.ID.Compare(b.ID)) })
	rows = rows[:min(len(rows), max(limit, 0))]
	for _, d := range rows {
		delete(m.rows, d.ID)
	}
	return len(rows), nil
}
