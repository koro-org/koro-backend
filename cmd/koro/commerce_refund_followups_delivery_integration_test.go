//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"
	"uuid"
)

type followupHTTPResult struct {
	status int
	body   map[string]any
	err    error
}

// Unlike the synchronous fixture helpers, an asynchronous request reports
// errors to the owning test goroutine instead of calling Fatal from a worker.
func followupPost(t *testing.T, e *commerceEnv, ctx context.Context, path, token string, body any, headers map[string]string) <-chan followupHTTPResult {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.c.base+"/api/v1"+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Koro-Client", "mobile")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	done := make(chan followupHTTPResult, 1)
	go func() {
		resp, err := e.c.http.Do(req)
		if err != nil {
			done <- followupHTTPResult{err: err}
			return
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		out := followupHTTPResult{status: resp.StatusCode, err: err}
		if err == nil {
			out.err = json.Unmarshal(raw, &out.body)
		}
		done <- out
	}()
	return done
}

func followupWaitLock(t *testing.T, e *commerceEnv, ctx context.Context, blockerPID int, advisory bool, done <-chan followupHTTPResult) int {
	t.Helper()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var waitingPID int
		query := `SELECT coalesce((SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid()
   AND $1=ANY(pg_blocking_pids(pid)) AND wait_event_type='Lock' AND query LIKE '%orders%' AND query LIKE '%FOR UPDATE%' LIMIT 1),0)`
		if advisory {
			query = `SELECT coalesce((SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid()
   AND $1=ANY(pg_blocking_pids(pid)) AND wait_event_type='Lock' AND wait_event='advisory' LIMIT 1),0)`
		}
		if err := e.d.Pool.QueryRow(ctx, query, blockerPID).Scan(&waitingPID); err != nil {
			t.Fatal(err)
		}
		if waitingPID != 0 {
			return waitingPID
		}
		select {
		case result := <-done:
			t.Fatalf("operation ended without expected lock wait: %+v", result)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-tick.C:
		}
	}
}

// T-305: the refund owns the shared payee lock before waiting for its order.
// A payout queued behind it must use the committed remainder exactly once.
// A later refund after that payout creates receivable only for the remaining
// seller share, leaving the earlier refunded share out of the clawback.
func TestCommerce_RefundFollowups_PartialPayoutLaterClawback(t *testing.T) {
	t.Parallel()
	e := newCommerce(t)
	p := e.place(false)
	capture := e.match(p, p.amount)
	payment := capture["payment"].(map[string]any)["payment_id"]
	e.post("/order-lines/"+p.booth+"/mark-fulfilled", e.clubToken, map[string]any{"note": "Synthetic delivery"}, http.StatusOK)
	e.post("/order-lines/"+p.booth+"/confirm", e.buyer, nil, http.StatusOK)
	first := e.post("/admin/refunds", e.finA, map[string]any{"payment_id": payment, "order_line_id": p.booth, "amount": 6000,
		"reason": "other", "reason_note": "Synthetic partial refund", "target_iban": commerceIBAN}, http.StatusCreated)
	rid := first["refund_id"].(string)
	e.post("/admin/refunds/"+rid+"/approve", e.finB, map[string]any{"reauth": e.proof(e.finBTOTP)}, http.StatusOK)
	receipt := e.upload(e.finA, "refund_receipt")
	body := map[string]any{"bank_reference": "SYNTHETIC-PARTIAL-REFUND", "receipt_file_id": receipt}
	key := map[string]string{"Idempotency-Key": uuid.NewV7().String()}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	blocker, err := e.d.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(context.WithoutCancel(ctx)) }()
	var blockerPID int
	if err := blocker.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(ctx, `SELECT id FROM orders WHERE id=$1 FOR UPDATE`, p.id); err != nil {
		t.Fatal(err)
	}
	refundDone := followupPost(t, e, ctx, "/admin/refunds/"+rid+"/complete", e.finA, body, key)
	refundPID := followupWaitLock(t, e, ctx, blockerPID, false, refundDone)
	payoutDone := followupPost(t, e, ctx, "/admin/payouts", e.finA, map[string]any{"payee_id": e.payee, "order_line_ids": []string{p.booth}}, map[string]string{"Idempotency-Key": uuid.NewV7().String()})
	followupWaitLock(t, e, ctx, refundPID, true, payoutDone)
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	refund, payout := <-refundDone, <-payoutDone
	if refund.err != nil || refund.status != http.StatusOK {
		t.Fatalf("refund result: %+v", refund)
	}
	if payout.err != nil || payout.status != http.StatusCreated || payout.body["amount"] != float64(5400) {
		t.Fatalf("remaining payout: %+v", payout)
	}
	e.c.do(http.MethodPost, "/api/v1/admin/refunds/"+rid+"/complete", e.finA, body, key, http.StatusOK)
	pid := payout.body["payout_id"].(string)
	e.orderState(p, "partially_refunded", 12000, 6000)
	e.balances(6000, 0, 5400, 500, 0)
	if n := e.count(`SELECT amount FROM payout_items WHERE payout_id=$1 AND order_line_id=$2`, pid, p.booth); n != 5400 {
		t.Fatalf("payout item: %d", n)
	}
	e.post("/admin/payouts/"+pid+"/approve", e.finB, map[string]any{"reauth": e.proof(e.finBTOTP)}, http.StatusOK)
	payoutReceipt := e.upload(e.finA, "payout_receipt")
	paidBody := map[string]any{"bank_reference": "SYNTHETIC-REMAINDER-PAYOUT", "receipt_file_id": payoutReceipt}
	paidKey := map[string]string{"Idempotency-Key": uuid.NewV7().String()}
	for range 2 {
		e.c.do(http.MethodPost, "/api/v1/admin/payouts/"+pid+"/mark-paid", e.finA, paidBody, paidKey, http.StatusOK)
	}
	e.balances(600, 0, 0, 500, 0)
	denied := e.post("/admin/payouts", e.finA, map[string]any{"payee_id": e.payee, "order_line_ids": []string{p.booth}}, http.StatusConflict)
	if denied["code"] != "ledger.line_not_payable" {
		t.Fatalf("second payout: %v", denied)
	}
	last := e.post("/admin/refunds", e.finA, map[string]any{"payment_id": payment, "order_line_id": p.booth, "amount": 6000,
		"reason": "other", "reason_note": "Synthetic final refund", "target_iban": commerceIBAN}, http.StatusCreated)
	e.refund(last["refund_id"].(string))
	e.orderState(p, "refunded", 12000, 12000)
	e.balanced("later refund with seller receivable")
	// The common fixture balance helper derives VAT assuming no receivable;
	// this flow has a real 5400 receivable instead of VAT after the clawback.
	for account, want := range map[string]int64{
		"koro_bank": -5400, "buyer_advances": 0, "seller_payable": 0,
		"platform_commission_revenue": 0, "platform_sales_revenue": 0, "vat_output": 0,
	} {
		if got := e.count(`SELECT coalesce(sum(amount),0) FROM ledger_entries WHERE account_code=$1`, account); got != want {
			t.Errorf("%s after clawback: %d, want %d", account, got, want)
		}
	}
	if n := e.count(`SELECT coalesce(sum(amount),0) FROM ledger_entries WHERE account_code='payee_receivable' AND order_line_id=$1`, p.booth); n != 5400 {
		t.Fatalf("clawback=%d; earlier 5400 refund must not be charged twice", n)
	}
	balancePath := "/orgs/" + e.club + "/payees/" + e.payee + "/balance"
	balance := e.get(balancePath, e.clubToken)
	if balance["receivable_amount"] != float64(5400) || balance["payable_amount"] != float64(0) {
		t.Fatalf("visible clawback balance: %v", balance)
	}
	for _, check := range []struct {
		query string
		args  []any
	}{
		{`SELECT count(*) FROM payouts WHERE id=$1 AND status='paid' AND amount=5400`, []any{pid}},
		{`SELECT count(*) FROM ledger_transactions WHERE kind='payout_paid' AND ref_id=$1`, []any{pid}},
		{`SELECT count(*) FROM order_lines WHERE id=$1 AND payout_id=$2 AND refunded_amount=12000`, []any{p.booth, pid}},
		{`SELECT count(*) FROM ledger_transactions WHERE kind='refund_completed' AND ref_id=$1`, []any{rid}},
	} {
		if n := e.count(check.query, check.args...); n != 1 {
			t.Fatalf("once-only invariant: %s => %d", check.query, n)
		}
	}
	// T-306 uses actual incoming money and a distinct finance approval; no
	// automatic deduction from another payout is introduced by this flow.
	collectionReceipt := e.upload(e.finA, "transfer_receipt")
	incoming := e.post("/admin/incoming-transfers", e.finA, map[string]any{
		"collection_account_id": e.account, "value_date": time.Now().UTC().Format("2006-01-02"), "amount": 5400,
		"sender_name": "Synthetic Seller", "description": "Synthetic receivable settlement",
		"bank_reference": "SYNTHETIC-RECEIVABLE-SETTLEMENT", "receipt_file_id": collectionReceipt}, http.StatusCreated)
	collection := e.post("/admin/receivable-collections", e.finA, map[string]any{
		"incoming_transfer_id": incoming["transfer_id"], "order_line_id": p.booth}, http.StatusCreated)
	cid := collection["collection_id"].(string)
	if collection["amount"] != float64(5400) || collection["status"] != "requested" {
		t.Fatalf("collection request: %v", collection)
	}
	e.deny("/admin/receivable-collections/"+cid+"/approve", e.finA, map[string]any{}, "authz.four_eyes_violation")
	collected := e.post("/admin/receivable-collections/"+cid+"/approve", e.finB, map[string]any{"reauth": e.proof(e.finBTOTP)}, http.StatusOK)
	if collected["status"] != "collected" || collected["requested_by"] == collected["approved_by"] {
		t.Fatalf("collection four eyes: %v", collected)
	}
	e.balances(0, 0, 0, 0, 0)
	if n := e.count(`SELECT coalesce(sum(amount),0) FROM ledger_entries WHERE account_code='payee_receivable' AND order_line_id=$1`, p.booth); n != 0 {
		t.Fatalf("receivable after manual collection: %d", n)
	}
	balance = e.get(balancePath, e.clubToken)
	if balance["receivable_amount"] != float64(0) || balance["payable_amount"] != float64(0) {
		t.Fatalf("visible collected balance: %v", balance)
	}
	if n := e.count(`SELECT count(*) FROM ledger_transactions WHERE kind='adjustment' AND ref_id=$1`, cid); n != 1 {
		t.Fatalf("collection ledger transactions: %d", n)
	}

}

// T-309: both HTTP delivery transitions reject a fully refunded line while
// another live line keeps the order partially refunded and otherwise deliverable.
func TestCommerce_RefundFollowups_FullyRefundedDeliveryHTTP(t *testing.T) {
	t.Parallel()
	e := newCommerce(t)
	pendingOffering, markedOffering, native := e.offering(false), e.offering(false), e.offering(true)
	created := e.post("/orgs/"+e.company+"/orders", e.buyer, map[string]any{
		"buyer_party_id": e.party, "lines": []any{
			map[string]any{"offering_id": pendingOffering, "quantity": 1},
			map[string]any{"offering_id": markedOffering, "quantity": 1},
			map[string]any{"offering_id": native, "quantity": 1}}}, http.StatusCreated)
	o := created["orders"].([]any)[0].(map[string]any)
	p := commerceOrder{id: o["order_id"].(string), no: o["order_no"].(string), amount: int64(o["payable_amount"].(float64))}
	lines := map[string]string{}
	for _, raw := range o["lines"].([]any) {
		line := raw.(map[string]any)
		lines[line["offering_id"].(string)] = line["line_id"].(string)
	}
	if p.amount != 27000 || len(lines) != 3 {
		t.Fatalf("three-line order: %v", o)
	}
	capture := e.match(p, p.amount)
	payment := capture["payment"].(map[string]any)["payment_id"]
	pending, marked := lines[pendingOffering], lines[markedOffering]
	e.post("/order-lines/"+marked+"/mark-fulfilled", e.clubToken, map[string]any{"note": "Synthetic delivery"}, http.StatusOK)
	for _, line := range []string{pending, marked} {
		r := e.post("/admin/refunds", e.finA, map[string]any{"payment_id": payment, "order_line_id": line, "amount": 12000,
			"reason": "other", "reason_note": "Synthetic full line refund", "target_iban": commerceIBAN}, http.StatusCreated)
		e.refund(r["refund_id"].(string))
	}
	e.orderState(p, "partially_refunded", 27000, 24000)
	for _, attempt := range []struct {
		path, actor string
		body        any
	}{
		{"/order-lines/" + pending + "/mark-fulfilled", e.clubToken, map[string]any{"note": "Synthetic retry"}},
		{"/order-lines/" + marked + "/confirm", e.buyer, nil},
	} {
		out := e.post(attempt.path, attempt.actor, attempt.body, http.StatusConflict)
		if out["code"] != "orders.invalid_state" {
			t.Fatalf("delivery denial: %v", out)
		}
	}
	if n := e.count(`SELECT count(*) FROM order_lines WHERE (id=$1 AND fulfillment_status='pending' OR id=$2 AND fulfillment_status='marked') AND fulfilled_at IS NULL AND refunded_amount=payable_amount`, pending, marked); n != 2 {
		t.Fatalf("denied delivery changed refunded lines: %d", n)
	}
	e.orderState(p, "partially_refunded", 27000, 24000)
	e.balanced("fully refunded delivery denials")
}
