//go:build integration

package payments_test

import (
	"context"
	"io/fs"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"koro/db/migrations"
	"koro/internal/files"
	"koro/internal/ledger"
	"koro/internal/orders"
	"koro/internal/payments"
	"koro/internal/platform/db"
	"koro/internal/platform/testutil"
)

func TestModelC_PostgresCaptureConfirmRefundBothOrders(t *testing.T) {
	t.Parallel()
	for _, first := range []bool{false, true} {
		name := "fee_first"
		if first {
			name = "direct_first"
		}
		t.Run(name, func(t *testing.T) { t.Parallel(); e, _ := newPGEnv(t); exerciseModelC(t, e, first) })
	}
}

// An order lock is deliberately held by direct confirmation before the
// fee transaction starts. Fee matching must wait without a child FK lock
// that prevents the direct transaction from committing.
type directGate struct {
	payments.Orders
	arrived, resume chan struct{}
}

func (g directGate) ConfirmDirectPayment(ctx context.Context, orderID, paymentID uuid.UUID) (orders.PaymentEffect, error) {
	eff, err := g.Orders.ConfirmDirectPayment(ctx, orderID, paymentID)
	if err != nil {
		return eff, err
	}
	close(g.arrived)
	select {
	case <-g.resume:
	case <-ctx.Done():
		return eff, ctx.Err()
	}
	return eff, nil
}

func TestModelC_PostgresConcurrentFeeAndConfirmation(t *testing.T) {
	t.Parallel()
	arrived, resume := make(chan struct{}), make(chan struct{})
	var release sync.Once
	defer release.Do(func() { close(resume) })
	e, d := newPGEnv(t, withOrders(func(o payments.Orders) payments.Orders {
		return directGate{Orders: o, arrived: arrived, resume: resume}
	}))
	p := e.order(t, orderSpec{modelC: true})
	acct := e.activeAccount(t, ibanKoro, true)
	tr := e.transfer(t, acct, 120_000, p.order.OrderNo, true)
	fin := actor(t, "finance_admin")
	ctx, cancel := context.WithTimeout(ctxOf(t, fin), 20*time.Second)
	defer cancel()
	in := payments.DirectInput{OrderID: p.order.ID, ExpectedOrderNo: p.order.OrderNo, EvidenceFileID: e.files.add(files.PurposeTransferReceipt)}
	errs := make(chan error, 2)
	go func() { _, err := e.svc.ConfirmDirectPayment(ctx, fin, in); errs <- err }()
	select {
	case <-arrived:
	case err := <-errs:
		t.Fatalf("early direct: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	go func() {
		_, err := e.svc.MatchTransfer(ctx, fin, tr.ID, payments.MatchInput{OrderID: p.order.ID, ExpectedOrderNo: p.order.OrderNo})
		errs <- err
	}()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var waiting bool
		if err := d.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND wait_event_type='Lock')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-errs:
			t.Fatalf("fee ended before order wait: %v", err)
		case <-tick.C:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	release.Do(func() { close(resume) })
	for range 2 {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if n := pgCount(t, d, `SELECT count(*) FROM payments WHERE order_id=$1`, p.order.ID); n != 2 {
		t.Fatalf("payments=%d", n)
	}
	if n := pgCount(t, d, `SELECT count(*) FROM ledger_transactions WHERE kind='order_allocated' AND ref_id=$1`, p.order.ID); n != 1 {
		t.Fatalf("allocations=%d", n)
	}
	got, _ := e.ordersM.Orders.ForPayment(t.Context(), p.order.ID)
	if got.Status != orders.StatusPaid {
		t.Fatalf("order=%+v", got)
	}
}

func TestModelC_PostgresFinalStepRollback(t *testing.T) {
	t.Parallel()
	var fail atomic.Bool
	e, d := newPGEnv(t, withPoster(func(p payments.Poster) payments.Poster {
		return switchPoster{next: p, kind: ledger.KindOrderAllocated, fail: &fail}
	}))
	p := e.order(t, orderSpec{modelC: true})
	acct := e.activeAccount(t, ibanKoro, true)
	if _, err := e.match(t, e.transfer(t, acct, 120_000, p.order.OrderNo, true), p, ""); err != nil {
		t.Fatal(err)
	}
	fin := actor(t, "finance_admin")
	in := payments.DirectInput{OrderID: p.order.ID, ExpectedOrderNo: p.order.OrderNo, EvidenceFileID: e.files.add(files.PurposeTransferReceipt)}
	fail.Store(true)
	if _, err := e.svc.ConfirmDirectPayment(ctxOf(t, fin), fin, in); err == nil {
		t.Fatal("allocation failure accepted")
	}
	var confirmed *time.Time
	if err := d.Pool.QueryRow(t.Context(), `SELECT direct_confirmed_at FROM orders WHERE id=$1`, p.order.ID).Scan(&confirmed); err != nil {
		t.Fatal(err)
	}
	if confirmed != nil || pgCount(t, d, `SELECT count(*) FROM payments WHERE order_id=$1`, p.order.ID) != 1 {
		t.Fatal("failed final step kept evidence state/payment")
	}
	got, _ := e.ordersM.Orders.ForPayment(t.Context(), p.order.ID)
	if got.Status != orders.StatusAwaitingPayment || got.PaidAmount != 120_000 {
		t.Fatalf("rollback order=%+v", got)
	}
	fail.Store(false)
	if r, err := e.svc.ConfirmDirectPayment(ctxOf(t, fin), fin, in); err != nil || !r.Order.Paid {
		t.Fatalf("retry=%+v,%v", r, err)
	}
}

func TestModelC_MigrationUpDownEvidenceBackfill(t *testing.T) {
	t.Parallel()
	e, d := newPGEnv(t)
	c := e.order(t, orderSpec{modelC: true})
	without := e.order(t, orderSpec{modelC: true})
	a := e.order(t, orderSpec{})
	owner := testutil.OwnerConn(t, d)
	apply := func(direction string) {
		t.Helper()
		b, err := fs.ReadFile(migrations.FS, "000122_model_c_direct_confirmation."+direction+".sql")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = owner.Exec(t.Context(), string(b)); err != nil {
			t.Fatal(err)
		}
	}
	apply("down")
	evidence := e.files.add(files.PurposeTransferReceipt)
	by := id("finance_admin")
	at := t0.Add(-time.Hour)
	p := payments.Payment{ID: uuid.NewV7(), OrderID: c.order.ID, OrgID: c.order.OrgID, Provider: payments.ProviderDirectConfirmed, Amount: c.order.DirectToPayeeAmount, Currency: "TRY", Status: payments.PaymentCaptured, CapturedAt: &at, CapturedBy: &by, EvidenceFileID: &evidence, CreatedAt: t0, UpdatedAt: t0}
	if err := e.store.InsertPayment(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	apply("up")
	var got *time.Time
	if err := d.Pool.QueryRow(t.Context(), `SELECT direct_confirmed_at FROM orders WHERE id=$1`, c.order.ID).Scan(&got); err != nil || got == nil || !got.Equal(at) {
		t.Fatalf("evidence backfill=%v,%v", got, err)
	}
	if err := d.Pool.QueryRow(t.Context(), `SELECT direct_confirmed_at FROM orders WHERE id=$1`, without.order.ID).Scan(&got); err != nil || got != nil {
		t.Fatalf("invented evidence=%v,%v", got, err)
	}
	for _, q := range []struct {
		sql string
		id  uuid.UUID
	}{
		{`UPDATE orders SET direct_confirmed_at=NULL WHERE id=$1`, c.order.ID},
		{`UPDATE orders SET direct_confirmed_at=now() WHERE id=$1`, c.order.ID},
		{`UPDATE orders SET direct_confirmed_at=now() WHERE id=$1`, a.order.ID},
	} {
		if _, err := d.Pool.Exec(t.Context(), q.sql, q.id); db.SQLState(err) != db.CodeCheckViolation {
			t.Fatalf("guard=%v", err)
		}
	}
	pgExec(t, d, `UPDATE orders SET status='cancelled',cancelled_at=now() WHERE id=$1`, c.order.ID)
	if err := d.Pool.QueryRow(t.Context(), `SELECT direct_confirmed_at FROM orders WHERE id=$1`, c.order.ID).Scan(&got); err != nil || got == nil || !got.Equal(at) {
		t.Fatalf("cancel lost evidence=%v,%v", got, err)
	}
	apply("down")
	apply("up")
}
