package outbox

import (
	"cmp"
	"context"
	"slices"
	"sync"
	"time"
	"uuid"

	"koro/internal/platform/db"
)

// NewMemStore returns the in-memory store of tests (05 §13.2). Inserted
// rows become visible when the transaction carried by ctx commits
// (db.AfterCommit; db.NopTx runs the hooks only when the outermost function
// succeeds), so a failed service call leaves no event behind.
func NewMemStore() *MemStore { return &MemStore{rows: map[uuid.UUID]*Row{}} }

// MemStore is the in-memory Store.
type MemStore struct {
	mu   sync.Mutex
	rows map[uuid.UUID]*Row
}

// Rows returns a copy of every committed row ordered by (occurred_at, id).
func (m *MemStore) Rows() []Row {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Row, 0, len(m.rows))
	for _, r := range m.rows {
		out = append(out, *r)
	}
	slices.SortFunc(out, func(a, b Row) int { return cmp.Or(a.OccurredAt.Compare(b.OccurredAt), a.ID.Compare(b.ID)) })
	return out
}

// Insert implements Store.
func (m *MemStore) Insert(ctx context.Context, r Row) error {
	db.AfterCommit(ctx, func(context.Context) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.rows[r.ID] = &r
	})
	return nil
}

// Due implements Store (no locking: tests run one relay).
func (m *MemStore) Due(_ context.Context, now time.Time, limit int) ([]Row, error) {
	var due []Row
	for _, r := range m.Rows() {
		if r.PublishedAt == nil && !r.AvailableAt.After(now) {
			due = append(due, r)
		}
	}
	slices.SortStableFunc(due, func(a, b Row) int { return cmp.Or(a.AvailableAt.Compare(b.AvailableAt), a.ID.Compare(b.ID)) })
	if len(due) > limit {
		due = due[:limit]
	}
	return due, nil
}

// MarkPublished implements Store.
func (m *MemStore) MarkPublished(_ context.Context, id uuid.UUID, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.rows[id]; ok {
		r.PublishedAt = &at
	}
	return nil
}

// MarkFailed implements Store.
func (m *MemStore) MarkFailed(_ context.Context, id uuid.UUID, attempts int, lastErr string, availableAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.rows[id]; ok {
		r.Attempts, r.LastError, r.AvailableAt = attempts, &lastErr, availableAt
	}
	return nil
}

// DeletePublished implements Store.
func (m *MemStore) DeletePublished(_ context.Context, before time.Time, limit int) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, r := range m.rows {
		if n == limit {
			break
		}
		if r.PublishedAt != nil && r.PublishedAt.Before(before) {
			delete(m.rows, id)
			n++
		}
	}
	return n, nil
}
