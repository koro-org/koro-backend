//go:build integration

package main

import (
	"net/http"
	"testing"
	"uuid"
)

// A platform product remains model A; the payee's model C service is a
// separate order. This exercises real API, bank matching, worker billing,
// entitlement activation and four-eyes fee reversal without inventing a
// platform-native C product or putting direct money in Koro's ledger.
func TestCommerce_ModelC_MixedCartFeeConfirmationRefund(t *testing.T) {
	t.Parallel()
	e := newCommerce(t)
	booth, native := e.offering(false), e.offering(true)
	e.exec(`UPDATE commerce_policies SET valid_to=now() WHERE scope='offering' AND scope_id=$1`, booth)
	policy := map[string]any{"scope": "offering", "scope_id": booth, "model_code": "C", "commission_rate_bp": 1000, "notes": "Synthetic model C arithmetic fixture", "reauth": e.proof(e.finATOTP)}
	e.deny("/admin/commerce-policies", e.buyer, policy, "forbidden")
	e.post("/admin/commerce-policies", e.finA, policy, http.StatusCreated)
	result := e.post("/orgs/"+e.company+"/orders", e.buyer, map[string]any{"buyer_party_id": e.party, "lines": []any{
		map[string]any{"offering_id": booth, "quantity": 1}, map[string]any{"offering_id": native, "quantity": 1},
	}}, http.StatusCreated)
	var c, a commerceOrder
	for _, item := range result["orders"].([]any) {
		row := item.(map[string]any)
		line := row["lines"].([]any)[0].(map[string]any)
		p := commerceOrder{id: row["order_id"].(string), no: row["order_no"].(string), amount: int64(row["payable_amount"].(float64))}
		if row["model_code"] == "C" {
			p.booth = line["line_id"].(string)
			c = p
		} else {
			p.native = line["line_id"].(string)
			a = p
		}
	}
	if c.id == "" || a.id == "" || c.amount != 13200 || a.amount != 3000 {
		t.Fatalf("split orders: %v", result)
	}
	instructions := e.get("/orders/"+c.id+"/payment-instructions", e.buyer)
	payee := instructions["direct_payments"].([]any)[0].(map[string]any)
	if instructions["amount_due_amount"] != float64(1200) || instructions["iban"] != commerceIBAN || payee["iban"] != commercePayeeIBAN || payee["direct_to_payee_amount"] != float64(12000) {
		t.Fatalf("two transfers: %v", instructions)
	}
	for _, token := range []string{e.finA, e.admin, e.clubToken} {
		view := e.get("/orders/"+c.id, token)
		if _, exists := view["direct_payments"]; exists {
			t.Fatalf("staff/seller sees buyer payment instructions: %v", view)
		}
	}
	if n := e.count(`SELECT count(*) FROM audit_events WHERE action='orders.order.payment_instructions' AND resource_id=$1`, c.id); n == 0 {
		t.Fatal("IBAN access has no audit")
	}
	e.entitlement(a, "pending", 0)
	e.match(a, 3000)
	e.entitlement(a, "active", 0)
	fee := e.match(c, 1200)
	e.orderState(c, "awaiting_payment", 1200, 0)
	e.balances(4200, -1200, 0, 0, 2500)
	if n := e.count(`SELECT count(*) FROM invoices WHERE order_id=$1`, c.id); n != 0 {
		t.Fatal("C fee invoice before direct confirmation")
	}
	evidence := e.upload(e.finA, "transfer_receipt")
	body := map[string]any{"order_id": c.id, "expected_order_no": c.no, "evidence_file_id": evidence}
	key := map[string]string{"Idempotency-Key": uuid.NewV7().String()}
	confirmed := e.c.do(http.MethodPost, "/api/v1/admin/payments/direct-confirmations", e.finA, body, key, http.StatusOK)
	again := e.c.do(http.MethodPost, "/api/v1/admin/payments/direct-confirmations", e.finA, body, key, http.StatusOK)
	direct := confirmed["payment"].(map[string]any)
	if direct["amount"] != float64(12000) || again["payment"].(map[string]any)["payment_id"] != direct["payment_id"] {
		t.Fatalf("direct evidence/replay: %v/%v", confirmed, again)
	}
	e.orderState(c, "paid", 1200, 0)
	e.balances(4200, 0, 0, 1000, 2500)
	e.event("payment.captured", direct["payment_id"].(string))
	waitUntil(t, "model C fee invoice", func() bool {
		return e.count(`SELECT count(*) FROM invoices WHERE order_id=$1 AND kind='platform_fee' AND net_amount=1000 AND vat_amount=200 AND total_amount=1200 AND recipient_party_id=$2`, c.id, e.party) == 1
	})
	if n := e.count(`SELECT count(*) FROM invoices WHERE order_id=$1`, c.id); n != 1 {
		t.Fatalf("C invoice count=%d", n)
	}
	e.post("/order-lines/"+c.booth+"/mark-fulfilled", e.clubToken, map[string]any{}, http.StatusOK)
	e.post("/order-lines/"+c.booth+"/confirm", e.buyer, nil, http.StatusOK)
	if n := e.count(`SELECT count(*) FROM payout_items WHERE order_line_id=$1`, c.booth); n != 0 {
		t.Fatal("direct service entered payout")
	}
	if n := e.count(`SELECT count(*) FROM ledger_entries WHERE order_line_id=$1 AND (payee_id IS NOT NULL OR account_code IN ('seller_payable','payee_receivable','platform_sales_revenue'))`, c.booth); n != 0 {
		t.Fatal("direct service entered Koro ledger")
	}
	refund := e.post("/admin/refunds", e.finA, map[string]any{"payment_id": fee["payment"].(map[string]any)["payment_id"], "order_line_id": c.booth, "amount": 1200, "reason": "goodwill", "target_iban": commerceIBAN}, http.StatusCreated)
	e.refund(refund["refund_id"].(string))
	e.orderState(c, "refunded", 1200, 1200)
	e.balances(3000, 0, 0, 0, 2500)
	e.entitlement(a, "active", 0)
}
