//go:build integration

package migrations_test

import (
	"strings"
	"testing"
	"uuid"

	"koro/internal/platform/db"
	"koro/internal/platform/testutil"
)

// TestSchema_MessageReports (BE-162b, Q-43, migration 000174): a content
// report may target a conversation message, and its kept messages live in
// message_reports + message_report_messages:
//   - the copy points at a message report of its own message only
//     (composite FK to content_reports (id, target_type, target_id));
//   - the CHECKs: a known side, a body of 1–2000 characters;
//   - app_rw never changes a kept row (no UPDATE);
//   - the copy goes with its report (CASCADE) and outlives the conversation
//     (no FK to it);
//   - the down migration drops the tables and the message reports and
//     restores the target CHECK, the up one brings them back.
func TestSchema_MessageReports(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	check := func(t *testing.T) {
		t.Helper()
		company, student, member := w.org("company"), w.user(), w.user()
		conv, msg := uuid.NewV7(), uuid.NewV7()
		w.exec(`INSERT INTO conversations (id, org_id, student_user_id, company_user_id, last_activity_at) VALUES ($1, $2, $3, $4, now())`,
			conv, company, student, member)
		w.exec(`INSERT INTO conversation_messages (id, conversation_id, sender_side, sender_user_id, body) VALUES ($1, $2, 'company', $3, 'Israr')`,
			msg, conv, member)
		const report = `INSERT INTO content_reports (id, reporter_user_id, target_type, target_id, reason) VALUES ($1, $2, $3, $4, 'harassment')`
		rep, postReport, otherRep := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
		w.exec(report, rep, student, "message", msg)
		w.exec(report, postReport, student, "post", msg)
		// A message report of another message, still without its copy.
		w.exec(report, otherRep, student, "message", uuid.NewV7())
		const ctxRow = `INSERT INTO message_reports (report_id, message_id, conversation_id, org_id, student_user_id, reported_side, taken_at)
			VALUES ($1, $2, $3, $4, $5, $6, now())`
		const kept = `INSERT INTO message_report_messages (report_id, message_id, sender_side, sender_user_id, body, sent_at)
			VALUES ($1, $2, $3, $4, $5, now())`
		expectOK(t, w.d.Pool, []p1aCase{
			{name: "the copy of a message report", sql: ctxRow, args: []any{rep, msg, conv, company, student, "company"}},
			{name: "the reported message", sql: kept, args: []any{rep, msg, "company", member, "Israr"}},
			{name: "the longest message before it", sql: kept, args: []any{rep, uuid.NewV7(), "student", student, strings.Repeat("ş", 2000)}},
		})
		fk, chk := db.CodeForeignKeyViolation, db.CodeCheckViolation
		expectFail(t, w.d.Pool, []p1aCase{
			{name: "a post report", sql: ctxRow, args: []any{postReport, msg, conv, company, student, "company"}, state: fk,
				constraint: "message_reports_report_fkey"},
			{name: "another message than the report's", sql: ctxRow, args: []any{otherRep, msg, conv, company, student, "company"},
				state: fk, constraint: "message_reports_report_fkey"},
			{name: "an unknown report", sql: ctxRow, args: []any{uuid.NewV7(), msg, conv, company, student, "company"}, state: fk,
				constraint: "message_reports_report_fkey"},
			{name: "an unknown side", sql: ctxRow, args: []any{rep, msg, conv, company, student, "platform"}, state: chk,
				constraint: "message_reports_reported_side_chk"},
			{name: "an empty message", sql: kept, args: []any{rep, uuid.NewV7(), "student", student, ""}, state: chk,
				constraint: "message_report_messages_body_chk"},
			{name: "a message of 2001 characters", sql: kept, args: []any{rep, uuid.NewV7(), "student", student, strings.Repeat("a", 2001)},
				state: chk, constraint: "message_report_messages_body_chk"},
			{name: "a message of no side", sql: kept, args: []any{rep, uuid.NewV7(), "platform", student, "x"}, state: chk,
				constraint: "message_report_messages_sender_side_chk"},
			{name: "a message without its report's copy", sql: kept, args: []any{postReport, uuid.NewV7(), "student", student, "x"},
				state: fk, constraint: "message_report_messages_report_id_fkey"},
			{name: "an unknown target type", sql: report, args: []any{uuid.NewV7(), student, "conversation", conv}, state: chk,
				constraint: "content_reports_target_type_chk"},
			{name: "a kept message is never changed", sql: `UPDATE message_report_messages SET body = 'x' WHERE report_id = $1`,
				args: []any{rep}, state: db.CodeInsufficientPriv},
			{name: "a copy is never changed", sql: `UPDATE message_reports SET reported_side = 'student' WHERE report_id = $1`,
				args: []any{rep}, state: db.CodeInsufficientPriv},
		})
		// The conversation goes (conversation_closed, the student's erasure): the copy stays.
		w.exec(`DELETE FROM conversations WHERE id = $1`, conv)
		if n := w.count(`SELECT count(*) FROM message_report_messages WHERE report_id = $1`, rep); n != 2 {
			t.Fatalf("kept messages after the conversation's deletion = %d, want 2", n)
		}
		// The report goes (content_report retention, the reporter's erasure): the copy with it.
		w.exec(`DELETE FROM content_reports WHERE id = $1`, rep)
		if n := w.count(`SELECT count(*) FROM message_reports WHERE report_id = $1`, rep) +
			w.count(`SELECT count(*) FROM message_report_messages WHERE report_id = $1`, rep); n != 0 {
			t.Fatalf("%d rows outlived their report", n)
		}
		// A message report waits for the next run of the check (the down migration deletes it).
		w.exec(report, uuid.NewV7(), student, "message", uuid.NewV7())
	}
	check(t)

	owner := testutil.OwnerConn(t, w.d)
	if _, err := owner.Exec(t.Context(), mustRead(t, "000174_create_message_reports.down.sql")); err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT count(*) FROM information_schema.tables WHERE table_name IN ('message_reports', 'message_report_messages')`) +
		w.count(`SELECT count(*) FROM content_reports WHERE target_type = 'message'`) +
		w.count(`SELECT count(*) FROM pg_constraint WHERE conname = 'content_reports_id_target_uq'`); n != 0 {
		t.Fatalf("down left %d objects of 000174", n)
	}
	expectFail(t, w.d.Pool, []p1aCase{{name: "a message report after the down migration",
		sql:  `INSERT INTO content_reports (id, reporter_user_id, target_type, target_id, reason) VALUES ($1, $2, 'message', $3, 'spam')`,
		args: []any{uuid.NewV7(), w.user(), uuid.NewV7()}, state: db.CodeCheckViolation, constraint: "content_reports_target_type_chk"}})
	if _, err := owner.Exec(t.Context(), mustRead(t, "000174_create_message_reports.up.sql")); err != nil {
		t.Fatal(err)
	}
	check(t)
}
