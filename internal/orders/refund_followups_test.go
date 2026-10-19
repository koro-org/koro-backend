package orders_test

import (
	"testing"
	"time"
	"uuid"

	"koro/internal/catalog"
	"koro/internal/orders"
	"koro/internal/platform/domainevent"
)

// T-309: a fully refunded line must not be delivered merely because another
// line keeps the order in the otherwise deliverable partially_refunded state.
func TestRefundFollowups_FullyRefundedLineCannotBeDelivered(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"mark", "confirm", "admin", "auto"} {
		t.Run(action, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			v := e.place(t, line(e.offering(t, nil), 1), line(e.offering(t, nil), 1))[0]
			e.pay(t, v.Order.ID)
			target := v.Lines[0]
			buyer := actor(t, "company_admin")
			if action != "mark" {
				e.mark(t, target.ID)
			}
			if action == "admin" {
				if _, err := e.svc.DisputeLine(ctxOf(t, buyer), buyer, target.ID, "Synthetic missing delivery"); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.mod.Orders.ApplyRefund(t.Context(), v.Order.ID, &target.ID, target.PayableAmount); err != nil {
				t.Fatal(err)
			}
			if o := e.order(t, v.Order.ID); o.Status != orders.StatusPartiallyRefunded {
				t.Fatalf("order must remain partially refunded: %s", o.Status)
			}
			before := e.line(t, target.ID)
			var err error
			switch action {
			case "mark":
				seller := actor(t, "club_admin")
				_, err = e.svc.MarkLineFulfilled(ctxOf(t, seller), seller, target.ID, orders.MarkInput{})
			case "confirm":
				_, err = e.svc.ConfirmLine(ctxOf(t, buyer), buyer, target.ID)
			case "admin":
				fin := actor(t, "finance_admin")
				_, err = e.svc.AdminResolveDispute(ctxOf(t, fin), fin, target.ID, orders.ResolveInput{Outcome: orders.OutcomeFulfilled, Note: "Synthetic review"})
			case "auto":
				e.clk.Advance(8 * 24 * time.Hour)
				err = e.job(t, orders.CloseWindowsJobName).Handle(t.Context())
			}
			if action == "auto" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				wantProblem(t, err, 409, "orders.invalid_state")
			}
			after := e.line(t, target.ID)
			if after.FulfillmentStatus != before.FulfillmentStatus || after.FulfilledAt != nil {
				t.Fatalf("refunded line delivered: %+v", after)
			}
			if n := len(e.events(domainevent.TopicOrderLineFulfilled)); n != 0 {
				t.Fatalf("unexpected fulfilled events: %d", n)
			}
		})
	}
}

func TestRefundFollowups_PartialRefundStillDeliverableAndPayable(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	l := e.paidLine(t)
	if err := e.mod.Orders.ApplyRefund(t.Context(), l.OrderID, &l.ID, 100_001); err != nil {
		t.Fatal(err)
	}
	e.mark(t, l.ID)
	buyer := actor(t, "company_admin")
	if _, err := e.svc.ConfirmLine(ctxOf(t, buyer), buyer, l.ID); err != nil {
		t.Fatal(err)
	}
	payable, err := e.mod.Orders.PayableLines(t.Context(), e.payeeA, e.clk.Now().AddDate(0, 0, 30))
	if err != nil || len(payable) != 1 || payable[0].LineID != l.ID || payable[0].SellerNetAmount != l.SellerNetAmount {
		t.Fatalf("partial refund payable original snapshot: %+v, %v", payable, err)
	}
	// Ledger deducts completed refunded shares; orders retains its frozen amount.
	if open, err := e.mod.OpenCommerce(t.Context(), org("club_a")); err != nil || !open {
		t.Fatalf("seller with remainder can close: %v %v", open, err)
	}
	if err := e.mod.Orders.MarkLinesPaidOut(t.Context(), []uuid.UUID{l.ID}, uuid.NewV7()); err != nil {
		t.Fatal(err)
	}
	payable, err = e.mod.Orders.PayableLines(t.Context(), e.payeeA, e.clk.Now().AddDate(0, 0, 30))
	if err != nil || len(payable) != 0 {
		t.Fatalf("line paid out twice: %+v %v", payable, err)
	}
}

// A zero-fee C order has paid_amount=refunded_amount=0 after direct confirmation;
// that equality cannot be mistaken for a refunded service (T-314 remains open).
func TestRefundFollowups_ZeroFeeModelCStillDeliverable(t *testing.T) {
	t.Parallel()
	for _, automatic := range []bool{false, true} {
		t.Run(map[bool]string{false: "buyer", true: "auto"}[automatic], func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			off := e.offering(t, nil)
			e.addPolicy(t, catalog.ScopeOffering, &off, func(p *catalog.CommercePolicy) { modelCPolicy(p); p.CommissionRateBP = 0 })
			v := e.place(t, line(off, 1))[0]
			if eff, err := e.mod.Orders.ConfirmDirectPayment(t.Context(), v.Order.ID, uuid.NewV7()); err != nil || !eff.Paid {
				t.Fatalf("direct confirmation: %+v %v", eff, err)
			}
			e.mark(t, v.Lines[0].ID)
			if automatic {
				e.clk.Advance(8 * 24 * time.Hour)
				if err := e.job(t, orders.CloseWindowsJobName).Handle(t.Context()); err != nil {
					t.Fatal(err)
				}
			} else {
				buyer := actor(t, "company_admin")
				if _, err := e.svc.ConfirmLine(ctxOf(t, buyer), buyer, v.Lines[0].ID); err != nil {
					t.Fatal(err)
				}
			}
			if l := e.line(t, v.Lines[0].ID); l.FulfillmentStatus != orders.FulfillFulfilled {
				t.Fatalf("zero-fee C blocked: %+v", l)
			}
			if payable, err := e.mod.Orders.PayableLines(t.Context(), e.payeeA, e.clk.Now().AddDate(0, 0, 30)); err != nil || len(payable) != 0 {
				t.Fatalf("C became Koro payout: %+v %v", payable, err)
			}
		})
	}
}
