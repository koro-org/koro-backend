//go:build integration

package migrations_test

import (
	"strings"
	"testing"
	"uuid"

	"koro/internal/platform/db"
)

// Tests of 000169 (BE-162, Q-40): the conversations of accepted contact
// requests.

// TestSchema_Conversations: a conversation belongs to a company (composite
// FK), names two different users and keeps one complete CHECK per status
// (06 H-37); a message is 1–2000 characters of a known side and is never
// changed by app_rw; a read marker comes with its time; a conversation's
// deletion takes its messages and states (CASCADE); the retention is the
// 12-month conversation_closed of messaging.
func TestSchema_Conversations(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	company, club := w.org("company"), w.org("club")
	student, member := w.user(), w.user()
	conv := uuid.NewV7()
	ins := `INSERT INTO conversations (id, org_id, student_user_id, company_user_id, status, closed_by, closed_at,
		company_unanswered, last_activity_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())`
	expectOK(t, w.d.Pool, []p1aCase{
		{name: "an open conversation", sql: ins, args: []any{conv, company, student, member, "open", nil, nil, 0}},
		{name: "a closed conversation", sql: ins, args: []any{uuid.NewV7(), company, student, member, "closed", "student", "2026-10-31T10:00:00Z", 2}},
	})
	chk := db.CodeCheckViolation
	expectFail(t, w.d.Pool, []p1aCase{
		{name: "a club", sql: ins, args: []any{uuid.NewV7(), club, student, member, "open", nil, nil, 0}, state: db.CodeForeignKeyViolation,
			constraint: "conversations_org_id_org_kind_fkey"},
		{name: "one user on both sides", sql: ins, args: []any{uuid.NewV7(), company, student, student, "open", nil, nil, 0}, state: chk,
			constraint: "conversations_parties_chk"},
		{name: "unknown status", sql: ins, args: []any{uuid.NewV7(), company, student, member, "archived", nil, nil, 0}, state: chk,
			constraint: "conversations_status_chk"},
		{name: "open with a closer", sql: ins, args: []any{uuid.NewV7(), company, student, member, "open", "company", nil, 0}, state: chk,
			constraint: "conversations_open_chk"},
		{name: "open with a closing time", sql: ins, args: []any{uuid.NewV7(), company, student, member, "open", nil, "2026-10-31T10:00:00Z", 0},
			state: chk, constraint: "conversations_open_chk"},
		{name: "closed without its time", sql: ins, args: []any{uuid.NewV7(), company, student, member, "closed", "student", nil, 0}, state: chk,
			constraint: "conversations_closed_chk"},
		{name: "closed without its closer", sql: ins, args: []any{uuid.NewV7(), company, student, member, "closed", nil, "2026-10-31T10:00:00Z", 0},
			state: chk, constraint: "conversations_closed_chk"},
		{name: "unknown closer", sql: ins, args: []any{uuid.NewV7(), company, student, member, "closed", "platform", "2026-10-31T10:00:00Z", 0},
			state: chk, constraint: "conversations_closed_by_chk"},
		{name: "negative unanswered", sql: ins, args: []any{uuid.NewV7(), company, student, member, "open", nil, nil, -1}, state: chk,
			constraint: "conversations_company_unanswered_chk"},
	})

	msg := `INSERT INTO conversation_messages (id, conversation_id, sender_side, sender_user_id, body) VALUES ($1, $2, $3, $4, $5)`
	m := uuid.NewV7()
	expectOK(t, w.d.Pool, []p1aCase{
		{name: "a message", sql: msg, args: []any{m, conv, "company", member, "Merhaba"}},
		{name: "the longest message", sql: msg, args: []any{uuid.NewV7(), conv, "student", student, strings.Repeat("ş", 2000)}},
		{name: "a read marker", sql: `INSERT INTO conversation_user_states (conversation_id, user_id, last_read_message_id, read_at)
			VALUES ($1, $2, $3, now())`, args: []any{conv, student, m}},
		{name: "a mute", sql: `INSERT INTO conversation_user_states (conversation_id, user_id, muted_at) VALUES ($1, $2, now())`,
			args: []any{conv, member}},
	})
	expectFail(t, w.d.Pool, []p1aCase{
		{name: "empty message", sql: msg, args: []any{uuid.NewV7(), conv, "student", student, ""}, state: chk,
			constraint: "conversation_messages_body_chk"},
		{name: "too long", sql: msg, args: []any{uuid.NewV7(), conv, "student", student, strings.Repeat("a", 2001)}, state: chk,
			constraint: "conversation_messages_body_chk"},
		{name: "unknown side", sql: msg, args: []any{uuid.NewV7(), conv, "platform", student, "x"}, state: chk,
			constraint: "conversation_messages_sender_side_chk"},
		{name: "unknown conversation", sql: msg, args: []any{uuid.NewV7(), uuid.NewV7(), "student", student, "x"},
			state: db.CodeForeignKeyViolation, constraint: "conversation_messages_conversation_id_fkey"},
		{name: "a message is never changed", sql: `UPDATE conversation_messages SET body = 'x' WHERE id = $1`, args: []any{m},
			state: db.CodeInsufficientPriv},
		{name: "a marker without its time", sql: `INSERT INTO conversation_user_states (conversation_id, user_id, last_read_message_id)
			VALUES ($1, $2, $3)`, args: []any{conv, w.user(), m}, state: chk, constraint: "conversation_user_states_read_chk"},
	})
	expectOK(t, w.d.Pool, []p1aCase{{name: "erase the conversation", sql: `DELETE FROM conversations WHERE id = $1`, args: []any{conv}}})
	if n := w.count(`SELECT count(*) FROM conversation_messages WHERE conversation_id = $1`, conv) +
		w.count(`SELECT count(*) FROM conversation_user_states WHERE conversation_id = $1`, conv); n != 0 {
		t.Fatalf("%d messages and states outlived their conversation", n)
	}
	if got := queryString(t, w.d, `SELECT retention::text || ' ' || anchor || ' ' || method || ' ' || target || ' ' || owner_module || ' ' ||
		legal_approved::text || ' ' || enabled::text FROM retention_policies WHERE code = 'conversation_closed'`); got !=
		"1 year closed_at hard_delete conversations messaging false true" {
		t.Fatalf("conversation_closed policy = %s", got)
	}
}
