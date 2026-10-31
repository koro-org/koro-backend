//go:build integration

package payments_test

import (
	"errors"
	"testing"
	"uuid"

	"koro/internal/files"
	"koro/internal/payments"
	"koro/internal/platform/db"
)

// pgReject marks an upload infected in the database as well (the guard of
// 000163 reads the files row).
func pgReject(t *testing.T, e *env, d *db.DB, fid uuid.UUID) {
	t.Helper()
	e.reject(fid)
	pgExec(t, d, `UPDATE files SET scan_status = 'infected' WHERE id = $1`, fid)
}

// TestReplaceEvidence_PostgresGuardAndCascade (BE-154b): on PostgreSQL a
// transfer's rejected receipt is replaced in one statement that its
// collection follows (receivable_collections_source_fkey ON UPDATE
// CASCADE, both guards see the rejected file); a clean receipt stays
// (koro_evidence_file_guard refuses even the store's UPDATE); a refund's
// receipt is replaced only once completed.
func TestReplaceEvidence_PostgresGuardAndCascade(t *testing.T) {
	t.Parallel()
	w := &collectionLine{}
	e, d := newPGEnv(t, withOrders(func(o payments.Orders) payments.Orders { w.Orders = o; return w }))
	_, acct := collectionDebt(t, e, w)
	fin := actor(t, "finance_admin")
	tr := e.collectionSource(t, acct, 540_000)
	c, err := e.svc.CreateCollection(ctxOf(t, fin), fin, payments.CollectionInput{IncomingTransferID: tr.ID, OrderLineID: w.line})
	if err != nil {
		t.Fatal(err)
	}
	old := *tr.ReceiptFileID
	if err := e.store.ReplaceEvidence(t.Context(), payments.SlotTransferReceipt, tr.ID, old, e.files.add(files.PurposeTransferReceipt)); err == nil {
		t.Fatal("the guard let a clean receipt go")
	}
	pgReject(t, e, d, old)
	next := e.files.add(files.PurposeTransferReceipt)
	got, err := e.svc.AdminReplaceTransferReceipt(ctxOf(t, fin), fin, tr.ID, next)
	if err != nil || *got.ReceiptFileID != next {
		t.Fatalf("replace = %+v, %v", got, err)
	}
	if n := pgCount(t, d, `SELECT count(*) FROM receivable_collections WHERE id = $1 AND receipt_file_id = $2`, c.ID, next); n != 1 {
		t.Fatal("the collection did not follow the transfer's receipt")
	}
	if _, err := e.svc.AdminReplaceTransferReceipt(ctxOf(t, fin), fin, tr.ID,
		e.files.add(files.PurposeTransferReceipt)); !errors.Is(err, files.ErrNotRejected) {
		t.Fatalf("the replacement is not rejected: %v", err)
	}

	po := e.paid(t, acct, orderSpec{}, 0)
	r := e.approved(t, lineRefund(po.payment.ID, po.lines[0].ID, 100_000))
	if err := e.store.ReplaceEvidence(t.Context(), payments.SlotRefundReceipt, r.ID, uuid.NewV7(), uuid.NewV7()); !errors.Is(err, payments.ErrInvalidState) {
		t.Fatalf("approved refund: %v, want ErrInvalidState", err)
	}
	res, err := e.completeAs(t, "finance_admin", r.ID)
	if err != nil {
		t.Fatal(err)
	}
	receipt := *res.Refund.ReceiptFileID
	pgReject(t, e, d, receipt)
	next = e.files.add(files.PurposeRefundReceipt)
	if _, err := e.svc.AdminReplaceRefundReceipt(ctxOf(t, fin), fin, r.ID, next); err != nil {
		t.Fatal(err)
	}
	if n := pgCount(t, d, `SELECT count(*) FROM refunds WHERE id = $1 AND receipt_file_id = $2 AND status = 'completed'`, r.ID, next); n != 1 {
		t.Fatal("the refund's receipt was not replaced")
	}
}
