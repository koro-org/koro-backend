//go:build integration

package migrations_test

import (
	"strings"
	"testing"
	"uuid"

	"koro/internal/platform/db"
	"koro/internal/platform/testutil"
)

// TestSchema_OrderLineDeliveryEvidence (BE-093, T-292, migration 000137):
// files accepts the delivery_evidence purpose; an order line carries the
// evidence file only once it was marked, refers to an existing file
// (which then cannot be deleted), never changes or drops it, and keeps it
// through the rest of the delivery. The down migration takes the column
// and the purpose away, the up one brings them back with the same rules.
func TestSchema_OrderLineDeliveryEvidence(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	check := func(t *testing.T) {
		t.Helper()
		s := w.shop()
		order, line := w.order(s)
		w.markPaid(order)
		ev, other := w.file("delivery_evidence"), w.file("delivery_evidence")
		const mark = `UPDATE order_lines SET fulfillment_status = 'marked', fulfillment_marked_at = now(),
			dispute_window_ends_at = now() + interval '7 days', fulfillment_evidence_file_id = $2 WHERE id = $1`
		expectFail(t, w.d.Pool, []p1aCase{
			{"evidence on a line that was never marked", `UPDATE order_lines SET fulfillment_evidence_file_id = $2 WHERE id = $1`,
				[]any{line, ev}, db.CodeCheckViolation, "order_lines_evidence_chk"},
			{"an unknown evidence file", mark, []any{line, uuid.NewV7()}, db.CodeForeignKeyViolation,
				"order_lines_fulfillment_evidence_file_id_fkey"},
			{"an unknown purpose", `INSERT INTO files (id, purpose, bucket, storage_key, declared_mime)
				VALUES ($1, 'delivery_proof', 'koro-files-dev', $2, 'application/pdf')`, []any{uuid.NewV7(), "x/" + w.n()},
				db.CodeCheckViolation, "files_purpose_chk"},
		})
		w.exec(mark, line, ev)
		expectFail(t, w.d.Pool, []p1aCase{
			{"the evidence is replaced", `UPDATE order_lines SET fulfillment_evidence_file_id = $2 WHERE id = $1`,
				[]any{line, other}, db.CodeRestrictViolation, ""},
			{"the evidence is dropped", `UPDATE order_lines SET fulfillment_evidence_file_id = NULL WHERE id = $1`,
				[]any{line}, db.CodeRestrictViolation, ""},
			{"the referenced file is deleted", `DELETE FROM files WHERE id = $1`, []any{ev}, db.CodeForeignKeyViolation,
				"order_lines_fulfillment_evidence_file_id_fkey"},
		})
		// The delivery moves on with its evidence (the buyer confirms).
		w.fulfil(line)
		if n := w.count(`SELECT count(*) FROM order_lines WHERE id = $1 AND fulfillment_status = 'fulfilled'
			AND fulfillment_evidence_file_id = $2`, line, ev); n != 1 {
			t.Fatalf("fulfilled line with its evidence = %d, want 1", n)
		}
		// A line without evidence is marked as before.
		order2, line2 := w.order(s)
		w.markPaid(order2)
		w.exec(`UPDATE order_lines SET fulfillment_status = 'marked', fulfillment_marked_at = now(),
			dispute_window_ends_at = now() + interval '7 days' WHERE id = $1`, line2)
	}
	check(t)
	if def := queryString(t, w.d, `SELECT indexdef FROM pg_indexes WHERE indexname = 'order_lines_evidence_idx'`); !strings.Contains(def,
		"(fulfillment_evidence_file_id) WHERE (fulfillment_evidence_file_id IS NOT NULL)") {
		t.Fatalf("order_lines_evidence_idx = %s", def)
	}
	owner := testutil.OwnerConn(t, w.d)
	if _, err := owner.Exec(t.Context(), mustRead(t, "000137_add_delivery_evidence.down.sql")); err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'order_lines'
		AND column_name = 'fulfillment_evidence_file_id'`); n != 0 {
		t.Fatal("down kept order_lines.fulfillment_evidence_file_id")
	}
	if n := w.count(`SELECT count(*) FROM files WHERE purpose = 'delivery_evidence'`); n != 0 {
		t.Fatalf("down kept %d delivery_evidence files", n)
	}
	if _, err := owner.Exec(t.Context(), mustRead(t, "000137_add_delivery_evidence.up.sql")); err != nil {
		t.Fatal(err)
	}
	check(t)
}
