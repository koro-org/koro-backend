//go:build integration

package migrations_test

import (
	"strings"
	"testing"
	"uuid"

	"koro/internal/platform/db"
	"koro/internal/platform/testutil"
)

// Tests of 000157 (BE-134, Q-28c): a line of an unpaid order that took no
// money may drop out of it, and only then do the order's amounts change —
// to the sums of its live lines.

// dropOrder is a committed two-line model A order of s: the default line
// (1,200,000 payable) and a second one (2 × 250,000 + 20 % VAT, the buyer
// pays a 60,000 fee: 660,000 payable); 1,860,000 in total.
func (w *p1aWorld) dropOrder(s shop) (order, first, second uuid.UUID) {
	w.t.Helper()
	order, first, second = uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	w.mustCommit(
		w.orderStmt(s, order, map[string]string{"subtotal_amount": "1500000", "vat_amount": "300000", "buyer_fee_amount": "60000",
			"total_amount": "1860000", "payable_amount": "1860000", "koro_collect_amount": "1860000"}),
		lineStmt(s, first, order, nil),
		lineStmt(s, second, order, secondLine),
	)
	return order, first, second
}

// secondLine are the columns of dropOrder's second line.
var secondLine = map[string]string{
	"quantity": "2", "unit_price_amount": "250000", "net_amount": "500000", "vat_amount": "100000", "gross_amount": "600000",
	"commission_payer": "'buyer'", "commission_amount": "50000", "commission_vat_amount": "10000", "buyer_fee_amount": "60000",
	"seller_net_amount": "600000", "payable_amount": "660000",
}

const (
	dropLineSQL = `UPDATE order_lines SET fulfillment_status = 'cancelled', dropped_at = now() WHERE id = $1`
	// firstOnlySQL: the amounts of dropOrder without its second line.
	firstOnlySQL = `UPDATE orders SET subtotal_amount = 1000000, vat_amount = 200000, buyer_fee_amount = 0, total_amount = 1200000,
		payable_amount = 1200000, koro_collect_amount = 1200000 WHERE id = $1`
)

// TestSchema_OrderLineDrop (000157): the drop and the new amounts commit
// together, lines first; every other way to lower an order's amounts, to
// drop a line of an order that took money, to drop the last line or to undo
// or refund a drop fails.
func TestSchema_OrderLineDrop(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	s := w.shop()
	restrict, chk := db.CodeRestrictViolation, db.CodeCheckViolation
	order, first, second := w.dropOrder(s)

	expectCommit(t, w, "amounts lowered without a drop", restrict, "orders_amounts_frozen", st(firstOnlySQL, order))
	expectCommit(t, w, "a drop without the new amounts", chk, "orders_totals_match_lines", st(dropLineSQL, second))
	expectCommit(t, w, "the amounts before the drop", restrict, "orders_amounts_frozen", st(firstOnlySQL, order), st(dropLineSQL, second))
	expectCommit(t, w, "amounts that are not the live lines'", restrict, "orders_amounts_frozen", st(dropLineSQL, second),
		st(`UPDATE orders SET subtotal_amount = 999999, vat_amount = 200000, buyer_fee_amount = 0, total_amount = 1199999,
			payable_amount = 1199999, koro_collect_amount = 1199999 WHERE id = $1`, order))
	expectCommit(t, w, "a drop of a line still pending", chk, "order_lines_dropped_chk",
		st(`UPDATE order_lines SET dropped_at = now() WHERE id = $1`, second))
	expectCommit(t, w, "every line dropped", restrict, "orders_amounts_frozen", st(dropLineSQL, first), st(dropLineSQL, second),
		st(`UPDATE orders SET subtotal_amount = 0, vat_amount = 0, buyer_fee_amount = 0, total_amount = 0, payable_amount = 0,
			koro_collect_amount = 0 WHERE id = $1`, order))
	expectCommit(t, w, "every line dropped, amounts kept", chk, "orders_totals_match_lines", st(dropLineSQL, first), st(dropLineSQL, second))
	expectCommit(t, w, "a line inserted dropped", chk, "order_lines_drop_unpaid",
		lineStmt(s, uuid.NewV7(), order, map[string]string{"fulfillment_status": "'cancelled'", "dropped_at": "now()"}))

	w.mustCommit(st(dropLineSQL, second), st(firstOnlySQL, order))
	if n := w.count(`SELECT count(*) FROM orders WHERE id = $1 AND status = 'awaiting_payment' AND payable_amount = 1200000
		AND koro_collect_amount = 1200000 AND total_amount = 1200000`, order); n != 1 {
		t.Fatalf("the order after the drop = %d rows with the new amounts, want 1", n)
	}
	expectFail(t, w.d.Pool, []p1aCase{
		{"the drop undone", `UPDATE order_lines SET dropped_at = NULL WHERE id = $1`, []any{second}, restrict, ""},
		{"a dropped line pending again", `UPDATE order_lines SET fulfillment_status = 'pending' WHERE id = $1`, []any{second}, restrict, ""},
		{"a dropped line refunded", `UPDATE order_lines SET refunded_amount = 1 WHERE id = $1`, []any{second}, restrict, ""},
		{"the amounts back up", `UPDATE orders SET subtotal_amount = 1500000, vat_amount = 300000, buyer_fee_amount = 60000,
			total_amount = 1860000, payable_amount = 1860000, koro_collect_amount = 1860000 WHERE id = $1`, []any{order}, restrict, "orders_amounts_frozen"},
	})
	// The order is paid in full of its new amount; its amounts are final.
	w.markPaid(order)
	expectFail(t, w.d.Pool, []p1aCase{{"the amounts of a paid order", `UPDATE orders SET subtotal_amount = 999999, total_amount = 1199999,
		payable_amount = 1199999, koro_collect_amount = 1199999, paid_amount = 1199999 WHERE id = $1`, []any{order}, restrict, "orders_amounts_frozen"}})

	// Money arrived (a matched transfer in part, the order paid, a confirmed
	// direct payment): no line drops.
	partial, _, partialSecond := w.dropOrder(s)
	w.exec(`UPDATE orders SET paid_amount = 1 WHERE id = $1`, partial)
	paid, _, paidSecond := w.dropOrder(s)
	w.markPaid(paid)
	for name, line := range map[string]uuid.UUID{"a line of an order paid in part": partialSecond, "a line of a paid order": paidSecond} {
		expectCommit(t, w, name, chk, "order_lines_drop_unpaid", st(dropLineSQL, line))
	}
}

// TestSchema_OrderLineDrop_ModelC (000157): on a model C order Koro's part
// of the new amounts is the buyer fee of the live lines, the rest goes to
// the payees directly; another split is refused, and a confirmed direct
// payment stops any drop.
func TestSchema_OrderLineDrop_ModelC(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	s := w.shop()
	direct := func() (order, first, second uuid.UUID) {
		order, first, second = uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
		w.mustCommit(
			w.orderStmt(s, order, map[string]string{"model_code": "'C'", "collection_method": "'direct_to_payee'",
				"subtotal_amount": "1000000", "vat_amount": "200000", "buyer_fee_amount": "120000", "total_amount": "1320000",
				"payable_amount": "1320000", "koro_collect_amount": "120000", "direct_to_payee_amount": "1200000"}),
			lineStmt(s, first, order, secondLine),
			lineStmt(s, second, order, secondLine),
		)
		return order, first, second
	}
	order, _, second := direct()
	const half = `UPDATE orders SET subtotal_amount = 500000, vat_amount = 100000, buyer_fee_amount = 60000, total_amount = 660000,
		payable_amount = 660000, koro_collect_amount = $2, direct_to_payee_amount = $3 WHERE id = $1`
	expectCommit(t, w, "Koro collecting everything of a model C order", db.CodeRestrictViolation, "orders_amounts_frozen",
		st(dropLineSQL, second), st(half, order, 660000, 0))
	w.mustCommit(st(dropLineSQL, second), st(half, order, 60000, 600000))

	confirmed, _, confirmedSecond := direct()
	w.exec(`UPDATE orders SET direct_confirmed_at = now() WHERE id = $1`, confirmed)
	expectCommit(t, w, "a line of a confirmed direct payment", db.CodeCheckViolation, "order_lines_drop_unpaid", st(dropLineSQL, confirmedSecond))
}

// TestSchema_OrderLineDrop_DownUp (000157): the down migration refuses
// while a line is dropped; without one it freezes the amounts again (an
// order's totals are the sums of all its lines) and the up migration brings
// the drop back with the same rules.
func TestSchema_OrderLineDrop_DownUp(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	s := w.shop()
	owner := testutil.OwnerConn(t, w.d)
	down, up := mustRead(t, "000157_order_line_drop.down.sql"), mustRead(t, "000157_order_line_drop.up.sql")

	order, _, second := w.dropOrder(s)
	if _, err := owner.Exec(t.Context(), down); err != nil {
		t.Fatalf("down without a dropped line: %v", err)
	}
	if n := w.count(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'order_lines' AND column_name = 'dropped_at'`); n != 0 {
		t.Fatal("down kept order_lines.dropped_at")
	}
	expectCommit(t, w, "amounts lowered after down", db.CodeRestrictViolation, "", st(firstOnlySQL, order))
	if _, err := owner.Exec(t.Context(), up); err != nil {
		t.Fatalf("up again: %v", err)
	}
	w.mustCommit(st(dropLineSQL, second), st(firstOnlySQL, order))
	_, err := owner.Exec(t.Context(), down)
	if err == nil || !strings.Contains(err.Error(), "000157 down") {
		t.Fatalf("down with a dropped line = %v, want the refusal", err)
	}
	if _, err := owner.Exec(t.Context(), "ROLLBACK"); err != nil { // the file's transaction is aborted
		t.Fatal(err)
	}
	if n := w.count(`SELECT count(*) FROM order_lines WHERE id = $1 AND dropped_at IS NOT NULL`, second); n != 1 {
		t.Fatal("the refused down changed the dropped line")
	}
}
