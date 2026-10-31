package messaging

import (
	"bytes"
	"context"
	"maps"
	"slices"
	"sync"
	"time"
	"uuid"
)

// NewMemStore returns the in-memory store of the tests (05 §13.2). now
// stamps created_at and updated_at the way the column defaults and the
// touch trigger do in PostgreSQL.
func NewMemStore(now func() time.Time) *MemStore {
	return &MemStore{now: now, requests: map[uuid.UUID]ContactRequest{}, blocks: map[[2]uuid.UUID]Block{},
		convs: map[uuid.UUID]Conversation{}, msgs: map[uuid.UUID]Message{}, states: map[[2]uuid.UUID]UserState{}}
}

// MemStore is the in-memory Store. Every method is atomic under one mutex.
type MemStore struct {
	mu       sync.Mutex
	now      func() time.Time
	requests map[uuid.UUID]ContactRequest
	blocks   map[[2]uuid.UUID]Block
	convs    map[uuid.UUID]Conversation // conversations (store_mem_conversations.go)
	msgs     map[uuid.UUID]Message      // conversation_messages
	states   map[[2]uuid.UUID]UserState // conversation_user_states: (conversation, user)
}

var _ Store = (*MemStore)(nil)

// MemSnapshot is a copy of the store's rows (Snapshot / Restore: tests that
// emulate a rolled back transaction).
type MemSnapshot struct {
	requests map[uuid.UUID]ContactRequest
	blocks   map[[2]uuid.UUID]Block
	convs    map[uuid.UUID]Conversation
	msgs     map[uuid.UUID]Message
	states   map[[2]uuid.UUID]UserState
}

// Snapshot copies the rows.
func (m *MemStore) Snapshot() MemSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return MemSnapshot{requests: maps.Clone(m.requests), blocks: maps.Clone(m.blocks), convs: maps.Clone(m.convs),
		msgs: maps.Clone(m.msgs), states: maps.Clone(m.states)}
}

// Restore puts a snapshot back.
func (m *MemStore) Restore(s MemSnapshot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests, m.blocks = maps.Clone(s.requests), maps.Clone(s.blocks)
	m.convs, m.msgs, m.states = maps.Clone(s.convs), maps.Clone(s.msgs), maps.Clone(s.states)
}

// Age moves the updated_at of a request back by d (tests of the retention
// sweep; PostgreSQL's trigger stamps it with the database clock).
func (m *MemStore) Age(id uuid.UUID, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.requests[id]; ok {
		r.UpdatedAt = r.UpdatedAt.Add(-d)
		m.requests[id] = r
	}
}

func (m *MemStore) stamp() time.Time { return m.now().UTC().Truncate(time.Microsecond) }

func cloneRequest(r ContactRequest) ContactRequest {
	if r.OpportunityPostID != nil {
		id := *r.OpportunityPostID
		r.OpportunityPostID = &id
	}
	if r.RespondedAt != nil {
		at := *r.RespondedAt
		r.RespondedAt = &at
	}
	return r
}

func compareID(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) }

// Insert implements Store.
func (m *MemStore) Insert(_ context.Context, r ContactRequest) (ContactRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.requests[r.ID]; dup {
		return ContactRequest{}, ErrRequestPending
	}
	if r.Status == "" {
		r.Status = StatusPending
	}
	if r.Status == StatusPending {
		for _, o := range m.requests {
			if o.OrgID == r.OrgID && o.StudentUserID == r.StudentUserID && o.Status == StatusPending {
				return ContactRequest{}, ErrRequestPending
			}
		}
	}
	at := m.stamp()
	r.CreatedAt, r.UpdatedAt = at, at
	m.requests[r.ID] = cloneRequest(r)
	return cloneRequest(r), nil
}

// Get implements Store.
func (m *MemStore) Get(_ context.Context, orgID, id uuid.UUID) (ContactRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.requests[id]
	if !ok || orgID == uuid.Nil() || r.OrgID != orgID {
		return ContactRequest{}, ErrNotFound
	}
	return cloneRequest(r), nil
}

// GetForStudent implements Store.
func (m *MemStore) GetForStudent(_ context.Context, studentID, id uuid.UUID) (ContactRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.requests[id]
	if !ok || studentID == uuid.Nil() || r.StudentUserID != studentID {
		return ContactRequest{}, ErrNotFound
	}
	return cloneRequest(r), nil
}

// Lock implements Store.
func (m *MemStore) Lock(_ context.Context, id uuid.UUID) (ContactRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.requests[id]
	if !ok {
		return ContactRequest{}, ErrNotFound
	}
	return cloneRequest(r), nil
}

// Transition implements Store.
func (m *MemStore) Transition(_ context.Context, id uuid.UUID, from, to Status, respondedAt *time.Time) (ContactRequest, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.requests[id]
	if !ok || r.Status != from {
		return ContactRequest{}, false, nil
	}
	r.Status, r.UpdatedAt = to, m.stamp()
	r.RespondedAt = nil
	if respondedAt != nil {
		at := respondedAt.UTC()
		r.RespondedAt = &at
	}
	m.requests[id] = r
	return cloneRequest(r), true, nil
}

// Pending implements Store.
func (m *MemStore) Pending(_ context.Context, orgID, studentID uuid.UUID) (ContactRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.requests {
		if r.OrgID == orgID && r.StudentUserID == studentID && r.Status == StatusPending {
			return cloneRequest(r), nil
		}
	}
	return ContactRequest{}, ErrNotFound
}

// DeclinedSince implements Store.
func (m *MemStore) DeclinedSince(_ context.Context, orgID, studentID uuid.UUID, since time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.requests {
		if r.OrgID == orgID && r.StudentUserID == studentID && r.Status == StatusDeclined && r.RespondedAt != nil &&
			r.RespondedAt.After(since) {
			return true, nil
		}
	}
	return false, nil
}

// matches is the effective status filter of a list (ListQuery.Status).
func (q ListQuery) matches(r ContactRequest) bool {
	return q.Status == "" || r.Effective(q.Now) == q.Status
}

func (m *MemStore) list(keep func(ContactRequest) bool, q ListQuery) []ContactRequest {
	var out []ContactRequest
	for _, r := range m.requests {
		if keep(r) && q.matches(r) && (q.After == nil || compareID(r.ID, *q.After) < 0) {
			out = append(out, cloneRequest(r))
		}
	}
	slices.SortFunc(out, func(a, b ContactRequest) int { return compareID(b.ID, a.ID) })
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out
}

// ListByOrg implements Store.
func (m *MemStore) ListByOrg(_ context.Context, orgID uuid.UUID, q ListQuery) ([]ContactRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if orgID == uuid.Nil() {
		return nil, ErrNotFound
	}
	return m.list(func(r ContactRequest) bool { return r.OrgID == orgID }, q), nil
}

// ListByStudent implements Store.
func (m *MemStore) ListByStudent(_ context.Context, studentID uuid.UUID, q ListQuery) ([]ContactRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.list(func(r ContactRequest) bool { return r.StudentUserID == studentID }, q), nil
}

// PendingOfCompany implements Store.
func (m *MemStore) PendingOfCompany(_ context.Context, orgID, studentID uuid.UUID) ([]ContactRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ContactRequest
	for _, r := range m.requests {
		if r.OrgID == orgID && r.StudentUserID == studentID && r.Status == StatusPending {
			out = append(out, cloneRequest(r))
		}
	}
	slices.SortFunc(out, func(a, b ContactRequest) int { return compareID(a.ID, b.ID) })
	return out, nil
}

// DueForExpiry implements Store.
func (m *MemStore) DueForExpiry(_ context.Context, now time.Time, limit int) ([]ContactRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ContactRequest
	for _, r := range m.requests {
		if r.Status == StatusPending && !r.ExpiresAt.After(now) {
			out = append(out, cloneRequest(r))
		}
	}
	slices.SortFunc(out, func(a, b ContactRequest) int {
		if c := a.ExpiresAt.Compare(b.ExpiresAt); c != 0 {
			return c
		}
		return compareID(a.ID, b.ID)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// InsertBlock implements Store.
func (m *MemStore) InsertBlock(_ context.Context, studentID, orgID uuid.UUID) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := [2]uuid.UUID{studentID, orgID}
	if _, ok := m.blocks[k]; ok {
		return false, nil
	}
	m.blocks[k] = Block{StudentUserID: studentID, CompanyOrgID: orgID, CreatedAt: m.stamp()}
	return true, nil
}

// DeleteBlock implements Store.
func (m *MemStore) DeleteBlock(_ context.Context, studentID, orgID uuid.UUID) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := [2]uuid.UUID{studentID, orgID}
	if _, ok := m.blocks[k]; !ok {
		return false, nil
	}
	delete(m.blocks, k)
	return true, nil
}

// Blocked implements Store.
func (m *MemStore) Blocked(_ context.Context, studentID, orgID uuid.UUID) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.blocks[[2]uuid.UUID{studentID, orgID}]
	return ok, nil
}

// before reports whether b sorts after the position k in a newest-first
// list.
func (k BlockKey) before(b Block) bool {
	if c := b.CreatedAt.Compare(k.CreatedAt); c != 0 {
		return c < 0
	}
	return compareID(b.CompanyOrgID, k.OrgID) < 0
}

// Blocks implements Store.
func (m *MemStore) Blocks(_ context.Context, studentID uuid.UUID, after *BlockKey, limit int) ([]Block, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Block
	for _, b := range m.blocks {
		if b.StudentUserID == studentID && (after == nil || after.before(b)) {
			out = append(out, b)
		}
	}
	slices.SortFunc(out, func(a, b Block) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return compareID(b.CompanyOrgID, a.CompanyOrgID)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemStore) where(keep func(ContactRequest) bool) []ContactRequest {
	var out []ContactRequest
	for _, r := range m.requests {
		if keep(r) {
			out = append(out, cloneRequest(r))
		}
	}
	slices.SortFunc(out, func(a, b ContactRequest) int { return compareID(a.ID, b.ID) })
	return out
}

// ReceivedBy implements Store.
func (m *MemStore) ReceivedBy(_ context.Context, studentID uuid.UUID) ([]ContactRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.where(func(r ContactRequest) bool { return r.StudentUserID == studentID }), nil
}

// SentBy implements Store.
func (m *MemStore) SentBy(_ context.Context, senderID uuid.UUID) ([]ContactRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.where(func(r ContactRequest) bool { return r.SenderUserID == senderID }), nil
}

// DeleteReceived implements Store.
func (m *MemStore) DeleteReceived(_ context.Context, studentID uuid.UUID) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, r := range m.requests {
		if r.StudentUserID == studentID {
			m.deleteRequest(id)
			n++
		}
	}
	return n, nil
}

// deleteRequest removes a request; its conversation stays without it
// (conversations_contact_request_id_fkey ON DELETE SET NULL).
func (m *MemStore) deleteRequest(id uuid.UUID) {
	delete(m.requests, id)
	for cid, c := range m.convs {
		if c.ContactRequestID != nil && *c.ContactRequestID == id {
			c.ContactRequestID = nil
			m.convs[cid] = c
		}
	}
}

// DeleteBlocks implements Store.
func (m *MemStore) DeleteBlocks(_ context.Context, studentID uuid.UUID) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for k := range m.blocks {
		if k[0] == studentID {
			delete(m.blocks, k)
			n++
		}
	}
	return n, nil
}

// closedBefore lists the closed requests whose closing moment is before
// cutoff, oldest first.
func (m *MemStore) closedBefore(cutoff time.Time) []ContactRequest {
	out := m.where(func(r ContactRequest) bool { return r.Status != StatusPending && r.UpdatedAt.Before(cutoff) })
	slices.SortStableFunc(out, func(a, b ContactRequest) int { return a.UpdatedAt.Compare(b.UpdatedAt) })
	return out
}

// CountClosedBefore implements Store.
func (m *MemStore) CountClosedBefore(_ context.Context, cutoff time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.closedBefore(cutoff)), nil
}

// PurgeClosed implements Store.
func (m *MemStore) PurgeClosed(_ context.Context, cutoff time.Time, limit int) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := m.closedBefore(cutoff)
	if len(rows) > limit {
		rows = rows[:limit]
	}
	for _, r := range rows {
		m.deleteRequest(r.ID)
	}
	return len(rows), nil
}
