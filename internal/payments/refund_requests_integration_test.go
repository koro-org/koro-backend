//go:build integration

package payments_test

import (
	"errors"
	"sync"
	"testing"

	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/payments"
	"koro/internal/platform/idempotency"
)

// TestRefundRequest_PostgresAcceptSameTx (BE-132) on PostgreSQL: the buyer's
// request is stored with its order's buyer and its line's seller (composite
// FKs) and its evidence; a second waiting request of the line is refused by
// refund_requests_line_pending_uq — also when two arrive at once; an
// acceptance that fails at its last step (its audit record) leaves neither
// the refund nor the decision behind and the request still waits; the
// acceptance then opens the refund (FK refund_id) whose four eyes and EFT
// complete the line's refund; a decided request is final.
func TestRefundRequest_PostgresAcceptSameTx(t *testing.T) {
	t.Parallel()
	e, d := newPGEnv(t)
	po := e.paid(t, e.activeAccount(t, ibanKoro, true), orderSpec{platformLine: true}, 0)
	line := po.lines[0].ID
	ev := e.files.addOrg(files.PurposeRefundRequestEvidence, id("company_a"))
	v, err := e.askRefund(t, "company_admin", po.order.ID, line, 400_000, func(in *payments.RefundRequestInput) { in.EvidenceFileID = &ev })
	if err != nil {
		t.Fatal(err)
	}
	r := v.Request
	if n := pgCount(t, d, `SELECT count(*) FROM refund_requests WHERE id = $1 AND org_id = $2 AND seller_org_id = $3 AND status = 'open'
		AND evidence_file_id = $4 AND amount = 400000 AND target_iban = $5 AND order_no = $6`,
		r.ID, id("company_a"), id("club_a"), ev, ibanOther, po.order.OrderNo); n != 1 {
		t.Fatalf("stored request rows = %d", n)
	}
	if _, err := e.askRefund(t, "company_admin", po.order.ID, line, 1, nil); !errors.Is(err, payments.ErrRefundRequestOpen) {
		t.Fatalf("second waiting request = %v", err)
	}
	// Two requests of the platform line at once: one is stored.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Go(func() {
			_, errs[i] = e.askRefund(t, "company_admin", po.order.ID, po.lines[1].ID, 1_000, nil)
		})
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) || !errors.Is(errors.Join(errs...), payments.ErrRefundRequestOpen) {
		t.Fatalf("concurrent requests = %v", errs)
	}
	// The acceptance is one transaction.
	e.audit.FailOn(authz.PaymentsRefundRequestAccept, errors.New("audit is down"))
	if _, err := e.acceptAs(t, "finance_admin", r.ID, payments.AcceptInput{}); err == nil {
		t.Fatal("accept with the audit down succeeded")
	}
	if n := pgCount(t, d, `SELECT count(*) FROM refunds WHERE order_line_id = $1`, line); n != 0 {
		t.Fatalf("refunds after the failed acceptance = %d", n)
	}
	if n := pgCount(t, d, `SELECT count(*) FROM refund_requests WHERE id = $1 AND status = 'open' AND refund_id IS NULL`, r.ID); n != 1 {
		t.Fatal("the failed acceptance changed the request")
	}
	e.audit.FailOn(authz.PaymentsRefundRequestAccept, nil)
	fin := actor(t, "finance_admin")
	key := idempotency.Key{Scope: fin.UserID.String() + ":POST accept", Value: "pg-accept", Status: 200}
	ctx := idempotency.WithKey(ctxOf(t, fin), key)
	av, err := e.svc.AcceptRefundRequest(ctx, fin, r.ID, payments.AcceptInput{})
	if err != nil || av.Request.RefundID == nil {
		t.Fatalf("accept = %+v, %v", av, err)
	}
	if again, err := e.svc.AcceptRefundRequest(ctx, fin, r.ID, payments.AcceptInput{}); err != nil || again.Request.RefundID == nil ||
		*again.Request.RefundID != *av.Request.RefundID {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	if n := pgCount(t, d, `SELECT count(*) FROM refund_requests q JOIN refunds f ON f.id = q.refund_id
		WHERE q.id = $1 AND q.status = 'accepted' AND f.status = 'requested' AND f.requested_by = $2 AND f.amount = 400000
		AND f.order_line_id = $3 AND f.target_iban = $4`, r.ID, fin.UserID, line, ibanOther); n != 1 {
		t.Fatalf("accepted request with its refund = %d", n)
	}
	if _, err := e.approve(t, "finance_admin_2", *av.Request.RefundID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.completeAs(t, "finance_admin", *av.Request.RefundID); err != nil {
		t.Fatal(err)
	}
	if n := pgCount(t, d, `SELECT count(*) FROM order_lines WHERE id = $1 AND refunded_amount = 400000`, line); n != 1 {
		t.Fatal("the line is not refunded")
	}
	cma := actor(t, "company_admin")
	if _, err := e.svc.WithdrawRefundRequest(ctxOf(t, cma), cma, r.ID); !errors.Is(err, payments.ErrInvalidState) {
		t.Fatalf("withdraw an accepted request = %v", err)
	}
	if got, err := e.svc.GetRefundRequest(ctxOf(t, cma), cma, r.ID); err != nil || got.Refund == nil || got.Refund.Status != payments.RefundStatusCompleted {
		t.Fatalf("the buyer's read = %+v, %v", got, err)
	}
}
