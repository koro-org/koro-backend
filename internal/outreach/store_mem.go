package outreach

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
	"unicode/utf8"
	"uuid"

	"koro/internal/platform/db"
	"koro/internal/platform/problem"
)

// errCheck mirrors a CHECK violation of 02 §13.1 (the GORM store gets 422
// from db.Translate).
var errCheck = problem.ErrValidation.Wrap(errors.New("outreach: check constraint violated"))

// MemStore is the in-memory Store of service, wiring and matrix tests (05
// §13.2). It follows the GORM store's contract, including the CHECK
// constraints of segments; every method is atomic under one mutex and it
// has no transactions (db.NopTx).
type MemStore struct {
	mu   sync.Mutex
	rows map[uuid.UUID]Segment
	// now stamps created_at and updated_at like the column default and the
	// segments_touch_updated_at trigger.
	now func() time.Time
}

var _ Store = (*MemStore)(nil)

// NewMemStore returns an empty in-memory store; now stamps the timestamps.
func NewMemStore(now func() time.Time) *MemStore {
	return &MemStore{rows: map[uuid.UUID]Segment{}, now: now}
}

// Put stores s as is (test fixtures: fixed timestamps for a known ETag).
func (m *MemStore) Put(s Segment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := check(s); err != nil {
		return err
	}
	m.rows[s.ID] = s.clone()
	return nil
}

// check applies the CHECK constraints of 02 §13.1.
func check(s Segment) error {
	switch n := utf8.RuneCountInString(s.Name); {
	case s.OrgID == uuid.Nil(), s.CreatedBy == uuid.Nil():
		return errCheck
	case n < 2 || n > 100: // segments_name_chk
		return errCheck
	case s.AudienceDimCount < 1 || s.AudienceDimCount > 4: // segments_audience_dim_count_chk
		return errCheck
	case s.LastCount != nil && *s.LastCount < KThreshold: // segments_last_count_chk
		return errCheck
	}
	return nil
}

func (m *MemStore) stamp() time.Time { return m.now().UTC().Truncate(time.Microsecond) }

// Insert implements Store.
func (m *MemStore) Insert(_ context.Context, s Segment) (Segment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.ID == uuid.Nil() || s.OrgID == uuid.Nil() {
		return Segment{}, fmt.Errorf("outreach: insert segment: %w", db.ErrMissingOrgScope)
	}
	if err := check(s); err != nil {
		return Segment{}, err
	}
	if _, dup := m.rows[s.ID]; dup {
		return Segment{}, problem.ErrConflict
	}
	now := m.stamp()
	s.CreatedAt, s.UpdatedAt = now, now
	m.rows[s.ID] = s.clone()
	return s.clone(), nil
}

// Get implements Store.
func (m *MemStore) Get(_ context.Context, orgID, id uuid.UUID) (Segment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.get(orgID, id)
}

// Lock implements Store (no transactions: Get).
func (m *MemStore) Lock(ctx context.Context, orgID, id uuid.UUID) (Segment, error) {
	return m.Get(ctx, orgID, id)
}

func (m *MemStore) get(orgID, id uuid.UUID) (Segment, error) {
	if orgID == uuid.Nil() {
		return Segment{}, db.ErrMissingOrgScope
	}
	s, ok := m.rows[id]
	if !ok || s.OrgID != orgID {
		return Segment{}, ErrNotFound
	}
	return s.clone(), nil
}

// Update implements Store.
func (m *MemStore) Update(_ context.Context, s Segment) (Segment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, err := m.get(s.OrgID, s.ID)
	if err != nil {
		return Segment{}, err
	}
	next := cur
	next.Name, next.Definition, next.AudienceKeys, next.AudienceDimCount = s.Name, s.Definition, s.AudienceKeys, s.AudienceDimCount
	next.LastCount, next.LastCountedAt, next.ArchivedAt = s.LastCount, s.LastCountedAt, s.ArchivedAt
	if err := check(next); err != nil {
		return Segment{}, err
	}
	next.UpdatedAt = m.stamp()
	m.rows[s.ID] = next.clone()
	return next.clone(), nil
}

// List implements Store.
func (m *MemStore) List(_ context.Context, orgID uuid.UUID, archived bool, after *uuid.UUID, limit int) ([]Segment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if orgID == uuid.Nil() {
		return nil, db.ErrMissingOrgScope
	}
	var out []Segment
	for _, s := range m.rows {
		if s.OrgID != orgID || s.Archived() != archived || (after != nil && s.ID.Compare(*after) >= 0) {
			continue
		}
		out = append(out, s.clone())
	}
	slices.SortFunc(out, func(a, b Segment) int { return b.ID.Compare(a.ID) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ActiveAfter implements Store.
func (m *MemStore) ActiveAfter(_ context.Context, after uuid.UUID, limit int) ([]Segment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Segment
	for _, s := range m.rows {
		if !s.Archived() && s.ID.Compare(after) > 0 {
			out = append(out, s.clone())
		}
	}
	slices.SortFunc(out, func(a, b Segment) int { return a.ID.Compare(b.ID) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// SetCount implements Store.
func (m *MemStore) SetCount(_ context.Context, orgID, id uuid.UUID, count *int, at time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, err := m.get(orgID, id)
	switch {
	case errors.Is(err, ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	case cur.Archived():
		return false, nil
	}
	cur.LastCount, cur.LastCountedAt = count, &at
	if err := check(cur); err != nil {
		return false, err
	}
	cur.UpdatedAt = m.stamp()
	m.rows[id] = cur.clone()
	return true, nil
}
