//go:build integration

package main

import (
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"testing"
	"uuid"
)

// cartPayee adds a second verified payee (another synthetic foundation) of
// the fixture's club: the cart then pays two payees in one order.
func (e *commerceEnv) cartPayee() string {
	e.t.Helper()
	party, payee := uuid.NewV7().String(), uuid.NewV7().String()
	e.exec(`INSERT INTO legal_parties (id,kind,legal_type,title,tax_office,tax_no,address,email,status,verified_at,verified_by,created_by)
 VALUES ($1,'payee','foundation','Synthetic Second Foundation','Test',$2,'{"line1":"Test street","city":"Ankara","country":"TR"}',$3,'verified',now(),$4,$5)`,
		party, fmt.Sprintf("97%08d", rand.IntN(100_000_000)), seed2Email, e.finBID, e.adminID)
	e.exec(`INSERT INTO payees (id,legal_party_id,org_id,status,invoicing_capability,verified_at,verified_by,created_by)
 VALUES ($1,$2,$3,'active','can_issue',now(),$4,$5)`, payee, party, e.club, e.finBID, e.adminID)
	// Its approved account (an offering goes on sale only with one): a real
	// stored PDF, distinct submitter and approver, as in legalFixtures.
	bank := uuid.NewV7().String()
	e.exec(`INSERT INTO payee_bank_accounts (id,payee_id,org_id,iban,holder_name,evidence_file_id,status,submitted_by,submitted_by_admin)
 VALUES ($1,$2,$3,$4,'Synthetic Second Foundation',$5,'pending_approval',$6,true)`, bank, payee, e.club, cartPayeeIBAN, e.upload(e.finA, "bank_account_evidence"), e.finAID)
	e.exec(`UPDATE payee_bank_accounts SET status='approved',approved_by=$2,approved_at=now() WHERE id=$1`, bank, e.finBID)
	return payee
}

// cartPayeeIBAN is a synthetic checksum vector (ibantest; bank code 99999
// does not exist), distinct from the fixture's other accounts.
const cartPayeeIBAN = "TR729999970000000000000099"

// seed2Email is the synthetic invoice address of the second payee.
const seed2Email = "second.foundation@example.test"

// cartOffering is an approved offering of the club paid to payee, of a
// reviewed service with the VAT rate, invoiced on trigger, under its own
// model A policy (10 % from the seller).
func (e *commerceEnv) cartOffering(title string, vatBP int, payee, trigger string) string {
	e.t.Helper()
	body := map[string]any{"code": "cart_" + strings.ReplaceAll(uuid.NewV7().String(), "-", "")[:24], "name": title, "category": "booth",
		"fulfillment_type": "org_bound", "default_vat_rate_bp": vatBP, "accounting_reviewed": true, "accounting_reviewed_ref": "SYNTHETIC-TEST"}
	service := e.post("/admin/services", e.finA, body, http.StatusCreated)["service_id"].(string)
	off := e.post("/orgs/"+e.club+"/offerings", e.clubToken, map[string]any{"service_id": service, "title": title, "unit_price_amount": 10000,
		"quantity_total": 5, "deliverables": []any{map[string]any{"key": "test_package", "label": "Synthetic package"}},
		"invoice_trigger": trigger, "default_payee_id": payee}, http.StatusCreated)["offering_id"].(string)
	e.exec(`INSERT INTO commerce_policies (scope,scope_id,model_code,collection_method,commission_rate_bp,commission_basis,commission_payer,valid_from,created_by)
 VALUES ('offering',$1,'A','bank_transfer',1000,'net_excl_vat','seller',now()-interval '1 day',$2)`, off, e.adminID)
	e.post("/offerings/"+off+"/submit", e.clubToken, nil, http.StatusOK)
	if out := e.post("/admin/offerings/"+off+"/decision", e.admin, map[string]any{"decision": "approved"}, http.StatusOK); out["status"] != "active" {
		e.t.Fatalf("approval: %v", out)
	}
	return off
}

// cartBalances checks the ledger by account and by payee: Koro's bank,
// each payee's payable, the commission and the platform sales; the VAT
// account takes the rest (the ledger sums to zero at every step).
func (e *commerceEnv) cartBalances(bank int64, payees map[string]int64, commission, sales int64) {
	e.t.Helper()
	e.balanced("cart balances")
	var payable int64
	for payee, want := range payees {
		payable += want
		if n := e.count(`SELECT -coalesce(sum(amount),0) FROM ledger_entries WHERE account_code='seller_payable' AND payee_id=$1`, payee); n != want {
			e.t.Errorf("payee %s balance=%d, want %d", payee, n, want)
		}
	}
	for code, want := range map[string]int64{"koro_bank": bank, "buyer_advances": 0, "seller_payable": -payable,
		"platform_commission_revenue": -commission, "platform_sales_revenue": -sales, "vat_output": -(bank - payable - commission - sales)} {
		if n := e.count(`SELECT coalesce(sum(amount),0) FROM ledger_entries WHERE account_code=$1`, code); n != want {
			e.t.Errorf("%s balance=%d, want %d", code, n, want)
		}
	}
}

// TestCommerce_Cart_TwoPayeesMixedVAT_LineRefund_PayoutInvoices (Q-25,
// BE-131): a company's cart of a 20 % booth (×2, payee 1, invoiced on
// fulfilment), a 10 % workshop (payee 2, invoiced on payment) and Koro's
// own quota is one order with one reference code, paid by one transfer.
// The ledger, the invoices and the payouts follow each line: the
// workshop's offering is cancelled after payment and its line refunded in
// full under four eyes (its invoice cancelled, payee 2 back to zero), the
// booth is delivered, invoiced at 20 % and paid out to payee 1 alone, and
// the ledger balances at every step.
func TestCommerce_Cart_TwoPayeesMixedVAT_LineRefund_PayoutInvoices(t *testing.T) {
	t.Parallel()
	e := newCommerce(t)
	payee2 := e.cartPayee()
	booth := e.cartOffering("Synthetic booth", 2000, e.payee, "on_fulfillment")
	workshop := e.cartOffering("Synthetic workshop", 1000, payee2, "on_payment")
	native := e.offering(true)
	cart := map[string]any{"buyer_party_id": e.party, "lines": []any{
		map[string]any{"offering_id": booth, "quantity": 2}, map[string]any{"offering_id": workshop, "quantity": 1},
		map[string]any{"offering_id": native, "quantity": 1},
	}}
	// BE-136: the cart's price preview writes nothing and is what createOrder places.
	quote := e.quote(cart, booth, workshop, native)
	out := e.post("/orgs/"+e.company+"/orders", e.buyer, cart, http.StatusCreated)
	e.quoteMatches(quote, out)
	if n := len(out["orders"].([]any)); n != 1 {
		t.Fatalf("cart split into %d orders, want one (all model A)", n)
	}
	o := out["orders"].([]any)[0].(map[string]any)
	// 2 × 10,000 + 20 % = 24,000; 10,000 + 10 % = 11,000; 2,500 + 20 % = 3,000.
	p := commerceOrder{id: o["order_id"].(string), no: o["order_no"].(string), amount: int64(o["payable_amount"].(float64))}
	if p.amount != 38000 || o["subtotal_amount"] != float64(32500) || o["vat_amount"] != float64(5500) {
		t.Fatalf("cart order: %v", o)
	}
	lineOf := map[string]string{}
	for _, v := range o["lines"].([]any) {
		l := v.(map[string]any)
		lineOf[l["offering_id"].(string)] = l["line_id"].(string)
		for _, field := range []string{"payee_id", "seller_net_amount", "commission_amount"} {
			if _, ok := l[field]; ok {
				t.Fatalf("buyer line leaked %s", field)
			}
		}
	}
	p.booth, p.native = lineOf[booth], lineOf[native]
	workshopLine := lineOf[workshop]
	if instr := e.get("/orders/"+p.id+"/payment-instructions", e.buyer); instr["reference_code"] != p.no || instr["amount_due_amount"] != float64(38000) {
		t.Fatalf("one transfer for the cart: %v", instr)
	}
	// The seller sees its two lines with its net shares, never the platform's line or the totals.
	seller := e.get("/orders/"+p.id, e.clubToken)
	if seller["view"] != "seller" || len(seller["lines"].([]any)) != 2 || seller["payable_amount"] != nil {
		t.Fatalf("seller view: %v", seller)
	}
	e.cartBalances(0, map[string]int64{e.payee: 0, payee2: 0}, 0, 0)

	e.match(p, 38000)
	e.orderState(p, "in_fulfilment", 38000, 0)
	e.entitlement(p, "active", 0)
	// Payee 1: 24,000 − 2,000 − 400; payee 2: 11,000 − 1,000 − 200.
	e.cartBalances(38000, map[string]int64{e.payee: 21600, payee2: 9800}, 3000, 2500)
	waitUntil(t, "on_payment invoices of the cart", func() bool {
		return e.count(`SELECT count(*) FROM invoices WHERE order_id=$1`, p.id) == 2
	})
	if n := e.count(`SELECT count(*) FROM invoices i JOIN invoice_lines il ON il.invoice_id=i.id
 WHERE i.order_id=$1 AND i.kind='service_sale' AND il.order_line_id=$2 AND il.vat_rate_bp=1000 AND i.net_amount=10000 AND i.vat_amount=1000
   AND i.issuer_party_id=(SELECT legal_party_id FROM payees WHERE id=$3)`, p.id, workshopLine, payee2); n != 1 {
		t.Fatal("the workshop's 10 % invoice from payee 2 is not planned")
	}

	e.post("/offerings/"+workshop+"/cancel", e.clubToken, map[string]any{"cancel_reason": "Synthetic cancellation"}, http.StatusOK)
	waitUntil(t, "paid cart line cancelled by outbox", func() bool {
		return e.count(`SELECT count(*) FROM order_lines WHERE id=$1 AND fulfillment_status='cancelled'`, workshopLine) == 1
	})
	if n := e.count(`SELECT count(*) FROM order_lines WHERE order_id=$1 AND fulfillment_status='cancelled'`, p.id); n != 1 {
		t.Fatalf("cancelled lines=%d, want only the workshop's", n)
	}
	var payment string
	e.q(`SELECT id FROM payments WHERE order_id=$1 AND status='captured'`, &payment, p.id)
	r := e.post("/admin/refunds", e.finA, map[string]any{"payment_id": payment, "order_line_id": workshopLine, "amount": 11000,
		"reason": "offering_cancelled", "target_iban": commerceIBAN}, http.StatusCreated)
	e.refund(r["refund_id"].(string))
	e.orderState(p, "partially_refunded", 38000, 11000)
	e.cartBalances(27000, map[string]int64{e.payee: 21600, payee2: 0}, 2000, 2500)
	waitUntil(t, "the refunded line's invoice cancelled", func() bool {
		return e.count(`SELECT count(*) FROM invoices i JOIN invoice_lines il ON il.invoice_id=i.id
 WHERE il.order_line_id=$1 AND i.status='cancelled'`, workshopLine) == 1
	})

	e.post("/order-lines/"+p.booth+"/mark-fulfilled", e.clubToken, map[string]any{"note": "Synthetic delivery"}, http.StatusOK)
	e.post("/order-lines/"+p.booth+"/confirm", e.buyer, nil, http.StatusOK)
	e.event("order_line.fulfilled", p.booth)
	e.orderState(p, "partially_refunded", 38000, 11000)
	waitUntil(t, "the booth's 20 % invoice", func() bool {
		return e.count(`SELECT count(*) FROM invoices i JOIN invoice_lines il ON il.invoice_id=i.id
 WHERE il.order_line_id=$1 AND i.kind='service_sale' AND i.status='planned' AND il.vat_rate_bp=2000 AND i.net_amount=20000 AND i.vat_amount=4000`, p.booth) == 1
	})
	if n := e.count(`SELECT count(*) FROM order_lines WHERE id=$1 AND fulfillment_status='fulfilled' AND fulfilled_via='auto_platform'`, p.native); n != 1 {
		t.Fatal("the paid cart's native line was not fulfilled")
	}

	payout := e.post("/admin/payouts", e.finA, map[string]any{"payee_id": e.payee, "order_line_ids": []string{p.booth}}, http.StatusCreated)
	pid := payout["payout_id"].(string)
	if payout["amount"] != float64(21600) {
		t.Fatalf("payout: %v", payout)
	}
	e.post("/admin/payouts/"+pid+"/approve", e.finB, map[string]any{"reauth": e.proof(e.finBTOTP)}, http.StatusOK)
	receipt := e.upload(e.finA, "payout_receipt")
	e.post("/admin/payouts/"+pid+"/mark-paid", e.finA, map[string]any{"bank_reference": "SYNTHETIC-CART-PAYOUT", "receipt_file_id": receipt}, http.StatusOK)
	e.event("payout.paid", pid)
	// Koro keeps its commission on the booth (2,000), its quota sale (2,500) and their VAT (400 + 500).
	e.cartBalances(5400, map[string]int64{e.payee: 0, payee2: 0}, 2000, 2500)
	waitUntil(t, "the commission invoice of the payout", func() bool {
		return e.count(`SELECT count(*) FROM invoices WHERE payout_id=$1 AND kind='commission' AND net_amount=2000 AND vat_amount=400`, pid) == 1
	})
	if n := e.count(`SELECT count(*) FROM payout_items WHERE order_line_id IN ($1, $2)`, workshopLine, p.native); n != 0 {
		t.Fatal("a refunded or platform line was paid out")
	}
}
