package payments_test

import (
	"testing"
	"uuid"

	"koro/internal/payments"
)

// TestOpenCommerce_RefundsInFlight (BE-135, T-382): closeOrg's payments
// check holds a buyer while refund money is in flight on one of its orders,
// whatever the order's status — a refund request waiting (open, in review)
// or a refund open (requested, approved) — and lets it go once the request
// is withdrawn and the refund completed or rejected. Another company is not
// held, nor is a buyer by a request whose order orders does not hold.
func TestOpenCommerce_RefundsInFlight(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	po := e.paid(t, e.activeAccount(t, ibanKoro, true), orderSpec{platformLine: true}, 0)
	buyer := id("company_a")
	open := func(step string, want bool) {
		t.Helper()
		got, err := e.mod.OpenCommerce(t.Context(), buyer)
		if err != nil || got != want {
			t.Fatalf("%s: buyer open = %v, %v; want %v", step, got, err, want)
		}
	}
	open("a paid order alone (orders' own check)", false)
	req, err := e.askRefund(t, "company_admin", po.order.ID, po.lines[0].ID, 10_000, nil)
	if err != nil {
		t.Fatal(err)
	}
	open("a waiting request", true)
	fin, cma := actor(t, "finance_admin"), actor(t, "company_admin")
	if _, err := e.svc.ReviewRefundRequest(ctxOf(t, fin), fin, req.Request.ID); err != nil {
		t.Fatal(err)
	}
	open("a request in review", true)
	if got, err := e.mod.OpenCommerce(t.Context(), id("company_b")); err != nil || got {
		t.Fatalf("another company open = %v, %v", got, err)
	}
	if _, err := e.svc.WithdrawRefundRequest(ctxOf(t, cma), cma, req.Request.ID); err != nil {
		t.Fatal(err)
	}
	open("the request withdrawn", false)

	r, err := e.request(t, "finance_admin_2", lineRefund(po.payment.ID, po.lines[0].ID, 10_000))
	if err != nil {
		t.Fatal(err)
	}
	open("a requested refund", true)
	if _, err := e.approve(t, "finance_admin", r.ID); err != nil {
		t.Fatal(err)
	}
	open("an approved refund", true)
	if _, err := e.completeAs(t, "finance_admin", r.ID); err != nil {
		t.Fatal(err)
	}
	open("the refund completed", false)
	r2, err := e.request(t, "finance_admin_2", lineRefund(po.payment.ID, po.lines[0].ID, 10_000))
	if err != nil {
		t.Fatal(err)
	}
	open("another requested refund", true)
	if _, err := e.svc.RejectRefund(ctxOf(t, fin), fin, r2.ID, "Tutar hatalı"); err != nil {
		t.Fatal(err)
	}
	open("the refund rejected", false)

	// The buyer is the order's own (06 H-38): a request naming company_b on
	// an order orders does not hold keeps nobody.
	ghost := payments.RefundRequest{ID: uuid.NewV7(), OrderID: uuid.NewV7(), OrderLineID: uuid.NewV7(), OrgID: id("company_b"),
		SellerOrgID: id("club_a"), OrderNo: "KR00000000", LineTitle: "Stant", Amount: 1_000, Reason: payments.RefundReasonNotFulfilled,
		Explanation: explanation, TargetIBAN: ibanSender, Status: payments.RequestOpen, RequestedBy: id("other_company_member")}
	if err := e.mem.SeedRefundRequest(ghost); err != nil {
		t.Fatal(err)
	}
	if ids, err := e.store.RefundsInFlightOf(t.Context(), id("company_b")); err != nil || len(ids) != 1 {
		t.Fatalf("company_b in flight = %v, %v", ids, err)
	}
	if got, err := e.mod.OpenCommerce(t.Context(), id("company_b")); err != nil || got {
		t.Fatalf("a request of an unknown order: open = %v, %v", got, err)
	}
}
