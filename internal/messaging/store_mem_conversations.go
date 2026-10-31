package messaging

import (
	"context"
	"slices"
	"time"
	"uuid"

	"koro/internal/platform/problem"
)

// The conversation part of MemStore (conversationStore; BE-162).

func cloneConversation(c Conversation) Conversation {
	if c.ContactRequestID != nil {
		id := *c.ContactRequestID
		c.ContactRequestID = &id
	}
	if c.ClosedBy != nil {
		by := *c.ClosedBy
		c.ClosedBy = &by
	}
	if c.ClosedAt != nil {
		at := *c.ClosedAt
		c.ClosedAt = &at
	}
	return c
}

func cloneState(st UserState) UserState {
	if st.LastReadMessageID != nil {
		id := *st.LastReadMessageID
		st.LastReadMessageID = &id
	}
	if st.ReadAt != nil {
		at := *st.ReadAt
		st.ReadAt = &at
	}
	if st.MutedAt != nil {
		at := *st.MutedAt
		st.MutedAt = &at
	}
	return st
}

// InsertConversation implements Store.
func (m *MemStore) InsertConversation(_ context.Context, c Conversation) (Conversation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.convs[c.ID]; dup {
		return Conversation{}, problem.ErrConflict
	}
	for _, o := range m.convs {
		if c.ContactRequestID != nil && o.ContactRequestID != nil && *o.ContactRequestID == *c.ContactRequestID {
			return Conversation{}, problem.ErrConflict
		}
	}
	if c.Status == "" {
		c.Status = ConversationOpen
	}
	at := m.stamp()
	c.CreatedAt, c.UpdatedAt = at, at
	m.convs[c.ID] = cloneConversation(c)
	return cloneConversation(c), nil
}

// ConversationOfOrg implements Store.
func (m *MemStore) ConversationOfOrg(_ context.Context, orgID, id uuid.UUID) (Conversation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.convs[id]
	if !ok || orgID == uuid.Nil() || c.OrgID != orgID {
		return Conversation{}, ErrNotFound
	}
	return cloneConversation(c), nil
}

// ConversationOfStudent implements Store.
func (m *MemStore) ConversationOfStudent(_ context.Context, studentID, id uuid.UUID) (Conversation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.convs[id]
	if !ok || studentID == uuid.Nil() || c.StudentUserID != studentID {
		return Conversation{}, ErrNotFound
	}
	return cloneConversation(c), nil
}

// LockConversation implements Store.
func (m *MemStore) LockConversation(_ context.Context, id uuid.UUID) (Conversation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.convs[id]
	if !ok {
		return Conversation{}, ErrNotFound
	}
	return cloneConversation(c), nil
}

// compareActivity orders conversations by last_activity_at DESC, id DESC.
func compareActivity(a, b Conversation) int {
	if c := b.LastActivityAt.Compare(a.LastActivityAt); c != 0 {
		return c
	}
	return compareID(b.ID, a.ID)
}

// ConversationRows implements Store.
func (m *MemStore) ConversationRows(_ context.Context, q ConversationQuery) ([]ConversationRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if (q.OrgID == nil) == (q.StudentUserID == nil) {
		return nil, errMissingScope
	}
	var rows []Conversation
	for _, c := range m.convs {
		switch {
		case q.OrgID != nil && c.OrgID != *q.OrgID,
			q.StudentUserID != nil && c.StudentUserID != *q.StudentUserID,
			q.ID != nil && c.ID != *q.ID,
			q.Status != "" && c.Status != q.Status:
			continue
		case q.After != nil && compareActivity(c, Conversation{ID: q.After.ID, LastActivityAt: q.After.At}) <= 0:
			continue
		}
		rows = append(rows, c)
	}
	slices.SortFunc(rows, compareActivity)
	if q.Limit > 0 && len(rows) > q.Limit {
		rows = rows[:q.Limit]
	}
	out := make([]ConversationRow, len(rows))
	for i, c := range rows {
		r := ConversationRow{Conversation: cloneConversation(c)}
		if st, ok := m.states[[2]uuid.UUID{c.ID, q.Viewer}]; ok {
			st = cloneState(st)
			r.Muted, r.LastReadMessageID = st.MutedAt != nil, st.LastReadMessageID
		}
		for _, msg := range m.msgs {
			if msg.ConversationID != c.ID {
				continue
			}
			if r.Last == nil || compareID(msg.ID, r.Last.ID) > 0 {
				last := msg
				r.Last = &last
			}
			if msg.SenderSide != q.ViewerSide && (r.LastReadMessageID == nil || compareID(msg.ID, *r.LastReadMessageID) > 0) {
				r.Unread++
			}
		}
		out[i] = r
	}
	return out, nil
}

// OpenConversationsOf implements Store.
func (m *MemStore) OpenConversationsOf(_ context.Context, orgID, studentID uuid.UUID) ([]Conversation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Conversation
	for _, c := range m.convs {
		if c.OrgID == orgID && c.StudentUserID == studentID && c.Status == ConversationOpen {
			out = append(out, cloneConversation(c))
		}
	}
	slices.SortFunc(out, func(a, b Conversation) int { return compareID(a.ID, b.ID) })
	return out, nil
}

// CloseConversation implements Store.
func (m *MemStore) CloseConversation(_ context.Context, id uuid.UUID, by Side, at time.Time) (Conversation, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.convs[id]
	if !ok || c.Status != ConversationOpen {
		return Conversation{}, false, nil
	}
	at = at.UTC()
	c.Status, c.ClosedBy, c.ClosedAt, c.LastActivityAt, c.UpdatedAt = ConversationClosed, &by, &at, at, m.stamp()
	m.convs[id] = cloneConversation(c)
	return cloneConversation(c), true, nil
}

// InsertMessage implements Store.
func (m *MemStore) InsertMessage(_ context.Context, msg Message) (Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.convs[msg.ConversationID]
	if !ok {
		return Message{}, problem.ErrConflict // conversation_messages_conversation_id_fkey
	}
	if _, dup := m.msgs[msg.ID]; dup {
		return Message{}, problem.ErrConflict
	}
	msg.CreatedAt = msg.CreatedAt.UTC()
	m.msgs[msg.ID] = msg
	c.LastActivityAt, c.UpdatedAt = msg.CreatedAt, m.stamp()
	if msg.SenderSide == SideCompany {
		c.CompanyUnanswered++
	} else {
		c.CompanyUnanswered = 0
	}
	m.convs[c.ID] = c
	return msg, nil
}

// MessageIn implements Store.
func (m *MemStore) MessageIn(_ context.Context, conversationID, id uuid.UUID) (Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	msg, ok := m.msgs[id]
	if !ok || msg.ConversationID != conversationID {
		return Message{}, ErrNotFound
	}
	return msg, nil
}

// messagesOf returns the messages of a conversation, oldest first.
func (m *MemStore) messagesOf(conversationID uuid.UUID) []Message {
	var out []Message
	for _, msg := range m.msgs {
		if msg.ConversationID == conversationID {
			out = append(out, msg)
		}
	}
	slices.SortFunc(out, func(a, b Message) int { return compareID(a.ID, b.ID) })
	return out
}

// Messages implements Store.
func (m *MemStore) Messages(_ context.Context, conversationID uuid.UUID, before *uuid.UUID, limit int) ([]Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	all := m.messagesOf(conversationID)
	slices.Reverse(all)
	var out []Message
	for _, msg := range all {
		if before != nil && compareID(msg.ID, *before) >= 0 {
			continue
		}
		out = append(out, msg)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// CompanySenders implements Store.
func (m *MemStore) CompanySenders(_ context.Context, conversationID uuid.UUID) ([]uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []uuid.UUID
	for _, msg := range m.messagesOf(conversationID) {
		if msg.SenderSide == SideCompany && !slices.Contains(out, msg.SenderUserID) {
			out = append(out, msg.SenderUserID)
		}
	}
	return out, nil
}

// State implements Store.
func (m *MemStore) State(_ context.Context, conversationID, userID uuid.UUID) (UserState, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.states[[2]uuid.UUID{conversationID, userID}]
	return cloneState(st), ok, nil
}

// state returns the user's state, a new one when there is none.
func (m *MemStore) state(conversationID, userID uuid.UUID) UserState {
	if st, ok := m.states[[2]uuid.UUID{conversationID, userID}]; ok {
		return cloneState(st)
	}
	return UserState{ConversationID: conversationID, UserID: userID}
}

// MarkRead implements Store.
func (m *MemStore) MarkRead(_ context.Context, conversationID, userID, messageID uuid.UUID, at time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.convs[conversationID]; !ok {
		return false, problem.ErrConflict
	}
	st := m.state(conversationID, userID)
	if st.LastReadMessageID != nil && compareID(*st.LastReadMessageID, messageID) >= 0 {
		return false, nil
	}
	at = at.UTC()
	st.LastReadMessageID, st.ReadAt, st.UpdatedAt = &messageID, &at, m.stamp()
	m.states[[2]uuid.UUID{conversationID, userID}] = st
	return true, nil
}

// SetMuted implements Store.
func (m *MemStore) SetMuted(_ context.Context, conversationID, userID uuid.UUID, muted bool, at time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.convs[conversationID]; !ok {
		return false, problem.ErrConflict
	}
	st := m.state(conversationID, userID)
	if (st.MutedAt != nil) == muted {
		return false, nil
	}
	st.MutedAt = nil
	if muted {
		at = at.UTC()
		st.MutedAt = &at
	}
	st.UpdatedAt = m.stamp()
	m.states[[2]uuid.UUID{conversationID, userID}] = st
	return true, nil
}

// StudentConversations implements Store.
func (m *MemStore) StudentConversations(_ context.Context, studentID uuid.UUID) ([]Conversation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Conversation
	for _, c := range m.convs {
		if c.StudentUserID == studentID {
			out = append(out, cloneConversation(c))
		}
	}
	slices.SortFunc(out, func(a, b Conversation) int { return compareID(a.ID, b.ID) })
	return out, nil
}

// AllMessages implements Store.
func (m *MemStore) AllMessages(_ context.Context, conversationID uuid.UUID) ([]Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.messagesOf(conversationID), nil
}

// MessagesSentBy implements Store.
func (m *MemStore) MessagesSentBy(_ context.Context, userID uuid.UUID) ([]Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Message
	for _, msg := range m.msgs {
		if msg.SenderUserID == userID {
			out = append(out, msg)
		}
	}
	slices.SortFunc(out, func(a, b Message) int { return compareID(a.ID, b.ID) })
	return out, nil
}

// UserStates implements Store.
func (m *MemStore) UserStates(_ context.Context, userID uuid.UUID) ([]UserState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []UserState
	for k, st := range m.states {
		if k[1] == userID {
			out = append(out, cloneState(st))
		}
	}
	slices.SortFunc(out, func(a, b UserState) int { return compareID(a.ConversationID, b.ConversationID) })
	return out, nil
}

// deleteConversation removes a conversation with its messages and states
// (ON DELETE CASCADE).
func (m *MemStore) deleteConversation(id uuid.UUID) {
	delete(m.convs, id)
	for mid, msg := range m.msgs {
		if msg.ConversationID == id {
			delete(m.msgs, mid)
		}
	}
	for k := range m.states {
		if k[0] == id {
			delete(m.states, k)
		}
	}
}

// DeleteStudentConversations implements Store.
func (m *MemStore) DeleteStudentConversations(_ context.Context, studentID uuid.UUID) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, c := range m.convs {
		if c.StudentUserID == studentID {
			m.deleteConversation(id)
			n++
		}
	}
	return n, nil
}

// DeleteUserStates implements Store.
func (m *MemStore) DeleteUserStates(_ context.Context, userID uuid.UUID) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for k := range m.states {
		if k[1] == userID {
			delete(m.states, k)
			n++
		}
	}
	return n, nil
}

// inactiveBefore returns the conversations whose last activity is before
// cutoff, oldest first.
func (m *MemStore) inactiveBefore(cutoff time.Time) []Conversation {
	var out []Conversation
	for _, c := range m.convs {
		if c.LastActivityAt.Before(cutoff) {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b Conversation) int { return -compareActivity(a, b) })
	return out
}

// CountInactiveBefore implements Store.
func (m *MemStore) CountInactiveBefore(_ context.Context, cutoff time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.inactiveBefore(cutoff)), nil
}

// PurgeInactive implements Store.
func (m *MemStore) PurgeInactive(_ context.Context, cutoff time.Time, limit int) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := m.inactiveBefore(cutoff)
	if len(rows) > limit {
		rows = rows[:limit]
	}
	for _, c := range rows {
		m.deleteConversation(c.ID)
	}
	return len(rows), nil
}

// AgeConversation moves the last activity of a conversation back by d
// (tests of the retention sweep).
func (m *MemStore) AgeConversation(id uuid.UUID, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.convs[id]; ok {
		c.LastActivityAt = c.LastActivityAt.Add(-d)
		m.convs[id] = c
	}
}
