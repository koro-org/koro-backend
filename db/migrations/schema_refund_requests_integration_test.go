//go:build integration

package migrations_test

import (
	"testing"
	"uuid"

	"koro/internal/platform/db"
	"koro/internal/platform/testutil"
)

// refundRequestSQL inserts a waiting request of the line of the order:
// $1 id, $2 order, $3 line, $4 buyer org, $5 seller org, $6 order no,
// $7 requester, $8 target IBAN.
const refundRequestSQL = `INSERT INTO refund_requests (id, order_id, order_line_id, org_id, seller_org_id, order_no, line_title, amount,
	reason, explanation, target_iban, requested_by)
	VALUES ($1, $2, $3, $4, $5, $6, 'Stant', 100000, 'not_fulfilled', 'Stant kurulmadı, alan boştu.', $8, $7)`

// TestSchema_RefundRequests (BE-132, Q-26, migration 000156): files
// accepts the refund_request_evidence purpose; a request names its order's
// buyer, its line's seller and the order's own line (composite FKs, 06
// H-38), a TR IBAN and an explanation of 10–2000 characters, starts open,
// and has one waiting request per line; one CHECK per state (06 H-37), the
// requester never reviews or decides, the request never changes and a
// decision or a withdrawal is final; app_rw never deletes; the request
// goes with its order in the financial records purge (ON DELETE CASCADE).
// The down migration takes the table, the order_lines key and the purpose
// away, the up one brings them back with the same rules.
func TestSchema_RefundRequests(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	check := func(t *testing.T) {
		t.Helper()
		s := w.shop()
		order, line := w.payableLine(s)
		no := queryString(t, w.d, `SELECT order_no FROM orders WHERE id = $1`, order)
		other := w.shop()
		_, otherLine := w.order(other)
		iban := w.iban()
		insert := func(id, ord, ln, buyer, seller uuid.UUID) []any {
			return []any{id, ord, ln, buyer, seller, no, s.buyer.user, iban}
		}
		expectFail(t, w.d.Pool, []p1aCase{
			{"another org as the buyer", refundRequestSQL, insert(uuid.NewV7(), order, line, other.buyer.org, s.seller.org),
				db.CodeForeignKeyViolation, "refund_requests_order_id_org_id_fkey"},
			{"another org as the seller", refundRequestSQL, insert(uuid.NewV7(), order, line, s.buyer.org, other.seller.org),
				db.CodeForeignKeyViolation, "refund_requests_order_line_id_seller_org_id_fkey"},
			{"another order's line", refundRequestSQL, insert(uuid.NewV7(), order, otherLine, s.buyer.org, other.seller.org),
				db.CodeForeignKeyViolation, "refund_requests_order_line_id_order_id_fkey"},
			{"not a TR IBAN", refundRequestSQL, []any{uuid.NewV7(), order, line, s.buyer.org, s.seller.org, no, s.buyer.user,
				"DE89370400440532013000"}, db.CodeCheckViolation, "refund_requests_target_iban_chk"},
			{"an unknown purpose", `INSERT INTO files (id, purpose, bucket, storage_key, declared_mime)
				VALUES ($1, 'refund_proof', 'koro-files-dev', $2, 'application/pdf')`, []any{uuid.NewV7(), "x/" + w.n()},
				db.CodeCheckViolation, "files_purpose_chk"},
			{"a short explanation", `INSERT INTO refund_requests (id, order_id, order_line_id, org_id, seller_org_id, order_no, line_title,
				amount, reason, explanation, target_iban, requested_by) VALUES ($1, $2, $3, $4, $5, $6, 'Stant', 1, 'other', 'kısa', $7, $8)`,
				[]any{uuid.NewV7(), order, line, s.buyer.org, s.seller.org, no, iban, s.buyer.user},
				db.CodeCheckViolation, "refund_requests_explanation_chk"},
			{"a request that starts accepted", `INSERT INTO refund_requests (id, order_id, order_line_id, org_id, seller_org_id, order_no,
				line_title, amount, reason, explanation, target_iban, requested_by, status) VALUES ($1, $2, $3, $4, $5, $6, 'Stant', 1,
				'other', 'Stant kurulmadı, alan boştu.', $7, $8, 'accepted')`,
				[]any{uuid.NewV7(), order, line, s.buyer.org, s.seller.org, no, iban, s.buyer.user},
				db.CodeCheckViolation, "refund_requests_initial_status_chk"},
		})
		ev := w.file("refund_request_evidence")
		req := uuid.NewV7()
		w.exec(refundRequestSQL, insert(req, order, line, s.buyer.org, s.seller.org)...)
		staff := w.user()
		expectFail(t, w.d.Pool, []p1aCase{
			{"a second waiting request of the line", refundRequestSQL, insert(uuid.NewV7(), order, line, s.buyer.org, s.seller.org),
				db.CodeUniqueViolation, "refund_requests_line_pending_uq"},
			{"in review without its reviewer", `UPDATE refund_requests SET status = 'in_review' WHERE id = $1`, []any{req},
				db.CodeCheckViolation, "refund_requests_reviewed_chk"},
			{"reviewed by its requester", `UPDATE refund_requests SET status = 'in_review', reviewed_by = requested_by, reviewed_at = now()
				WHERE id = $1`, []any{req}, db.CodeCheckViolation, "refund_requests_review_eyes_chk"},
			{"decided by its requester", `UPDATE refund_requests SET status = 'rejected', decided_by = requested_by, decided_at = now(),
				rejection_reason = 'x' WHERE id = $1`, []any{req}, db.CodeCheckViolation, "refund_requests_decision_eyes_chk"},
			{"accepted without its refund", `UPDATE refund_requests SET status = 'accepted', decided_by = $2, decided_at = now()
				WHERE id = $1`, []any{req, staff}, db.CodeCheckViolation, "refund_requests_accepted_chk"},
			{"rejected without its reason", `UPDATE refund_requests SET status = 'rejected', decided_by = $2, decided_at = now()
				WHERE id = $1`, []any{req, staff}, db.CodeCheckViolation, "refund_requests_rejected_chk"},
			{"withdrawn without when", `UPDATE refund_requests SET status = 'withdrawn', withdrawn_by = requested_by WHERE id = $1`,
				[]any{req}, db.CodeCheckViolation, "refund_requests_withdrawn_chk"},
			{"a decision without its state", `UPDATE refund_requests SET decided_by = $2, decided_at = now() WHERE id = $1`,
				[]any{req, staff}, db.CodeCheckViolation, "refund_requests_decided_chk"},
			{"the amount changes", `UPDATE refund_requests SET amount = 1 WHERE id = $1`, []any{req}, db.CodeRestrictViolation, ""},
			{"the evidence is set later", `UPDATE refund_requests SET evidence_file_id = $2 WHERE id = $1`, []any{req, ev},
				db.CodeRestrictViolation, ""},
			{"app_rw deletes a request", `DELETE FROM refund_requests WHERE id = $1`, []any{req}, db.CodeInsufficientPriv, ""},
		})
		w.exec(`UPDATE refund_requests SET status = 'in_review', reviewed_by = $2, reviewed_at = now() WHERE id = $1`, req, staff)
		w.exec(`UPDATE refund_requests SET status = 'rejected', decided_by = $2, decided_at = now(), rejection_reason = 'Kanıt yok'
			WHERE id = $1`, req, staff)
		expectFail(t, w.d.Pool, []p1aCase{
			{"a rejected request is withdrawn", `UPDATE refund_requests SET status = 'withdrawn', withdrawn_by = requested_by,
				withdrawn_at = now(), decided_by = NULL, decided_at = NULL, rejection_reason = NULL WHERE id = $1`, []any{req},
				db.CodeRestrictViolation, ""},
			{"the review is undone", `UPDATE refund_requests SET reviewed_by = NULL, reviewed_at = NULL WHERE id = $1`, []any{req},
				db.CodeRestrictViolation, ""},
		})
		// The line is free again; a request with evidence is withdrawn, and final.
		req2 := uuid.NewV7()
		w.exec(`INSERT INTO refund_requests (id, order_id, order_line_id, org_id, seller_org_id, order_no, line_title, amount, reason,
			explanation, target_iban, requested_by, evidence_file_id)
			VALUES ($1, $2, $3, $4, $5, $6, 'Stant', 1, 'other', 'Stant kurulmadı, alan boştu.', $7, $8, $9)`,
			req2, order, line, s.buyer.org, s.seller.org, no, iban, s.buyer.user, ev)
		w.exec(`UPDATE refund_requests SET status = 'withdrawn', withdrawn_by = requested_by, withdrawn_at = now() WHERE id = $1`, req2)
		expectFail(t, w.d.Pool, []p1aCase{
			{"a withdrawn request reopens", `UPDATE refund_requests SET status = 'open', withdrawn_by = NULL, withdrawn_at = NULL
				WHERE id = $1`, []any{req2}, db.CodeRestrictViolation, ""},
			{"the evidence file is deleted", `DELETE FROM files WHERE id = $1`, []any{ev}, db.CodeForeignKeyViolation,
				"refund_requests_evidence_file_id_fkey"},
		})
	}
	check(t)

	// The financial records purge takes a request with its order (CASCADE).
	owner := testutil.OwnerConn(t, w.d)
	s := w.shop()
	order, line, _ := w.settledOrder(s)
	no := queryString(t, w.d, `SELECT order_no FROM orders WHERE id = $1`, order)
	req := uuid.NewV7()
	w.exec(refundRequestSQL, req, order, line, s.buyer.org, s.seller.org, no, s.buyer.user, w.iban())
	ageAll(t, owner, "12 years")
	w.approve("financial_records")
	for range 5 {
		n, state := w.purgeAt("financial_records_purge", 100, false)
		if state != "" {
			t.Fatalf("purge: SQLSTATE %s", state)
		}
		if n == 0 {
			break
		}
	}
	if n := w.count(`SELECT count(*) FROM orders WHERE id = $1`, order); n != 0 {
		t.Fatal("the settled order was not purged")
	}
	if n := w.count(`SELECT count(*) FROM refund_requests WHERE id = $1`, req); n != 0 {
		t.Fatal("the request outlived its order")
	}

	if _, err := owner.Exec(t.Context(), mustRead(t, "000156_create_refund_requests.down.sql")); err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT count(*) FROM information_schema.tables WHERE table_name = 'refund_requests'`); n != 0 {
		t.Fatal("down kept refund_requests")
	}
	if n := w.count(`SELECT count(*) FROM files WHERE purpose = 'refund_request_evidence'`); n != 0 {
		t.Fatalf("down kept %d refund_request_evidence files", n)
	}
	if n := w.count(`SELECT count(*) FROM pg_constraint WHERE conname = 'order_lines_id_seller_uq'`); n != 0 {
		t.Fatal("down kept order_lines_id_seller_uq")
	}
	if _, err := owner.Exec(t.Context(), mustRead(t, "000156_create_refund_requests.up.sql")); err != nil {
		t.Fatal(err)
	}
	check(t)
}
