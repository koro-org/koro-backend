//go:build integration

package payments_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"koro/internal/files"
	"koro/internal/ledger"
	"koro/internal/payments"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/outbox"
)

// recognitionGate pauses a refund after it locks the order line but before
// it posts the late recognition, allowing the scheduled job to catch up.
type recognitionGate struct {
	payments.Poster
	arrived chan struct{}
	resume  chan struct{}
}

func (g recognitionGate) Post(ctx context.Context, tx ledger.Txn) (uuid.UUID, error) {
	if tx.Kind == ledger.KindAdjustment && tx.Memo == ledger.LateRecognitionMemo {
		close(g.arrived)
		select {
		case <-g.resume:
		case <-ctx.Done():
			return uuid.Nil(), ctx.Err()
		}
	}
	return g.Poster.Post(ctx, tx)
}

// TestRecognizePlatformSales_Postgres_ConcurrentRefund locks the line in
// a refund before the job starts. The job must wait without holding the
// recognition's unique key: otherwise its entry's FK lock and the refund's
// duplicate header insert form a deadlock. Both operations must finish,
// leaving one recognition and one reversal, with no duplicated revenue.
func TestRecognizePlatformSales_Postgres_ConcurrentRefund(t *testing.T) {
	t.Parallel()
	legacy, on := withLegacy()
	arrived, resume := make(chan struct{}), make(chan struct{})
	var release sync.Once
	defer release.Do(func() { close(resume) })
	e, d := newPGEnv(t, legacy, withPoster(func(p payments.Poster) payments.Poster {
		return recognitionGate{Poster: p, arrived: arrived, resume: resume}
	}))
	po := e.paid(t, e.activeAccount(t, ibanKoro, true), orderSpec{platformOnly: true, withheld: true}, 0)
	on.Store(false)
	line := po.lines[0].ID
	r := e.approved(t, lineRefund(po.payment.ID, line, 100_001))
	fin := actor(t, "finance_admin")
	ctx, cancel := context.WithTimeout(ctxOf(t, fin), 20*time.Second)
	defer cancel()
	ctx = idempotency.WithKey(ctx, idempotency.Key{
		Scope: fin.UserID.String() + ":POST /admin/refunds/{refund_id}/complete", Value: "concurrent-platform-refund", Status: 200})
	in := payments.RefundReceipt{BankReference: "SYNTHETIC-EFT-67", ReceiptFileID: e.files.add(files.PurposeRefundReceipt)}
	refundDone := make(chan error, 1)
	go func() {
		_, err := e.svc.CompleteRefund(ctx, fin, r.ID, in)
		refundDone <- err
	}()
	select {
	case <-arrived:
	case err := <-refundDone:
		t.Fatalf("refund ended before recognition: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	jobDone := make(chan error, 1)
	go func() {
		_, err := e.ledMod.RecognizePlatformSales(ctx)
		jobDone <- err
	}()
	// Observe a real PostgreSQL lock wait, without a timing-based sleep.
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var waiting bool
		err := d.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid() AND wait_event_type = 'Lock')`).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-jobDone:
			t.Fatalf("recognition ended without waiting for the refund: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-tick.C:
		}
	}
	release.Do(func() { close(resume) })
	for name, done := range map[string]<-chan error{"refund": refundDone, "recognition": jobDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("%s failed: %v", name, err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if t.Failed() {
		return
	}
	if n := pgCount(t, d, `SELECT count(*) FROM ledger_transactions WHERE kind = 'adjustment' AND ref_id = $1`, line); n != 1 {
		t.Fatalf("recognitions = %d, want 1", n)
	}
	unbalanced, total, acc := pgLedger(t, d)
	if unbalanced != 0 || total != 0 || acc["buyer_advances"] != 0 || acc["koro_bank"] != 174_999 ||
		acc["platform_sales_revenue"] != -159_090 || acc["vat_output"] != -15_909 {
		t.Fatalf("ledger: %d unbalanced, total %d, %v", unbalanced, total, acc)
	}
	if rep, err := e.ledMod.CheckIntegrity(ctx); err != nil || !rep.OK() {
		t.Fatalf("integrity = %+v, %v", rep.Findings, err)
	}
}

// pgLedger reads the ledger as a whole on PostgreSQL: the transactions
// that do not sum to zero, the total and the balances of the accounts.
func pgLedger(t *testing.T, d *db.DB) (unbalanced int, total int64, acc map[string]int64) {
	t.Helper()
	unbalanced = pgCount(t, d, `SELECT count(*) FROM (SELECT transaction_id FROM ledger_entries GROUP BY transaction_id
		HAVING sum(amount) <> 0) u`)
	rows, err := d.Pool.Query(t.Context(), `SELECT account_code, sum(amount)::bigint FROM ledger_entries GROUP BY account_code`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	acc = map[string]int64{}
	for rows.Next() {
		var code string
		var sum int64
		if err := rows.Scan(&code, &sum); err != nil {
			t.Fatal(err)
		}
		acc[code], total = sum, total+sum
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return unbalanced, total, acc
}

// TestPlatformSales_Postgres_PlatformAndMixedOrders_Balanced (BE-067,
// T-293): on PostgreSQL an order of Koro's own line alone (VAT withheld
// 5/10) and a mixed order (a payee's booth and Koro's line) are allocated
// at payment — Koro's sale and its VAT with the booth in one
// order_allocated — and commit balanced (ledger_txn_balanced): no
// transaction off zero, buyer_advances back to zero, Koro's revenue and
// the VAT it owes (the withheld part apart), and a clean integrity check
// over the settled lines.
func TestPlatformSales_Postgres_PlatformAndMixedOrders_Balanced(t *testing.T) {
	t.Parallel()
	e, d := newPGEnv(t)
	acct := e.activeAccount(t, ibanKoro, true)
	own := e.order(t, orderSpec{platformOnly: true, withheld: true})
	mixed := e.order(t, orderSpec{platformLine: true})
	for _, p := range []placed{own, mixed} {
		if res, err := e.match(t, e.transfer(t, acct, p.order.PayableAmount, p.order.OrderNo, false), p, ""); err != nil || !res.Order.Paid {
			t.Fatalf("match %s = %+v, %v", p.order.OrderNo, res, err)
		}
	}
	unbalanced, total, acc := pgLedger(t, d)
	want := map[string]int64{
		"koro_bank": 275_000 + 1_500_000, "buyer_advances": 0, "seller_payable": -1_080_000, "platform_commission_revenue": -100_000,
		"platform_sales_revenue": -250_000 - 250_000, "vat_output": -(50_000 - 25_000) - 20_000 - 50_000,
	}
	if unbalanced != 0 || total != 0 {
		t.Fatalf("%d unbalanced transactions, total %d", unbalanced, total)
	}
	for k, v := range want {
		if acc[k] != v {
			t.Errorf("%s = %d, want %d (all %v)", k, acc[k], v, acc)
		}
	}
	if n := pgCount(t, d, `SELECT count(*) FROM ledger_entries WHERE account_code = 'vat_output' AND amount = 25000 AND order_line_id = $1`,
		own.lines[0].ID); n != 1 {
		t.Fatalf("withholding entries of Koro's line = %d, want 1 of its own", n)
	}
	if n := pgCount(t, d, `SELECT count(*) FROM ledger_transactions WHERE kind = 'order_allocated'`); n != 2 {
		t.Fatalf("order_allocated = %d, want one per order", n)
	}
	if rep, err := e.ledMod.CheckIntegrity(t.Context()); err != nil || !rep.OK() {
		t.Fatalf("integrity = %+v, %v", rep.Findings, err)
	}
}

// TestRecognizePlatformSales_Postgres_Idempotent (BE-067, T-293): orders
// paid before the template (through the real chain, as BE-041 posted
// them: a mixed order's booth allocated, Koro's lines left in
// buyer_advances) are found by the integrity check and recognised once:
// three concurrent runs of ledger.recognize_platform_sales post one
// adjustment per line (the business key, ON CONFLICT and the replay), a
// later run posts nothing, buyer_advances is back to zero and the ledger
// is balanced and clean.
func TestRecognizePlatformSales_Postgres_Idempotent(t *testing.T) {
	t.Parallel()
	legacy, on := withLegacy()
	e, d := newPGEnv(t, legacy)
	acct := e.activeAccount(t, ibanKoro, true)
	own := e.order(t, orderSpec{platformOnly: true, withheld: true})
	mixed := e.order(t, orderSpec{platformLine: true})
	for _, p := range []placed{own, mixed} {
		if _, err := e.match(t, e.transfer(t, acct, p.order.PayableAmount, p.order.OrderNo, false), p, ""); err != nil {
			t.Fatal(err)
		}
	}
	on.Store(false)
	lines := map[string]bool{own.lines[0].ID.String(): true, mixed.lines[1].ID.String(): true}
	if _, _, acc := pgLedger(t, d); acc["buyer_advances"] != -(275_000+300_000) || acc["platform_sales_revenue"] != 0 {
		t.Fatalf("the legacy state = %v", acc)
	}
	rep, err := e.ledMod.CheckIntegrity(t.Context())
	if err != nil || len(rep.Findings) != 2 {
		t.Fatalf("integrity before = %+v, %v", rep.Findings, err)
	}
	for _, f := range rep.Findings {
		if f.Kind != ledger.FindingUnallocatedLine || !lines[f.OrderLineID.String()] || f.Detail != ledger.AllocationMissing {
			t.Fatalf("finding %+v", f)
		}
	}

	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := range errs {
		wg.Go(func() { _, errs[i] = e.ledMod.RecognizePlatformSales(t.Context()) })
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	recognitions := func() int {
		t.Helper()
		return pgCount(t, d, `SELECT count(*) FROM ledger_transactions WHERE kind = 'adjustment' AND ref_type = 'manual' AND memo = $1`,
			ledger.LateRecognitionMemo)
	}
	if n := recognitions(); n != 2 {
		t.Fatalf("late recognitions after three concurrent runs = %d, want 2", n)
	}
	again, err := e.ledMod.RecognizePlatformSales(t.Context())
	if err != nil || len(again.Recognized) != 0 || len(again.Skipped) != 0 {
		t.Fatalf("a later run = %+v, %v", again, err)
	}
	for _, j := range e.ledMod.Jobs() {
		if j.Name == ledger.RecognizeJobName {
			if err := j.Handle(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
	}
	if n := recognitions(); n != 2 {
		t.Fatalf("late recognitions after the later runs = %d, want 2", n)
	}
	unbalanced, total, acc := pgLedger(t, d)
	if unbalanced != 0 || total != 0 || acc["buyer_advances"] != 0 || acc["platform_sales_revenue"] != -500_000 ||
		acc["vat_output"] != -25_000-20_000-50_000 {
		t.Fatalf("after the recognition: %d unbalanced, total %d, %v", unbalanced, total, acc)
	}
	if rep, err := e.ledMod.CheckIntegrity(t.Context()); err != nil || !rep.OK() {
		t.Fatalf("integrity after = %+v, %v", rep.Findings, err)
	}
}

// TestRefund_Postgres_PlatformLine_RecognitionAndReversalOneTx (BE-067): the
// refund of Koro's line paid before the template recognises it and takes
// it back in the completion's one transaction on PostgreSQL: with
// payment.refunded failing at the last step neither the recognition nor
// the refund_completed survives (the line unrefunded, buyer_advances as
// before); the retry commits both, balanced.
func TestRefund_Postgres_PlatformLine_RecognitionAndReversalOneTx(t *testing.T) {
	t.Parallel()
	legacy, on := withLegacy()
	fail := &atomic.Bool{}
	e, d := newPGEnv(t, legacy, withOutbox(func(w outbox.Writer) outbox.Writer {
		return switchOutbox{Writer: w, topic: domainevent.TopicPaymentRefunded, fail: fail}
	}))
	po := e.paid(t, e.activeAccount(t, ibanKoro, true), orderSpec{platformLine: true}, 0)
	on.Store(false)
	line := po.lines[1].ID
	r := e.approved(t, lineRefund(po.payment.ID, line, 300_000))
	fin := actor(t, "finance_admin")
	ctx := idempotency.WithKey(ctxOf(t, fin), idempotency.Key{
		Scope: fin.UserID.String() + ":POST /admin/refunds/{refund_id}/complete", Value: "pg-platform-refund-1", Status: 200})
	in := payments.RefundReceipt{BankReference: "EFT-20261014-7", ReceiptFileID: e.files.add(files.PurposeRefundReceipt)}
	fail.Store(true)
	if _, err := e.svc.CompleteRefund(ctx, fin, r.ID, in); err == nil {
		t.Fatal("the completion succeeded with the outbox down")
	}
	state := func() (adjustments, refunds, lineRefunded int, advances int64) {
		t.Helper()
		adjustments = pgCount(t, d, `SELECT count(*) FROM ledger_transactions WHERE kind = 'adjustment' AND ref_id = $1`, line)
		refunds = pgCount(t, d, `SELECT count(*) FROM ledger_transactions WHERE kind = 'refund_completed'`)
		lineRefunded = pgCount(t, d, `SELECT refunded_amount FROM order_lines WHERE id = $1`, line)
		_, _, acc := pgLedger(t, d)
		return adjustments, refunds, lineRefunded, acc["buyer_advances"]
	}
	if a, rf, lr, adv := state(); a != 0 || rf != 0 || lr != 0 || adv != -300_000 {
		t.Fatalf("after the rollback: %d recognitions, %d refunds, line refunded %d, buyer_advances %d", a, rf, lr, adv)
	}
	fail.Store(false)
	res, err := e.svc.CompleteRefund(ctx, fin, r.ID, in)
	if err != nil || res.Refund.Status != payments.RefundStatusCompleted || res.Unallocated != 0 {
		t.Fatalf("retry = %+v, %v", res, err)
	}
	if a, rf, lr, adv := state(); a != 1 || rf != 1 || lr != 300_000 || adv != 0 {
		t.Fatalf("after the commit: %d recognitions, %d refunds, line refunded %d, buyer_advances %d", a, rf, lr, adv)
	}
	unbalanced, total, acc := pgLedger(t, d)
	if unbalanced != 0 || total != 0 || acc["platform_sales_revenue"] != 0 || acc["vat_output"] != -20_000 || acc["koro_bank"] != 1_200_000 {
		t.Fatalf("ledger: %d unbalanced, total %d, %v", unbalanced, total, acc)
	}
	if rep, err := e.ledMod.CheckIntegrity(t.Context()); err != nil || !rep.OK() {
		t.Fatalf("integrity = %+v, %v", rep.Findings, err)
	}
}
