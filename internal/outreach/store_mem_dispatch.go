package outreach

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"

	"koro/internal/platform/db"
)

// The outreach_dispatches part of MemStore (BE-035). The composite FKs to
// posts and segments (06 H-38) are not modelled: the service derives the
// org from the post, and the GORM contract test proves the database side.

// PutDispatch stores d as is (test fixtures: fixed timestamps, any status).
func (m *MemStore) PutDispatch(d Dispatch) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := checkDispatch(d); err != nil {
		return err
	}
	m.dispatches[d.ID] = d.clone()
	return nil
}

// checkDispatch applies the CHECK constraints of 02 §13.2.
func checkDispatch(d Dispatch) error {
	nonNeg := func(n *int) bool { return n == nil || *n >= 0 }
	switch {
	case d.OrgID == uuid.Nil(), d.PostID == uuid.Nil(), d.CreatedBy == uuid.Nil():
		return errCheck
	case d.AudienceDimCount < 1 || d.AudienceDimCount > 4: // outreach_dispatches_audience_dim_count_chk
		return errCheck
	case len(d.Channels) < 1 || slices.ContainsFunc(d.Channels, func(c string) bool { return !Channel(c).valid() }):
		return errCheck // outreach_dispatches_channels_chk
	case utf8.RuneCountInString(d.TitleSnapshot) > maxTitle, !strings.HasPrefix(d.DeepLink, "/"):
		return errCheck
	case !slices.Contains([]DispatchStatus{DispatchScheduled, DispatchRunning, DispatchCompleted, DispatchCancelled,
		DispatchFailed}, d.Status):
		return errCheck
	case !nonNeg(d.EligibleCount) || !nonNeg(d.ConsentedCount) || !nonNeg(d.SentCount):
		return errCheck
	case d.Status == DispatchCompleted && d.CompletedAt == nil: // outreach_dispatches_completed_chk
		return errCheck
	case d.LastError != nil && utf8.RuneCountInString(*d.LastError) > 2000:
		return errCheck
	}
	return nil
}

// InsertDispatch implements Store.
func (m *MemStore) InsertDispatch(_ context.Context, d Dispatch) (Dispatch, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d.ID == uuid.Nil() || d.OrgID == uuid.Nil() {
		return Dispatch{}, fmt.Errorf("outreach: insert dispatch: %w", db.ErrMissingOrgScope)
	}
	if d.Status == "" {
		d.Status = DispatchScheduled
	}
	if err := checkDispatch(d); err != nil {
		return Dispatch{}, err
	}
	for _, cur := range m.dispatches {
		if cur.PostID == d.PostID {
			return Dispatch{}, ErrDispatchExists // outreach_dispatches_post_id_uq
		}
	}
	if _, dup := m.dispatches[d.ID]; dup {
		return Dispatch{}, ErrDispatchExists
	}
	now := m.stamp()
	d.CreatedAt, d.UpdatedAt = now, now
	m.dispatches[d.ID] = d.clone()
	return d.clone(), nil
}

func (m *MemStore) dispatch(orgID, id uuid.UUID) (Dispatch, error) {
	if orgID == uuid.Nil() {
		return Dispatch{}, db.ErrMissingOrgScope
	}
	d, ok := m.dispatches[id]
	if !ok || d.OrgID != orgID {
		return Dispatch{}, ErrNotFound
	}
	return d.clone(), nil
}

// GetDispatch implements Store.
func (m *MemStore) GetDispatch(_ context.Context, orgID, id uuid.UUID) (Dispatch, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dispatch(orgID, id)
}

// LockDispatch implements Store (no transactions: GetDispatch).
func (m *MemStore) LockDispatch(ctx context.Context, orgID, id uuid.UUID) (Dispatch, error) {
	return m.GetDispatch(ctx, orgID, id)
}

// DispatchOfPost implements Store.
func (m *MemStore) DispatchOfPost(_ context.Context, orgID, postID uuid.UUID, _ bool) (Dispatch, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if orgID == uuid.Nil() {
		return Dispatch{}, db.ErrMissingOrgScope
	}
	for _, d := range m.dispatches {
		if d.PostID == postID && d.OrgID == orgID {
			return d.clone(), nil
		}
	}
	return Dispatch{}, ErrNotFound
}

// DispatchIDsOfPosts implements Store.
func (m *MemStore) DispatchIDsOfPosts(_ context.Context, orgID uuid.UUID, postIDs []uuid.UUID) (map[uuid.UUID]uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if orgID == uuid.Nil() {
		return nil, db.ErrMissingOrgScope
	}
	out := map[uuid.UUID]uuid.UUID{}
	for _, d := range m.dispatches {
		if d.OrgID == orgID && slices.Contains(postIDs, d.PostID) {
			out[d.PostID] = d.ID
		}
	}
	return out, nil
}

// ListDispatches implements Store.
func (m *MemStore) ListDispatches(_ context.Context, orgID uuid.UUID, after *uuid.UUID, limit int) ([]Dispatch, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if orgID == uuid.Nil() {
		return nil, db.ErrMissingOrgScope
	}
	var out []Dispatch
	for _, d := range m.dispatches {
		if d.OrgID != orgID || (after != nil && d.ID.Compare(*after) >= 0) {
			continue
		}
		out = append(out, d.clone())
	}
	slices.SortFunc(out, func(a, b Dispatch) int { return b.ID.Compare(a.ID) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// DeleteDispatch implements Store.
func (m *MemStore) DeleteDispatch(_ context.Context, orgID, id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.dispatch(orgID, id); err != nil {
		return err
	}
	delete(m.dispatches, id)
	return nil
}

// TransitionDispatch implements Store.
func (m *MemStore) TransitionDispatch(_ context.Context, orgID, id uuid.UUID, from, to DispatchStatus, lastError *string, at time.Time) (Dispatch, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, err := m.dispatch(orgID, id)
	switch {
	case errors.Is(err, ErrNotFound):
		return Dispatch{}, false, nil
	case err != nil:
		return Dispatch{}, false, err
	case cur.Status != from:
		return Dispatch{}, false, nil
	}
	next := cur
	next.Status, next.LastError, next.CompletedAt = to, nil, nil
	if lastError != nil {
		v := *lastError
		next.LastError = &v
	}
	if to.Terminal() {
		t := at
		next.CompletedAt = &t
	}
	if err := checkDispatch(next); err != nil {
		return Dispatch{}, false, err
	}
	next.UpdatedAt = m.stamp()
	m.dispatches[id] = next.clone()
	return next.clone(), true, nil
}

// CompleteDispatch implements Store.
func (m *MemStore) CompleteDispatch(_ context.Context, orgID, id uuid.UUID, c DispatchCounts, at time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, err := m.dispatch(orgID, id)
	switch {
	case errors.Is(err, ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	case cur.Status != DispatchRunning:
		return false, nil
	}
	t := at
	cur.Status, cur.CompletedAt, cur.LastError = DispatchCompleted, &t, nil
	cur.EligibleCount, cur.ConsentedCount, cur.SentCount = cloneInt(c.Eligible), cloneInt(c.Consented), cloneInt(c.Sent)
	if err := checkDispatch(cur); err != nil {
		return false, err
	}
	cur.UpdatedAt = m.stamp()
	m.dispatches[id] = cur.clone()
	return true, nil
}

// DispatchByID implements Store.
func (m *MemStore) DispatchByID(_ context.Context, id uuid.UUID) (Dispatch, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.dispatches[id]
	if !ok {
		return Dispatch{}, ErrNotFound
	}
	return d.clone(), nil
}

// ClaimDispatch implements Store (the claim's age in the store clock).
func (m *MemStore) ClaimDispatch(_ context.Context, id uuid.UUID, now time.Time, lease time.Duration) (Dispatch, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.dispatches[id]
	if !ok {
		return Dispatch{}, false, nil
	}
	stale := d.Status == DispatchRunning && !d.UpdatedAt.After(m.stamp().Add(-lease))
	if d.Status != DispatchScheduled && !stale {
		return Dispatch{}, false, nil
	}
	d.Status = DispatchRunning
	if d.StartedAt == nil {
		t := now
		d.StartedAt = &t
	}
	d.UpdatedAt = m.stamp()
	m.dispatches[id] = d.clone()
	return d.clone(), true, nil
}

// TouchDispatch implements Store.
func (m *MemStore) TouchDispatch(_ context.Context, id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d, ok := m.dispatches[id]; ok && d.Status == DispatchRunning {
		d.UpdatedAt = m.stamp()
		m.dispatches[id] = d
	}
	return nil
}

// PendingDispatches implements Store.
func (m *MemStore) PendingDispatches(_ context.Context, scheduledBefore time.Time, lease time.Duration, after uuid.UUID, limit int) ([]Dispatch, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	staleBefore := m.stamp().Add(-lease)
	var out []Dispatch
	for _, d := range m.dispatches {
		if d.ID.Compare(after) <= 0 {
			continue
		}
		if (d.Status == DispatchScheduled && d.CreatedAt.Before(scheduledBefore)) ||
			(d.Status == DispatchRunning && !d.UpdatedAt.After(staleBefore)) {
			out = append(out, d.clone())
		}
	}
	slices.SortFunc(out, func(a, b Dispatch) int { return a.ID.Compare(b.ID) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// PurgeDispatches implements Store.
func (m *MemStore) PurgeDispatches(_ context.Context, cutoff time.Time, limit int, dryRun bool) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var due []Dispatch
	for _, d := range m.dispatches {
		if d.Status.Terminal() && d.CreatedAt.Before(cutoff) {
			due = append(due, d)
		}
	}
	if dryRun {
		return len(due), nil
	}
	slices.SortFunc(due, func(a, b Dispatch) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return a.ID.Compare(b.ID)
	})
	if len(due) > limit {
		due = due[:limit]
	}
	for _, d := range due {
		delete(m.dispatches, d.ID)
	}
	return len(due), nil
}
