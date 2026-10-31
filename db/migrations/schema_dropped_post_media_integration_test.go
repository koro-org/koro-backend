//go:build integration

package migrations_test

import (
	"testing"
	"uuid"

	"koro/internal/platform/db"
	"koro/internal/platform/testutil"
)

// TestSchema_DroppedPostMediaAndOpenEvidence (BE-158, migration 000166):
//   - post_dropped_media keeps an image an edit took off a published post:
//     a file is dropped once, its file row cannot go while the row holds it
//     (feed deletes the row first), the row goes with its post, and the
//     CHECKs mirror post_media's;
//   - order_lines_evidence_open_guard lets a rejected delivery evidence go
//     for another file only on an open line (marked or disputed, the status
//     unchanged, not paid out) and only stamping
//     fulfillment_evidence_replaced_at, which moves with a replacement only;
//   - the down migration drops both (and the policy), the up one restores
//     them.
func TestSchema_DroppedPostMediaAndOpenEvidence(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	check := func(t *testing.T) {
		t.Helper()
		// feed: the dropped images.
		club, author := w.org("club"), w.user()
		post := w.post(club, "club", author, nil)
		file := w.file("post_media")
		const drop = `INSERT INTO post_dropped_media (file_id, post_id, alt_text, width, height, dropped_at) VALUES ($1, $2, $3, $4, $5, now())`
		w.exec(drop, file, post, "Afiş", 1080, 1350)
		expectFail(t, w.d.Pool, []p1aCase{
			{"the same file twice", drop, []any{file, w.post(club, "club", author, nil), nil, nil, nil}, db.CodeUniqueViolation, "post_dropped_media_pkey"},
			{"unknown file", drop, []any{uuid.NewV7(), post, nil, nil, nil}, db.CodeForeignKeyViolation, "post_dropped_media_file_id_fkey"},
			{"unknown post", drop, []any{w.file("post_media"), uuid.NewV7(), nil, nil, nil}, db.CodeForeignKeyViolation, "post_dropped_media_post_id_fkey"},
			{"alt text of 301 characters", drop, []any{w.file("post_media"), post, repeat("a", 301), nil, nil}, db.CodeCheckViolation, "post_dropped_media_alt_text_chk"},
			{"zero width", drop, []any{w.file("post_media"), post, nil, 0, nil}, db.CodeCheckViolation, "post_dropped_media_width_chk"},
			{"zero height", drop, []any{w.file("post_media"), post, nil, nil, 0}, db.CodeCheckViolation, "post_dropped_media_height_chk"},
			{"the file before its row", `DELETE FROM files WHERE id = $1`, []any{file}, db.CodeForeignKeyViolation, "post_dropped_media_file_id_fkey"},
		})
		w.exec(`DELETE FROM posts WHERE id = $1`, post)
		if n := w.count(`SELECT count(*) FROM post_dropped_media WHERE file_id = $1`, file); n != 0 {
			t.Fatal("a dropped image outlived its post")
		}
		w.exec(`DELETE FROM files WHERE id = $1`, file)

		// orders: the delivery evidence is replaced on an open line only.
		const rejectSQL = `UPDATE files SET scan_status = 'infected' WHERE id = $1`
		marked := func(status string) (line, ev uuid.UUID) {
			t.Helper()
			order, l := w.order(w.shop())
			w.markPaid(order)
			ev = w.file("delivery_evidence")
			w.exec(`UPDATE order_lines SET fulfillment_status = 'marked', fulfillment_marked_at = now(),
				dispute_window_ends_at = now() + interval '7 days', fulfillment_evidence_file_id = $2 WHERE id = $1`, l, ev)
			if status != "marked" {
				w.exec(`UPDATE order_lines SET fulfillment_status = $2, fulfilled_at = CASE WHEN $2 = 'fulfilled' THEN now() END,
					fulfilled_via = CASE WHEN $2 = 'fulfilled' THEN 'buyer_confirmed' END,
					dispute_reason = CASE WHEN $2 = 'disputed' THEN 'Eksik' END WHERE id = $1`, l, status)
			}
			w.exec(rejectSQL, ev)
			return l, ev
		}
		const replace = `UPDATE order_lines SET fulfillment_evidence_file_id = $2, fulfillment_evidence_replaced_at = clock_timestamp() WHERE id = $1`
		const unstamped = `UPDATE order_lines SET fulfillment_evidence_file_id = $2 WHERE id = $1`
		open := func(name, sql string, args ...any) p1aCase {
			return p1aCase{name, sql, args, db.CodeRestrictViolation, "order_lines_evidence_open"}
		}
		line, _ := marked("marked")
		disputed, _ := marked("disputed")
		fulfilled, _ := marked("fulfilled")
		expectFail(t, w.d.Pool, []p1aCase{
			open("a replacement without its time", unstamped, line, w.file("delivery_evidence")),
			open("the time without a replacement", `UPDATE order_lines SET fulfillment_evidence_replaced_at = now() WHERE id = $1`, line),
			open("a fulfilled line's evidence", replace, fulfilled, w.file("delivery_evidence")),
			open("a replacement with a status change",
				`UPDATE order_lines SET fulfillment_evidence_file_id = $2, fulfillment_evidence_replaced_at = now(),
					fulfillment_status = 'fulfilled', fulfilled_at = now(), fulfilled_via = 'buyer_confirmed' WHERE id = $1`,
				line, w.file("delivery_evidence")),
		})
		w.exec(replace, line, w.file("delivery_evidence"))
		w.exec(replace, disputed, w.file("delivery_evidence"))
		if n := w.count(`SELECT count(*) FROM order_lines WHERE id IN ($1, $2) AND fulfillment_evidence_replaced_at IS NOT NULL`, line, disputed); n != 2 {
			t.Fatalf("stamped replacements = %d, want 2", n)
		}
		// The line moves on with its new evidence; the stamp stays.
		w.exec(`UPDATE order_lines SET fulfillment_status = 'fulfilled', fulfilled_at = now(), fulfilled_via = 'buyer_confirmed',
			fulfillment_evidence_file_id = COALESCE(fulfillment_evidence_file_id, NULL) WHERE id = $1`, line)
		if n := w.count(`SELECT count(*) FROM retention_policies WHERE code = 'post_media_dropped' AND target = 'post_dropped_media'`); n != 1 {
			t.Fatal("no post_media_dropped retention policy")
		}
	}
	check(t)

	owner := testutil.OwnerConn(t, w.d)
	if _, err := owner.Exec(t.Context(), mustRead(t, "000166_keep_dropped_post_media_and_open_evidence.down.sql")); err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT count(*) FROM pg_class WHERE relname = 'post_dropped_media'`) +
		w.count(`SELECT count(*) FROM pg_proc WHERE proname = 'order_lines_evidence_open_guard'`) +
		w.count(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'order_lines' AND column_name = 'fulfillment_evidence_replaced_at'`) +
		w.count(`SELECT count(*) FROM retention_policies WHERE code = 'post_media_dropped'`); n != 0 {
		t.Fatalf("down left %d objects of 000166", n)
	}
	if _, err := owner.Exec(t.Context(), mustRead(t, "000166_keep_dropped_post_media_and_open_evidence.up.sql")); err != nil {
		t.Fatal(err)
	}
	check(t)
}
