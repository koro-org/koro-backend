//go:build integration

package payments_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"uuid"

	"koro/internal/files"
	"koro/internal/ledger"
	"koro/internal/orders"
	"koro/internal/payments"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/outbox"
)

// switchOutbox fails the outbox writes of one topic while fail is set.
type switchOutbox struct {
	outbox.Writer
	topic string
	fail  *atomic.Bool
}

func (o switchOutbox) Add(ctx context.Context, e domainevent.Envelope) error {
	if o.fail.Load() && e.Topic == o.topic {
		return errors.New("outbox is down")
	}
	return o.Writer.Add(ctx, e)
}

// TestCompleteRefund_LedgerAndOrderSameTx (01 §6.3 "İade tamamlama"): the
// completion is one transaction on PostgreSQL. With payment.refunded failing
// at the last step nothing of the chain survives — the refund stays
// approved without its receipt, the payment, the order and the line are not
// refunded, no refund_completed transaction exists, no event leaves and the
// Idempotency-Key is not consumed; the retry with the same key completes it
// all at once: refund, payment, order, line, ledger (balanced at COMMIT,
// the payee owed nothing for the line) and the event.
func TestCompleteRefund_LedgerAndOrderSameTx(t *testing.T) {
	t.Parallel()
	fail := &atomic.Bool{}
	fail.Store(true)
	e, d := newPGEnv(t, withOutbox(func(w outbox.Writer) outbox.Writer {
		return switchOutbox{Writer: w, topic: domainevent.TopicPaymentRefunded, fail: fail}
	}))
	po := e.paid(t, e.activeAccount(t, ibanKoro, true), orderSpec{}, 0)
	line := po.lines[0].ID
	r := e.approved(t, lineRefund(po.payment.ID, line, 1_200_000))
	fin := actor(t, "finance_admin")
	ctx := idempotency.WithKey(ctxOf(t, fin), idempotency.Key{
		Scope: fin.UserID.String() + ":POST /admin/refunds/{refund_id}/complete", Value: "pg-complete-1", Status: 200})
	in := payments.RefundReceipt{BankReference: "EFT-20261014-1", ReceiptFileID: e.files.add(files.PurposeRefundReceipt)}
	keysBefore := pgCount(t, d, `SELECT count(*) FROM idempotency_keys`)
	if _, err := e.svc.CompleteRefund(ctx, fin, r.ID, in); err == nil {
		t.Fatal("the completion succeeded with the outbox down")
	}
	state := func() (refund string, receipt bool, payRefunded, orderRefunded, lineRefunded int64, orderStatus string, txns int) {
		t.Helper()
		if err := d.Pool.QueryRow(t.Context(), `SELECT r.status, r.receipt_file_id IS NOT NULL, p.refunded_amount, o.refunded_amount,
			l.refunded_amount, o.status FROM refunds r JOIN payments p ON p.id = r.payment_id JOIN orders o ON o.id = r.order_id
			JOIN order_lines l ON l.id = r.order_line_id WHERE r.id = $1`, r.ID).Scan(&refund, &receipt, &payRefunded, &orderRefunded,
			&lineRefunded, &orderStatus); err != nil {
			t.Fatal(err)
		}
		txns = pgCount(t, d, `SELECT count(*) FROM ledger_transactions WHERE kind = 'refund_completed' AND ref_id = $1`, r.ID)
		return
	}
	if st, rc, pr, or, lr, os, n := state(); st != "approved" || rc || pr != 0 || or != 0 || lr != 0 || os != "paid" || n != 0 {
		t.Fatalf("after the rollback: refund %s receipt %v, refunded payment %d order %d line %d, order %s, %d refund_completed",
			st, rc, pr, or, lr, os, n)
	}
	if n := pgCount(t, d, `SELECT count(*) FROM idempotency_keys`); n != keysBefore {
		t.Fatalf("idempotency keys %d → %d after the rollback", keysBefore, n)
	}
	if n := len(e.events(domainevent.TopicPaymentRefunded)); n != 0 {
		t.Fatalf("payment.refunded left a rolled back transaction (%d)", n)
	}
	if b := payeeBalance(t, e); b != 1_080_000 {
		t.Fatalf("payee balance after the rollback %d", b)
	}

	fail.Store(false)
	res, err := e.svc.CompleteRefund(ctx, fin, r.ID, in)
	if err != nil || res.Refund.Status != payments.RefundStatusCompleted {
		t.Fatalf("retry = %+v, %v", res, err)
	}
	if st, rc, pr, or, lr, os, n := state(); st != "completed" || !rc || pr != 1_200_000 || or != 1_200_000 || lr != 1_200_000 ||
		os != string(orders.StatusRefunded) || n != 1 {
		t.Fatalf("after the commit: refund %s receipt %v, refunded payment %d order %d line %d, order %s, %d refund_completed",
			st, rc, pr, or, lr, os, n)
	}
	if n := pgCount(t, d, `SELECT count(*) FROM payments WHERE id = $1 AND status = 'refunded'`, po.payment.ID); n != 1 {
		t.Fatal("the payment is not refunded")
	}
	if n := len(e.events(domainevent.TopicPaymentRefunded)); n != 1 {
		t.Fatalf("payment.refunded = %d", n)
	}
	if b := payeeBalance(t, e); b != 0 {
		t.Fatalf("payee balance after the refund %d", b)
	}
	var sum int64
	if err := d.Pool.QueryRow(t.Context(), `SELECT coalesce(sum(amount), 0) FROM ledger_entries`).Scan(&sum); err != nil || sum != 0 {
		t.Fatalf("ledger sum %d, %v", sum, err)
	}
	// The same key replays the completion; another key finds it completed.
	again, err := e.svc.CompleteRefund(ctx, fin, r.ID, in)
	if err != nil || again.TransactionID != res.TransactionID {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	if _, err := e.completeAs(t, "finance_admin", r.ID); !errors.Is(err, payments.ErrInvalidState) {
		t.Fatalf("second completion = %v", err)
	}
}

// paidOutLine reports one line as already paid out (payout_id set), as
// orders does after a payout's mark-paid (06 H-59).
type paidOutLine struct {
	payments.Orders
	line, payout uuid.UUID
}

func (o paidOutLine) GetLine(ctx context.Context, id uuid.UUID) (orders.Line, error) {
	l, err := o.Orders.GetLine(ctx, id)
	if err == nil && id == o.line {
		l.PayoutID = &o.payout
	}
	return l, err
}

// TestRefund_Postgres_Clawback (06 H-59): on PostgreSQL the refund of a
// paid-out line posts its seller part to payee_receivable (the account
// seeded by 000110, an entry naming the line and no payee:
// ledger_entries_account_chk) and commits balanced.
func TestRefund_Postgres_Clawback(t *testing.T) {
	t.Parallel()
	w := &paidOutLine{payout: uuid.NewV7()}
	e, d := newPGEnv(t, withOrders(func(o payments.Orders) payments.Orders { w.Orders = o; return w }))
	po := e.paid(t, e.activeAccount(t, ibanKoro, true), orderSpec{}, 0)
	w.line = po.lines[0].ID
	r := e.approved(t, lineRefund(po.payment.ID, w.line, 600_000))
	res, err := e.completeAs(t, "finance_admin", r.ID)
	if err != nil || res.Clawback != 540_000 {
		t.Fatalf("complete = %+v, %v", res, err)
	}
	var amount int64
	var payee *uuid.UUID
	if err := d.Pool.QueryRow(t.Context(), `SELECT e.amount, e.payee_id FROM ledger_entries e JOIN ledger_transactions t ON t.id = e.transaction_id
		WHERE t.kind = 'refund_completed' AND e.account_code = 'payee_receivable' AND e.order_line_id = $1`, w.line).Scan(&amount, &payee); err != nil ||
		amount != 540_000 || payee != nil {
		t.Fatalf("payee_receivable entry %d payee %v, %v", amount, payee, err)
	}
	shares, err := e.ledMod.RefundedShares(t.Context(), w.line)
	if err != nil || shares != (ledger.LineShares{Seller: 540_000, Commission: 50_000, CommissionVAT: 10_000}) {
		t.Fatalf("refunded shares = %+v, %v", shares, err)
	}
	// The payee's payable is untouched by the clawback.
	if b := payeeBalance(t, e); b != 1_080_000 {
		t.Fatalf("payee balance %d", b)
	}
}

// TestCompleteRefund_ConcurrentSameRefund_Postgres: finance admins
// completing one refund at once on PostgreSQL complete it once (the refund
// row lock); the others get 409 payments.invalid_state and the money left
// once.
func TestCompleteRefund_ConcurrentSameRefund_Postgres(t *testing.T) {
	t.Parallel()
	e, d := newPGEnv(t)
	po := e.paid(t, e.activeAccount(t, ibanKoro, true), orderSpec{}, 0)
	r := e.approved(t, lineRefund(po.payment.ID, po.lines[0].ID, 1_200_000))
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Go(func() { _, errs[i] = e.completeAs(t, "finance_admin", r.ID) })
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, payments.ErrInvalidState):
		default:
			t.Errorf("complete: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d completions, want 1", ok)
	}
	if n := pgCount(t, d, `SELECT count(*) FROM ledger_transactions WHERE kind = 'refund_completed'`); n != 1 {
		t.Fatalf("refund_completed = %d", n)
	}
	if n := pgCount(t, d, `SELECT count(*) FROM payments WHERE id = $1 AND refunded_amount = 1200000`, po.payment.ID); n != 1 {
		t.Fatal("the payment is not refunded once")
	}
}
