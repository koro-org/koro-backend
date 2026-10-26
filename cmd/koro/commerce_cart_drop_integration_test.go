//go:build integration

package main

import (
	"net/http"
	"strings"
	"testing"

	"koro/db/seed"
	"koro/internal/platform/money"
)

// TestCommerce_Cart_UnpaidLineDropped_StaleTransferRefunded (Q-28c,
// BE-134): an unpaid cart of a 20 % booth (payee 1) and a 10 % workshop
// (payee 2) loses the workshop when its offering is cancelled — through the
// real API, outbox and worker: the order keeps its reference code and the
// booth, its amounts and payment instructions become the booth's, the
// workshop's units are back, the buyer gets an e-mail with the new amount
// and the seller an in-app notice, the drop is audited, and no ledger entry
// or invoice exists. The buyer then transfers the old amount: payments
// applies what the order now waits for, opens an overpayment refund for the
// rest (four eyes) and the ledger keeps it in buyer_advances until it is
// refunded. The dropped line is never split, invoiced, queued for a refund
// nor refundable; the booth is delivered and the order completes.
func TestCommerce_Cart_UnpaidLineDropped_StaleTransferRefunded(t *testing.T) {
	t.Parallel()
	e := newCommerce(t)
	payee2 := e.cartPayee()
	booth := e.cartOffering("Synthetic booth", 2000, e.payee, "on_payment")
	workshop := e.cartOffering("Synthetic workshop", 1000, payee2, "on_payment")
	out := e.post("/orgs/"+e.company+"/orders", e.buyer, map[string]any{"buyer_party_id": e.party, "lines": []any{
		map[string]any{"offering_id": booth, "quantity": 1}, map[string]any{"offering_id": workshop, "quantity": 1},
	}}, http.StatusCreated)
	o := out["orders"].([]any)[0].(map[string]any)
	// 10,000 + 20 % = 12,000; 10,000 + 10 % = 11,000.
	p := commerceOrder{id: o["order_id"].(string), no: o["order_no"].(string), amount: int64(o["payable_amount"].(float64))}
	if p.amount != 23000 {
		t.Fatalf("cart order: %v", o)
	}
	lineOf := map[string]string{}
	for _, v := range o["lines"].([]any) {
		l := v.(map[string]any)
		lineOf[l["offering_id"].(string)] = l["line_id"].(string)
	}
	p.booth = lineOf[booth]
	workshopLine := lineOf[workshop]
	e.mail.wait(seed.CompanyEmail, "Siparişiniz alındı")

	e.post("/offerings/"+workshop+"/cancel", e.clubToken, map[string]any{"cancel_reason": "Synthetic cancellation"}, http.StatusOK)
	waitUntil(t, "unpaid cart line dropped by outbox", func() bool {
		return e.count(`SELECT count(*) FROM order_lines WHERE id=$1 AND fulfillment_status='cancelled' AND dropped_at IS NOT NULL`, workshopLine) == 1
	})
	got := e.get("/orders/"+p.id, e.buyer)
	if got["status"] != "awaiting_payment" || got["order_no"] != p.no || got["payable_amount"] != float64(12000) ||
		got["koro_collect_amount"] != float64(12000) || got["subtotal_amount"] != float64(10000) || got["vat_amount"] != float64(2000) {
		t.Fatalf("order after the drop: %v", got)
	}
	for _, v := range got["lines"].([]any) {
		l := v.(map[string]any)
		if dropped := l["dropped_at"] != nil; dropped != (l["line_id"] == workshopLine) {
			t.Fatalf("line %v: dropped_at %v", l["line_id"], l["dropped_at"])
		}
	}
	if instr := e.get("/orders/"+p.id+"/payment-instructions", e.buyer); instr["reference_code"] != p.no || instr["amount_due_amount"] != float64(12000) {
		t.Fatalf("instructions after the drop: %v", instr)
	}
	if n := e.count(`SELECT quantity_sold FROM offerings WHERE id=$1`, workshop); n != 0 {
		t.Fatalf("units of the cancelled workshop sold=%d", n)
	}
	mail := e.mail.wait(seed.CompanyEmail, "Sipariş tutarınız değişti")
	if !strings.Contains(mail.Text, p.no) || !strings.Contains(mail.Text, money.Decimal(12000)) {
		t.Fatalf("line dropped e-mail without the reference code or the new amount: %s", mail.Text)
	}
	waitUntil(t, "line dropped notices in-app", func() bool {
		return e.count(`SELECT count(*) FROM in_app_notifications WHERE kind='orders.line_dropped' AND data->>'order_id'=$1`, p.id) == 1 &&
			e.count(`SELECT count(*) FROM in_app_notifications WHERE kind='orders.sale_line_dropped' AND data->>'order_id'=$1`, p.id) >= 1
	})
	if n := e.count(`SELECT count(*) FROM audit_events WHERE action='orders.order.cancel' AND resource_id=$1
 AND metadata->>'effect'='line_dropped' AND (metadata->>'payable_before')::bigint=23000 AND (metadata->>'payable_after')::bigint=12000`, workshopLine); n != 1 {
		t.Fatalf("drop audit events=%d", n)
	}
	e.balances(0, 0, 0, 0, 0)
	if n := e.count(`SELECT count(*) FROM invoices WHERE order_id=$1`, p.id); n != 0 {
		t.Fatalf("invoices of the unpaid cart=%d", n)
	}

	// The buyer pays the amount of the first e-mail.
	m := e.match(p, 23000)
	refundID, ok := m["overpayment_refund_id"].(string)
	if !ok || m["applied_amount"] != float64(12000) {
		t.Fatalf("stale transfer: %v", m)
	}
	e.orderState(p, "paid", 12000, 0)
	// Koro holds 23,000: 11,000 for the buyer, 10,800 for payee 1, 1,000 + 200 VAT commission; payee 2 nothing.
	e.balances(23000, -11000, 10800, 1000, 0)
	if n := e.count(`SELECT -coalesce(sum(amount),0) FROM ledger_entries WHERE account_code='seller_payable' AND payee_id=$1`, payee2); n != 0 {
		t.Fatalf("payee 2 balance=%d", n)
	}
	if n := e.count(`SELECT count(*) FROM refunds WHERE id=$1 AND amount=11000 AND reason='overpayment' AND order_line_id IS NULL AND status='requested'`, refundID); n != 1 {
		t.Fatal("the overpayment refund is not the difference")
	}
	if n := e.count(`SELECT count(*) FROM payment_splits WHERE order_line_id=$1`, workshopLine); n != 0 {
		t.Fatal("the dropped line has a payment split")
	}
	if n := e.count(`SELECT count(*) FROM payment_splits WHERE order_line_id=$1 AND seller_amount=10800 AND platform_amount=1200`, p.booth); n != 1 {
		t.Fatal("the booth's split is missing")
	}
	waitUntil(t, "the booth's on_payment invoice", func() bool {
		return e.count(`SELECT count(*) FROM invoices i JOIN invoice_lines il ON il.invoice_id=i.id WHERE il.order_line_id=$1`, p.booth) == 1
	})
	if n := e.count(`SELECT count(*) FROM invoice_lines WHERE order_line_id=$1`, workshopLine); n != 0 {
		t.Fatal("the dropped line is invoiced")
	}
	due := e.get("/admin/refunds/due", e.finA)
	for _, v := range due["items"].([]any) {
		if x := v.(map[string]any); x["order_line_id"] == workshopLine || x["order_id"] == p.id {
			t.Fatalf("the paid cart is in the refund queue: %v", x)
		}
	}
	var payment string
	e.q(`SELECT id FROM payments WHERE order_id=$1 AND status='captured'`, &payment, p.id)
	if r := e.post("/admin/refunds", e.finA, map[string]any{"payment_id": payment, "order_line_id": workshopLine, "amount": 1,
		"reason": "offering_cancelled", "target_iban": commerceIBAN}, http.StatusUnprocessableEntity); r["code"] != "orders.refund_exceeds_paid" {
		t.Fatalf("refund of the dropped line: %v", r)
	}

	e.refund(refundID)
	e.orderState(p, "paid", 12000, 0)
	e.balances(12000, 0, 10800, 1000, 0)

	e.post("/order-lines/"+p.booth+"/mark-fulfilled", e.clubToken, map[string]any{"note": "Synthetic delivery"}, http.StatusOK)
	e.post("/order-lines/"+p.booth+"/confirm", e.buyer, nil, http.StatusOK)
	e.orderState(p, "completed", 12000, 0)
	e.balances(12000, 0, 10800, 1000, 0)
}
