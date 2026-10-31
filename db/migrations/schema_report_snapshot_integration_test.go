//go:build integration

package migrations_test

import (
	"testing"
	"uuid"

	"koro/internal/platform/db"
	"koro/internal/platform/testutil"
)

// TestSchema_ReportSnapshot (BE-160, migration 000168): content_reports
// keeps the reported post's title, plain text, opportunity and event with
// a post report:
//   - the CHECKs: a snapshot is a post report's only, its columns need
//     snapshot_at, and the limits are the posts' (title 140, text 5000
//     characters, the opportunity an object); a post report without a
//     snapshot (filed before BE-160) and an org report stay valid;
//   - content_reports_snapshot_guard: the snapshot is written with the row
//     only — any later change is refused (restrict_violation), an UPDATE
//     of the status or one that sets a column to its own value passes;
//   - the content_report policy text names the copy;
//   - the down migration drops all of it and restores the text, the up one
//     brings it back.
func TestSchema_ReportSnapshot(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	check := func(t *testing.T) {
		t.Helper()
		club, author := w.org("club"), w.user()
		post := w.post(club, "club", author, nil)
		const ins = `INSERT INTO content_reports (id, reporter_user_id, target_type, target_id, reason,
			snapshot_title, snapshot_body_text, snapshot_opportunity, snapshot_event_id, snapshot_at) VALUES ($1, $2, $3, $4, 'scam_or_misleading_job', $5, $6, $7, $8, $9)`
		report := uuid.NewV7()
		w.exec(ins, report, w.user(), "post", post, "Ücretli staj", "Başvuru ücreti 500 TL",
			`{"type": "internship", "apply_via": "external_url", "external_url": "https://a.koro.test"}`, uuid.NewV7(), "2026-10-31T12:00:00Z")
		// A post without text: only the time. No snapshot: before BE-160, or an org report.
		w.exec(ins, uuid.NewV7(), w.user(), "post", post, nil, nil, nil, nil, "2026-10-31T12:00:00Z")
		w.exec(ins, uuid.NewV7(), w.user(), "post", post, nil, nil, nil, nil, nil)
		w.exec(ins, uuid.NewV7(), w.user(), "org", club, nil, nil, nil, nil, nil)
		frozen := func(name, sql string) p1aCase {
			return p1aCase{name, sql, []any{report}, db.CodeRestrictViolation, "content_reports_snapshot_frozen"}
		}
		bad := func(name string, target string, title, body, opportunity, event, at any, constraint string) p1aCase {
			id := post
			if target == "org" {
				id = club
			}
			return p1aCase{name, ins, []any{uuid.NewV7(), w.user(), target, id, title, body, opportunity, event, at}, db.CodeCheckViolation, constraint}
		}
		const at = "2026-10-31T12:00:00Z"
		expectFail(t, w.d.Pool, []p1aCase{
			bad("an org report with a snapshot", "org", nil, nil, nil, nil, at, "content_reports_snapshot_target_chk"),
			bad("a title without its time", "post", "Başlık", nil, nil, nil, nil, "content_reports_snapshot_at_chk"),
			bad("a text without its time", "post", nil, "Metin", nil, nil, nil, "content_reports_snapshot_at_chk"),
			bad("an opportunity without its time", "post", nil, nil, `{"type": "job"}`, nil, nil, "content_reports_snapshot_at_chk"),
			bad("an event without its time", "post", nil, nil, nil, uuid.NewV7(), nil, "content_reports_snapshot_at_chk"),
			bad("a title of 141 characters", "post", repeat("ş", 141), nil, nil, nil, at, "content_reports_snapshot_title_chk"),
			bad("a text of 5001 characters", "post", nil, repeat("ş", 5001), nil, nil, at, "content_reports_snapshot_body_text_chk"),
			bad("an opportunity that is no object", "post", nil, nil, `[1]`, nil, at, "content_reports_snapshot_opportunity_chk"),
			frozen("a new title", `UPDATE content_reports SET snapshot_title = 'Staj' WHERE id = $1`),
			frozen("a cleared text", `UPDATE content_reports SET snapshot_body_text = NULL WHERE id = $1`),
			frozen("a new opportunity", `UPDATE content_reports SET snapshot_opportunity = '{"type": "job"}' WHERE id = $1`),
			frozen("another event", `UPDATE content_reports SET snapshot_event_id = uuidv7() WHERE id = $1`),
			frozen("a new time", `UPDATE content_reports SET snapshot_at = now() WHERE id = $1`),
			frozen("the whole snapshot cleared", `UPDATE content_reports SET snapshot_title = NULL, snapshot_body_text = NULL,
				snapshot_opportunity = NULL, snapshot_event_id = NULL, snapshot_at = NULL WHERE id = $1`),
		})
		// The moderation flow moves the row; an unchanged value passes.
		w.exec(`UPDATE content_reports SET status = 'in_review' WHERE id = $1`, report)
		w.exec(`UPDATE content_reports SET snapshot_title = snapshot_title, status = 'dismissed', resolved_at = now() WHERE id = $1`, report)
		if n := w.count(`SELECT count(*) FROM content_reports WHERE id = $1 AND status = 'dismissed' AND snapshot_title = 'Ücretli staj'
			AND snapshot_body_text = 'Başvuru ücreti 500 TL' AND snapshot_opportunity->>'external_url' = 'https://a.koro.test'
			AND snapshot_event_id IS NOT NULL AND snapshot_at = '2026-10-31T12:00:00Z'`, report); n != 1 {
			t.Fatal("the snapshot changed with the moderation flow")
		}
		// The report and its snapshot go together (retention, erasure).
		w.exec(`DELETE FROM content_reports WHERE id = $1`, report)
		if n := w.count(`SELECT count(*) FROM retention_policies WHERE code = 'content_report' AND description_tr LIKE '%gönderi metniyle%'`); n != 1 {
			t.Fatal("the content_report policy does not name the snapshot")
		}
	}
	check(t)

	owner := testutil.OwnerConn(t, w.d)
	if _, err := owner.Exec(t.Context(), mustRead(t, "000168_snapshot_reported_posts.down.sql")); err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'content_reports' AND column_name LIKE 'snapshot\_%'`) +
		w.count(`SELECT count(*) FROM pg_proc WHERE proname = 'content_reports_snapshot_guard'`) +
		w.count(`SELECT count(*) FROM pg_constraint WHERE conname LIKE 'content_reports_snapshot%'`); n != 0 {
		t.Fatalf("down left %d objects of 000168", n)
	}
	if n := w.count(`SELECT count(*) FROM retention_policies WHERE code = 'content_report' AND description_tr = 'Çözülmüş içerik şikâyetleri silinir.'`); n != 1 {
		t.Fatal("down did not restore the content_report policy text")
	}
	if _, err := owner.Exec(t.Context(), mustRead(t, "000168_snapshot_reported_posts.up.sql")); err != nil {
		t.Fatal(err)
	}
	check(t)
}
