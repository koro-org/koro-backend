package talent

import (
	"context"
	"slices"
	"time"
	"uuid"
)

// StampReversalDropped implements Store.
func (m *MemStore) StampReversalDropped(ctx context.Context, at time.Time) error {
	if err := m.check(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.ReversalsDropped++
	at = at.UTC().Truncate(time.Microsecond)
	if cur := m.state.LastDroppedAt; cur == nil || cur.Before(at) {
		m.state.LastDroppedAt = &at
	}
	return nil
}

// CollectionState implements Store.
func (m *MemStore) CollectionState(ctx context.Context) (CollectionState, error) {
	if err := m.check(ctx); err != nil {
		return CollectionState{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.state
	if st.LastDroppedAt != nil {
		st.LastDroppedAt = new(*st.LastDroppedAt)
	}
	if st.ReconciledAt != nil {
		st.ReconciledAt = new(*st.ReconciledAt)
	}
	return st, nil
}

// FinishReconcile implements Store; it keeps the CHECK of 000138
// (reconciled_drops ≤ reversals_dropped).
func (m *MemStore) FinishReconcile(ctx context.Context, through int64, at time.Time) error {
	if err := m.check(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case through < 0 || through > m.state.ReversalsDropped:
		return errMemCheck
	case through < m.state.ReconciledDrops, m.state.ReversalsDropped == 0:
		return nil // an older run, or nothing was ever dropped (no row in PostgreSQL)
	}
	at = at.UTC().Truncate(time.Microsecond)
	m.state.ReconciledDrops, m.state.ReconciledAt = through, &at
	return nil
}

// Unreversed implements Store.
func (m *MemStore) Unreversed(ctx context.Context, signals []string, after uuid.UUID, limit int) ([]PointEvent, error) {
	if err := m.check(ctx); err != nil {
		return nil, err
	}
	if err := checkBatch(limit); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	reversed := map[uuid.UUID]bool{}
	for _, p := range m.points {
		if p.ReversalOf != nil {
			reversed[*p.ReversalOf] = true
		}
	}
	var out []PointEvent
	for _, p := range m.points {
		if p.ReversalOf == nil && !reversed[p.ID] && slices.Contains(signals, p.Signal) && compareUUID(p.ID, after) > 0 {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b PointEvent) int { return compareUUID(a.ID, b.ID) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
