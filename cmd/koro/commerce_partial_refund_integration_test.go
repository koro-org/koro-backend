//go:build integration

package main

import (
	"net/http"
	"testing"
	"uuid"
)

// TestCommerce_PartialRefund_OrderCompletes_BuyerClosable (T-382, BE-135):
// through the real API and worker, a two-booth order loses one booth after
// payment (offering cancelled, the line refunded under four eyes): the order
// is partially refunded and still holds its buyer. Once the other booth is
// delivered the order completes with its refund kept, and only money in
// flight still holds the buyer — a waiting refund request does
// (hooks.openCommerce, payments) until it is withdrawn; then the company
// can be closed.
func TestCommerce_PartialRefund_OrderCompletes_BuyerClosable(t *testing.T) {
	t.Parallel()
	e := newCommerce(t)
	// The demo seed's unpaid orders of the company would hold it on their own.
	rows, err := e.d.Pool.Query(t.Context(), `SELECT id::text FROM orders WHERE org_id=$1 AND status='awaiting_payment'`, e.company)
	if err != nil {
		t.Fatal(err)
	}
	var unpaid []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		unpaid = append(unpaid, id)
	}
	if rows.Close(); rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	for _, id := range unpaid {
		e.post("/orders/"+id+"/cancel", e.buyer, map[string]any{"reason": "Synthetic cleanup"}, http.StatusOK)
	}

	kept, gone := e.offering(false), e.offering(false)
	out := e.c.do(http.MethodPost, "/api/v1/orgs/"+e.company+"/orders", e.buyer, map[string]any{"buyer_party_id": e.party, "lines": []any{
		map[string]any{"offering_id": kept, "quantity": 1}, map[string]any{"offering_id": gone, "quantity": 1},
	}}, map[string]string{"Idempotency-Key": uuid.NewV7().String()}, http.StatusCreated)
	o := out["orders"].([]any)[0].(map[string]any)
	p := commerceOrder{id: o["order_id"].(string), no: o["order_no"].(string), amount: int64(o["payable_amount"].(float64))}
	lineOf := map[string]string{}
	for _, v := range o["lines"].([]any) {
		l := v.(map[string]any)
		lineOf[l["offering_id"].(string)] = l["line_id"].(string)
	}
	if p.amount != 24000 || len(lineOf) != 2 {
		t.Fatalf("two-booth order: %v", o)
	}
	capture := e.match(p, 24000)
	e.orderState(p, "paid", 24000, 0)

	e.post("/offerings/"+gone+"/cancel", e.clubToken, map[string]any{"cancel_reason": "Synthetic cancellation"}, http.StatusOK)
	waitUntil(t, "the paid line cancelled by outbox", func() bool {
		return e.count(`SELECT count(*) FROM order_lines WHERE id=$1 AND fulfillment_status='cancelled'`, lineOf[gone]) == 1
	})
	r := e.post("/admin/refunds", e.finA, map[string]any{"payment_id": capture["payment"].(map[string]any)["payment_id"],
		"order_line_id": lineOf[gone], "amount": 12000, "reason": "offering_cancelled", "target_iban": commerceIBAN}, http.StatusCreated)
	e.refund(r["refund_id"].(string))
	e.orderState(p, "partially_refunded", 24000, 12000)

	closeBody := func() map[string]any {
		return map[string]any{"reason": "Synthetic closure", "reauth": e.proof(e.buyerTOTP)}
	}
	refused := func(step string) {
		t.Helper()
		if got := e.post("/orgs/"+e.company+"/close", e.buyer, closeBody(), http.StatusConflict); got["code"] != "orgs.open_commerce" {
			t.Fatalf("%s: closeOrg = %v, want orgs.open_commerce", step, got)
		}
	}
	refused("a booth still to deliver")

	e.post("/order-lines/"+lineOf[kept]+"/mark-fulfilled", e.clubToken, map[string]any{"note": "Synthetic delivery"}, http.StatusOK)
	e.post("/order-lines/"+lineOf[kept]+"/confirm", e.buyer, nil, http.StatusOK)
	e.orderState(p, "completed", 24000, 12000)
	if n := e.count(`SELECT count(*) FROM orders WHERE id=$1 AND status='completed' AND completed_at IS NOT NULL`, p.id); n != 1 {
		t.Fatal("the completed order has no completed_at")
	}
	if n := e.count(`SELECT count(*) FROM order_lines WHERE id=$1 AND fulfillment_status='cancelled' AND refunded_amount=12000`, lineOf[gone]); n != 1 {
		t.Fatal("the refunded line lost its refund")
	}

	rr := e.post("/orgs/"+e.company+"/orders/"+p.id+"/refund-requests", e.buyer, map[string]any{"order_line_id": lineOf[kept],
		"amount": 1000, "reason": "other", "explanation": "Synthetic: the booth was smaller than agreed.", "target_iban": commerceIBAN},
		http.StatusCreated)
	refused("a refund request waiting")
	e.post("/refund-requests/"+rr["refund_request_id"].(string)+"/withdraw", e.buyer, nil, http.StatusOK)

	closed := e.post("/orgs/"+e.company+"/close", e.buyer, closeBody(), http.StatusOK)
	if closed["status"] != "closed" {
		t.Fatalf("closeOrg = %v, want closed", closed)
	}
}
