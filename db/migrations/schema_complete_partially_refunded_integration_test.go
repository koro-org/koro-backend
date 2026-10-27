//go:build integration

package migrations_test

import (
	"testing"
	"time"
	"uuid"

	"koro/internal/platform/testutil"
)

// TestSchema_CompletePartiallyRefundedOrders (000158; BE-135, T-382): the
// correction completes the partially refunded orders whose every line is
// final — fulfilled, cancelled, or (not on model C) its service refunded in
// full — with one fulfilled, keeping completed_at when the order had
// completed before; orders still running, with nothing delivered, or of
// model C with only a fee refunded keep their state. Money columns and lines
// do not change, and a second run changes nothing.
func TestSchema_CompletePartiallyRefundedOrders(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	s := w.shop()
	owner := testutil.OwnerConn(t, w.d)
	up := mustRead(t, "000158_data_complete_partially_refunded_orders.up.sql")
	const (
		cancel    = `UPDATE order_lines SET fulfillment_status = 'cancelled' WHERE id = $1`
		refund    = `UPDATE order_lines SET refunded_amount = $2 WHERE id = $1`
		partially = `UPDATE orders SET status = 'partially_refunded', refunded_amount = $2 WHERE id = $1`
	)
	// paidCart is dropOrder's two-line cart (1,200,000 + 660,000), paid.
	paidCart := func() (order, first, second uuid.UUID) {
		order, first, second = w.dropOrder(s)
		w.markPaid(order)
		return order, first, second
	}

	// Stuck: one line delivered, the other cancelled and refunded.
	stuck, first, second := paidCart()
	w.fulfil(first)
	w.exec(cancel, second)
	w.exec(refund, second, 660000)
	w.exec(partially, stuck, 660000)
	// Stuck: one line delivered, the other refunded in full while pending (T-309).
	full, f1, f2 := paidCart()
	w.fulfil(f1)
	w.exec(refund, f2, 660000)
	w.exec(partially, full, 660000)
	// Completed first, then a refund moved it back: completed_at is kept.
	back, b1, b2 := paidCart()
	w.fulfil(b1)
	w.exec(cancel, b2)
	earlier := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	w.exec(`UPDATE orders SET status = 'completed', completed_at = $2 WHERE id = $1`, back, earlier)
	w.exec(refund, b2, 660000)
	w.exec(partially, back, 660000)
	// Still running: the first line is still to deliver.
	running, _, r2 := paidCart()
	w.exec(cancel, r2)
	w.exec(refund, r2, 660000)
	w.exec(partially, running, 660000)
	// Nothing delivered: both lines cancelled, one refunded, the other's refund due.
	nothing, n1, n2 := paidCart()
	w.exec(cancel, n1)
	w.exec(cancel, n2)
	w.exec(refund, n2, 660000)
	w.exec(partially, nothing, 660000)
	// Model C: one line delivered, the other's fee refunded while it is pending
	// (the service was paid to the payee directly; it may still be delivered).
	direct, c1, c2 := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	w.mustCommit(
		w.orderStmt(s, direct, map[string]string{"model_code": "'C'", "collection_method": "'direct_to_payee'",
			"subtotal_amount": "1000000", "vat_amount": "200000", "buyer_fee_amount": "120000", "total_amount": "1320000",
			"payable_amount": "1320000", "koro_collect_amount": "120000", "direct_to_payee_amount": "1200000"}),
		lineStmt(s, c1, direct, secondLine),
		lineStmt(s, c2, direct, secondLine),
	)
	w.exec(`UPDATE orders SET direct_confirmed_at = now() WHERE id = $1`, direct)
	w.markPaid(direct)
	w.fulfil(c1)
	w.exec(refund, c2, 60000)
	w.exec(partially, direct, 60000)

	const money = `SELECT count(*) FROM orders WHERE id = $1 AND status = $2 AND refunded_amount = $3 AND paid_amount = koro_collect_amount`
	check := func(run string) {
		t.Helper()
		for _, c := range []struct {
			name     string
			order    uuid.UUID
			status   string
			refunded int
		}{
			{"delivered and cancelled", stuck, "completed", 660000},
			{"delivered and refunded in full", full, "completed", 660000},
			{"completed before the refund", back, "completed", 660000},
			{"a line still to deliver", running, "partially_refunded", 660000},
			{"nothing delivered", nothing, "partially_refunded", 660000},
			{"model C, a fee refunded", direct, "partially_refunded", 60000},
		} {
			if n := w.count(money, c.order, c.status, c.refunded); n != 1 {
				t.Errorf("%s: %s is not %s with %d refunded", run, c.name, c.status, c.refunded)
			}
		}
		if n := w.count(`SELECT count(*) FROM orders WHERE id IN ($1, $2) AND completed_at IS NOT NULL`, stuck, full); n != 2 {
			t.Errorf("%s: the corrected orders have no completed_at", run)
		}
		if n := w.count(`SELECT count(*) FROM orders WHERE id = $1 AND completed_at = $2`, back, earlier); n != 1 {
			t.Errorf("%s: the earlier completion time was not kept", run)
		}
		if n := w.count(`SELECT count(*) FROM order_lines WHERE id IN ($1, $2) AND fulfillment_status = 'pending'`, f2, c2); n != 2 {
			t.Errorf("%s: a refunded pending line changed", run)
		}
	}
	if _, err := owner.Exec(t.Context(), up); err != nil {
		t.Fatalf("000158 up: %v", err)
	}
	check("first run")
	var at time.Time
	if err := w.d.Pool.QueryRow(t.Context(), `SELECT completed_at FROM orders WHERE id = $1`, stuck).Scan(&at); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(t.Context(), up); err != nil {
		t.Fatalf("000158 up again: %v", err)
	}
	check("second run")
	if n := w.count(`SELECT count(*) FROM orders WHERE id = $1 AND completed_at = $2`, stuck, at); n != 1 {
		t.Error("the second run moved a corrected order's completed_at")
	}
	down := mustRead(t, "000158_data_complete_partially_refunded_orders.down.sql")
	if _, err := owner.Exec(t.Context(), down); err != nil {
		t.Fatalf("000158 down: %v", err)
	}
	check("after down")
}
