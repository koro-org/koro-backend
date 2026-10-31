package messaging

import (
	"context"
	"fmt"
	"time"
	"uuid"

	"koro/internal/platform/db"
)

// The conversation part of the GORM store (conversationStore; BE-162):
// conversations, conversation_messages and conversation_user_states.

const conversationCols = `id, contact_request_id, org_id, student_user_id, company_user_id, status, closed_by, closed_at,
	company_unanswered, last_activity_at, created_at, updated_at`

const messageCols = `id, conversation_id, sender_side, sender_user_id, body, created_at`

// conversations runs a conversation query.
//
// cross-org: an executor; every caller writes its own scope into sql.
func (s gormStore) conversations(ctx context.Context, op, sql string, args map[string]any) ([]Conversation, error) {
	var out []Conversation
	if err := db.Conn(ctx, s.orm).Raw(sql, args).Scan(&out).Error; err != nil {
		return nil, wrap(op, err)
	}
	return out, nil
}

// conversation runs a conversation query expecting one row (ErrNotFound
// without).
//
// cross-org: an executor; every caller writes its own scope into sql.
func (s gormStore) conversation(ctx context.Context, op, sql string, args map[string]any) (Conversation, error) {
	rows, err := s.conversations(ctx, op, sql, args)
	switch {
	case err != nil:
		return Conversation{}, err
	case len(rows) == 0:
		return Conversation{}, ErrNotFound
	}
	return rows[0], nil
}

// messages runs a message query.
//
// cross-org: an executor; every caller scopes sql by conversation or sender.
func (s gormStore) messages(ctx context.Context, op, sql string, args map[string]any) ([]Message, error) {
	var out []Message
	if err := db.Conn(ctx, s.orm).Raw(sql, args).Scan(&out).Error; err != nil {
		return nil, wrap(op, err)
	}
	return out, nil
}

// InsertConversation implements Store. created_at and updated_at come
// from the column defaults; last_activity_at is the service's time.
//
// cross-org: org_id is the row's own scope (the service copies it from the
// accepted request; the composite FK binds it to a company org).
func (s gormStore) InsertConversation(ctx context.Context, c Conversation) (Conversation, error) {
	if c.Status == "" {
		c.Status = ConversationOpen
	}
	var closedBy *string
	if c.ClosedBy != nil {
		by := string(*c.ClosedBy)
		closedBy = &by
	}
	return s.conversation(ctx, "insert conversation", `INSERT INTO conversations
		(id, contact_request_id, org_id, student_user_id, company_user_id, status, closed_by, closed_at, company_unanswered,
		 last_activity_at)
		VALUES ( @id, @request, @org_id, @student, @company_user, @status, @closed_by, @closed_at, @unanswered, @activity)
		RETURNING `+conversationCols, map[string]any{
		"id": c.ID, "request": c.ContactRequestID, "org_id": c.OrgID, "student": c.StudentUserID, "company_user": c.CompanyUserID,
		"status": string(c.Status), "closed_by": closedBy, "closed_at": c.ClosedAt, "unanswered": c.CompanyUnanswered,
		"activity": c.LastActivityAt,
	})
}

// ConversationOfOrg implements Store.
func (s gormStore) ConversationOfOrg(ctx context.Context, orgID, id uuid.UUID) (Conversation, error) {
	if orgID == uuid.Nil() {
		return Conversation{}, fmt.Errorf("messaging: get conversation: %w", db.ErrMissingOrgScope)
	}
	return s.conversation(ctx, "get conversation", `SELECT `+conversationCols+` FROM conversations
		WHERE id = @id AND org_id = @org_id`, map[string]any{"id": id, "org_id": orgID})
}

// ConversationOfStudent implements Store.
//
// cross-org: the student's own conversation, addressed by its student (IDOR).
func (s gormStore) ConversationOfStudent(ctx context.Context, studentID, id uuid.UUID) (Conversation, error) {
	return s.conversation(ctx, "get conversation of student", `SELECT `+conversationCols+` FROM conversations
		WHERE id = @id AND student_user_id = @student`, map[string]any{"id": id, "student": studentID})
}

// LockConversation implements Store.
//
// cross-org: the caller read the conversation through its org or its
// student first; the lock is by ID.
func (s gormStore) LockConversation(ctx context.Context, id uuid.UUID) (Conversation, error) {
	return s.conversation(ctx, "lock conversation", `SELECT `+conversationCols+` FROM conversations WHERE id = @id FOR UPDATE`,
		map[string]any{"id": id})
}

// conversationRowScan is one row of ConversationRows: the conversation,
// the viewer's state and the newest message (lm_*, NULL without one).
type conversationRowScan struct {
	Conversation      `gorm:"embedded"`
	Muted             bool       `gorm:"column:muted"`
	LastReadMessageID *uuid.UUID `gorm:"type:uuid;column:last_read_message_id"`
	Unread            int        `gorm:"column:unread"`
	LmID              *uuid.UUID `gorm:"type:uuid;column:lm_id"`
	LmSenderSide      *string    `gorm:"column:lm_sender_side"`
	LmSenderUserID    *uuid.UUID `gorm:"type:uuid;column:lm_sender_user_id"`
	LmBody            *string    `gorm:"column:lm_body"`
	LmCreatedAt       *time.Time `gorm:"column:lm_created_at"`
}

// ConversationRows implements Store: unread counts the other side's
// messages after the viewer's marker (uuidv7 order).
//
// cross-org: the student's scope (/me) spans every company; the company's
// is its org_id. Exactly one of them is set.
func (s gormStore) ConversationRows(ctx context.Context, q ConversationQuery) ([]ConversationRow, error) {
	args := map[string]any{"viewer": q.Viewer, "side": string(q.ViewerSide)}
	where := ""
	switch {
	case q.OrgID != nil && q.StudentUserID == nil:
		if *q.OrgID == uuid.Nil() {
			return nil, fmt.Errorf("messaging: conversation rows: %w", db.ErrMissingOrgScope)
		}
		where, args["org_id"] = `c.org_id = @org_id`, *q.OrgID
	case q.StudentUserID != nil && q.OrgID == nil:
		where, args["student"] = `c.student_user_id = @student`, *q.StudentUserID
	default:
		return nil, errMissingScope
	}
	if q.ID != nil {
		where += ` AND c.id = @id`
		args["id"] = *q.ID
	}
	if q.Status != "" {
		where += ` AND c.status = @status`
		args["status"] = string(q.Status)
	}
	if q.After != nil {
		where += ` AND (c.last_activity_at, c.id) < ( @after_at, @after_id)`
		args["after_at"], args["after_id"] = q.After.At, q.After.ID
	}
	limit := ""
	if q.Limit > 0 {
		limit, args["limit"] = ` LIMIT @limit`, q.Limit
	}
	var scan []conversationRowScan
	err := db.Conn(ctx, s.orm).Raw(`SELECT c.id, c.contact_request_id, c.org_id, c.student_user_id, c.company_user_id, c.status,
		c.closed_by, c.closed_at, c.company_unanswered, c.last_activity_at, c.created_at, c.updated_at,
		(st.muted_at IS NOT NULL) AS muted, st.last_read_message_id,
		(SELECT count(*) FROM conversation_messages m WHERE m.conversation_id = c.id AND m.sender_side <> @side
		   AND (st.last_read_message_id IS NULL OR m.id > st.last_read_message_id)) AS unread,
		lm.id AS lm_id, lm.sender_side AS lm_sender_side, lm.sender_user_id AS lm_sender_user_id, lm.body AS lm_body,
		lm.created_at AS lm_created_at
		FROM conversations c
		LEFT JOIN conversation_user_states st ON st.conversation_id = c.id AND st.user_id = @viewer
		LEFT JOIN LATERAL (SELECT id, sender_side, sender_user_id, body, created_at FROM conversation_messages
		  WHERE conversation_id = c.id ORDER BY id DESC LIMIT 1) lm ON true
		WHERE `+where+` ORDER BY c.last_activity_at DESC, c.id DESC`+limit, args).Scan(&scan).Error
	if err != nil {
		return nil, wrap("conversation rows", err)
	}
	out := make([]ConversationRow, len(scan))
	for i, r := range scan {
		out[i] = ConversationRow{Conversation: r.Conversation, Muted: r.Muted, LastReadMessageID: r.LastReadMessageID, Unread: r.Unread}
		if r.LmID != nil && r.LmSenderSide != nil && r.LmSenderUserID != nil && r.LmBody != nil && r.LmCreatedAt != nil {
			out[i].Last = &Message{ID: *r.LmID, ConversationID: r.ID, SenderSide: Side(*r.LmSenderSide),
				SenderUserID: *r.LmSenderUserID, Body: *r.LmBody, CreatedAt: *r.LmCreatedAt}
		}
	}
	return out, nil
}

// OpenConversationsOf implements Store.
func (s gormStore) OpenConversationsOf(ctx context.Context, orgID, studentID uuid.UUID) ([]Conversation, error) {
	return s.conversations(ctx, "open conversations", `SELECT `+conversationCols+` FROM conversations
		WHERE org_id = @org_id AND student_user_id = @student AND status = 'open' ORDER BY id FOR UPDATE`,
		map[string]any{"org_id": orgID, "student": studentID})
}

// CloseConversation implements Store.
//
// cross-org: a state machine step of a conversation the caller locked or
// read through its scope (the open status is the guard).
func (s gormStore) CloseConversation(ctx context.Context, id uuid.UUID, by Side, at time.Time) (Conversation, bool, error) {
	rows, err := s.conversations(ctx, "close conversation", `UPDATE conversations
		SET status = 'closed', closed_by = @by, closed_at = @at, last_activity_at = @at
		WHERE id = @id AND status = 'open' RETURNING `+conversationCols, map[string]any{"by": string(by), "at": at, "id": id})
	if err != nil || len(rows) == 0 {
		return Conversation{}, false, err
	}
	return rows[0], true, nil
}

// InsertMessage implements Store: the message and its conversation's
// activity in one statement.
//
// cross-org: a message of a conversation the caller locked through its
// scope.
func (s gormStore) InsertMessage(ctx context.Context, m Message) (Message, error) {
	rows, err := s.messages(ctx, "insert message", `WITH msg AS (
		  INSERT INTO conversation_messages (id, conversation_id, sender_side, sender_user_id, body, created_at)
		  VALUES ( @id, @conversation, @side, @sender, @body, @at) RETURNING `+messageCols+`),
		touched AS (
		  UPDATE conversations SET last_activity_at = @at,
		    company_unanswered = CASE WHEN @side = 'company' THEN company_unanswered + 1 ELSE 0 END
		  WHERE id = @conversation)
		SELECT `+messageCols+` FROM msg`, map[string]any{
		"id": m.ID, "conversation": m.ConversationID, "side": string(m.SenderSide), "sender": m.SenderUserID, "body": m.Body,
		"at": m.CreatedAt,
	})
	switch {
	case err != nil:
		return Message{}, err
	case len(rows) == 0:
		return Message{}, ErrNotFound
	}
	return rows[0], nil
}

// MessageIn implements Store.
//
// cross-org: a message of a conversation the caller read through its scope.
func (s gormStore) MessageIn(ctx context.Context, conversationID, id uuid.UUID) (Message, error) {
	rows, err := s.messages(ctx, "message", `SELECT `+messageCols+` FROM conversation_messages
		WHERE id = @id AND conversation_id = @conversation`, map[string]any{"id": id, "conversation": conversationID})
	switch {
	case err != nil:
		return Message{}, err
	case len(rows) == 0:
		return Message{}, ErrNotFound
	}
	return rows[0], nil
}

// Messages implements Store.
//
// cross-org: the messages of a conversation the caller read through its
// scope.
func (s gormStore) Messages(ctx context.Context, conversationID uuid.UUID, before *uuid.UUID, limit int) ([]Message, error) {
	args := map[string]any{"conversation": conversationID, "limit": limit}
	where := ""
	if before != nil {
		where, args["before"] = ` AND id < @before`, *before
	}
	return s.messages(ctx, "messages", `SELECT `+messageCols+` FROM conversation_messages
		WHERE conversation_id = @conversation`+where+` ORDER BY id DESC LIMIT @limit`, args)
}

// CompanySenders implements Store.
//
// cross-org: the notice task of a conversation it read by ID.
func (s gormStore) CompanySenders(ctx context.Context, conversationID uuid.UUID) ([]uuid.UUID, error) {
	var out []uuid.UUID
	err := db.Conn(ctx, s.orm).Raw(`SELECT sender_user_id FROM (
		  SELECT DISTINCT ON (sender_user_id) sender_user_id, id FROM conversation_messages
		  WHERE conversation_id = @conversation AND sender_side = 'company' ORDER BY sender_user_id, id) first_messages
		ORDER BY id`,
		map[string]any{"conversation": conversationID}).Scan(&out).Error
	if err != nil {
		return nil, wrap("company senders", err)
	}
	return out, nil
}

const stateCols = `conversation_id, user_id, last_read_message_id, read_at, muted_at, updated_at`

// State implements Store.
//
// cross-org: the user's own state.
func (s gormStore) State(ctx context.Context, conversationID, userID uuid.UUID) (UserState, bool, error) {
	var out []UserState
	err := db.Conn(ctx, s.orm).Raw(`SELECT `+stateCols+` FROM conversation_user_states
		WHERE conversation_id = @conversation AND user_id = @user`,
		map[string]any{"conversation": conversationID, "user": userID}).Scan(&out).Error
	if err != nil {
		return UserState{}, false, wrap("state", err)
	}
	if len(out) == 0 {
		return UserState{}, false, nil
	}
	return out[0], true, nil
}

// MarkRead implements Store: the marker moves forward only (uuidv7 order).
//
// cross-org: the user's own state.
func (s gormStore) MarkRead(ctx context.Context, conversationID, userID, messageID uuid.UUID, at time.Time) (bool, error) {
	res := db.Conn(ctx, s.orm).Exec(`INSERT INTO conversation_user_states (conversation_id, user_id, last_read_message_id, read_at)
		VALUES ( @conversation, @user, @message, @at)
		ON CONFLICT (conversation_id, user_id) DO UPDATE
		  SET last_read_message_id = EXCLUDED.last_read_message_id, read_at = EXCLUDED.read_at
		  WHERE conversation_user_states.last_read_message_id IS NULL
		     OR conversation_user_states.last_read_message_id < EXCLUDED.last_read_message_id`,
		map[string]any{"conversation": conversationID, "user": userID, "message": messageID, "at": at})
	if res.Error != nil {
		return false, wrap("mark read", res.Error)
	}
	return res.RowsAffected == 1, nil
}

// SetMuted implements Store.
//
// cross-org: the user's own state.
func (s gormStore) SetMuted(ctx context.Context, conversationID, userID uuid.UUID, muted bool, at time.Time) (bool, error) {
	args := map[string]any{"conversation": conversationID, "user": userID, "at": at}
	sql := `UPDATE conversation_user_states SET muted_at = NULL
		WHERE conversation_id = @conversation AND user_id = @user AND muted_at IS NOT NULL`
	if muted {
		sql = `INSERT INTO conversation_user_states (conversation_id, user_id, muted_at) VALUES ( @conversation, @user, @at)
		ON CONFLICT (conversation_id, user_id) DO UPDATE SET muted_at = EXCLUDED.muted_at
		  WHERE conversation_user_states.muted_at IS NULL`
	}
	res := db.Conn(ctx, s.orm).Exec(sql, args)
	if res.Error != nil {
		return false, wrap("mute", res.Error)
	}
	return res.RowsAffected == 1, nil
}

// StudentConversations implements Store.
//
// cross-org: the data subject's conversations of every company (KVKK export).
func (s gormStore) StudentConversations(ctx context.Context, studentID uuid.UUID) ([]Conversation, error) {
	return s.conversations(ctx, "student conversations", `SELECT `+conversationCols+` FROM conversations
		WHERE student_user_id = @student ORDER BY id`, map[string]any{"student": studentID})
}

// AllMessages implements Store.
//
// cross-org: the messages of the data subject's conversation (KVKK export).
func (s gormStore) AllMessages(ctx context.Context, conversationID uuid.UUID) ([]Message, error) {
	return s.messages(ctx, "all messages", `SELECT `+messageCols+` FROM conversation_messages
		WHERE conversation_id = @conversation ORDER BY id`, map[string]any{"conversation": conversationID})
}

// MessagesSentBy implements Store.
//
// cross-org: the data subject's messages in any company's conversation
// (KVKK export).
func (s gormStore) MessagesSentBy(ctx context.Context, userID uuid.UUID) ([]Message, error) {
	return s.messages(ctx, "messages sent", `SELECT `+messageCols+` FROM conversation_messages
		WHERE sender_user_id = @user ORDER BY id`, map[string]any{"user": userID})
}

// UserStates implements Store.
//
// cross-org: the data subject's states (KVKK export).
func (s gormStore) UserStates(ctx context.Context, userID uuid.UUID) ([]UserState, error) {
	var out []UserState
	err := db.Conn(ctx, s.orm).Raw(`SELECT `+stateCols+` FROM conversation_user_states WHERE user_id = @user
		ORDER BY conversation_id`, map[string]any{"user": userID}).Scan(&out).Error
	if err != nil {
		return nil, wrap("user states", err)
	}
	return out, nil
}

// DeleteStudentConversations implements Store (messages and states go by
// ON DELETE CASCADE).
//
// cross-org: the data subject's conversations of every company (account
// erasure).
func (s gormStore) DeleteStudentConversations(ctx context.Context, studentID uuid.UUID) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM conversations WHERE student_user_id = @student`,
		map[string]any{"student": studentID})
	if res.Error != nil {
		return 0, wrap("delete student conversations", res.Error)
	}
	return int(res.RowsAffected), nil
}

// DeleteUserStates implements Store.
//
// cross-org: the data subject's states (account erasure).
func (s gormStore) DeleteUserStates(ctx context.Context, userID uuid.UUID) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM conversation_user_states WHERE user_id = @user`, map[string]any{"user": userID})
	if res.Error != nil {
		return 0, wrap("delete user states", res.Error)
	}
	return int(res.RowsAffected), nil
}

// CountInactiveBefore implements Store.
//
// cross-org: the retention sweep of every company (conversation_closed).
func (s gormStore) CountInactiveBefore(ctx context.Context, cutoff time.Time) (int, error) {
	var n int64
	err := db.Conn(ctx, s.orm).Raw(`SELECT count(*) FROM conversations WHERE last_activity_at < @cutoff`,
		map[string]any{"cutoff": cutoff}).Scan(&n).Error
	if err != nil {
		return 0, wrap("count inactive conversations", err)
	}
	return int(n), nil
}

// PurgeInactive implements Store.
//
// cross-org: the retention sweep of every company (conversation_closed).
func (s gormStore) PurgeInactive(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM conversations WHERE id IN (
		SELECT id FROM conversations WHERE last_activity_at < @cutoff
		ORDER BY last_activity_at, id LIMIT @limit FOR UPDATE SKIP LOCKED)`, map[string]any{"cutoff": cutoff, "limit": limit})
	if res.Error != nil {
		return 0, wrap("purge inactive conversations", res.Error)
	}
	return int(res.RowsAffected), nil
}
