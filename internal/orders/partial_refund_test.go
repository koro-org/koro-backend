package orders_test

import (
	"testing"
	"time"
	"uuid"

	"koro/internal/catalog"
	"koro/internal/orders"
	"koro/internal/parties"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/problem/problemtest"
)

// T-382 (BE-135): a partial refund no longer holds an order open. Once every
// line is final — fulfilled, cancelled, or its service refunded in full — the
// order completes, its refunds stay in refunded_amount, and neither its buyer
// nor the seller of a refunded line is held by it.

// TestProgressStatus_FinalLines pins the running status of a paid order: a
// partially refunded order completes like any other once every line is
// final and one was fulfilled, and stays partially_refunded until then.
func TestProgressStatus_FinalLines(t *testing.T) {
	t.Parallel()
	l := func(f orders.Fulfillment, refunded int64) orders.Line {
		return orders.Line{FulfillmentStatus: f, PayableAmount: 1000, BuyerFeeAmount: 100, RefundedAmount: refunded}
	}
	ful, can, pen := orders.FulfillFulfilled, orders.FulfillCancelled, orders.FulfillPending
	mar, dis := orders.FulfillMarked, orders.FulfillDisputed
	a := orders.CollectBankTransfer
	for _, tc := range []struct {
		name   string
		method string
		from   orders.Status
		lines  []orders.Line
		want   orders.Status
	}{
		{"partly refunded, a line still to deliver", a, orders.StatusPartiallyRefunded, []orders.Line{l(ful, 0), l(pen, 0), l(can, 1000)}, orders.StatusPartiallyRefunded},
		{"partly refunded, a line marked", a, orders.StatusPartiallyRefunded, []orders.Line{l(ful, 0), l(mar, 0)}, orders.StatusPartiallyRefunded},
		{"partly refunded, a line disputed", a, orders.StatusPartiallyRefunded, []orders.Line{l(ful, 0), l(dis, 0)}, orders.StatusPartiallyRefunded},
		{"partly refunded, the rest delivered or cancelled", a, orders.StatusPartiallyRefunded, []orders.Line{l(ful, 0), l(can, 1000), l(can, 0)}, orders.StatusCompleted},
		{"partly refunded, a pending line refunded in full", a, orders.StatusPartiallyRefunded, []orders.Line{l(ful, 0), l(pen, 1000)}, orders.StatusCompleted},
		{"partly refunded, a disputed line refunded in full", a, orders.StatusPartiallyRefunded, []orders.Line{l(ful, 0), l(dis, 1000)}, orders.StatusCompleted},
		{"partly refunded, a pending line refunded in part", a, orders.StatusPartiallyRefunded, []orders.Line{l(ful, 0), l(pen, 999)}, orders.StatusPartiallyRefunded},
		{"model C: a pending line's fee refunded", orders.CollectDirectToPayee, orders.StatusPartiallyRefunded, []orders.Line{l(ful, 0), l(pen, 100)}, orders.StatusPartiallyRefunded},
		{"partly refunded, nothing delivered", a, orders.StatusPartiallyRefunded, []orders.Line{l(can, 1000), l(can, 0)}, orders.StatusPartiallyRefunded},
		{"completed and refunded in part", a, orders.StatusCompleted, []orders.Line{l(ful, 500), l(can, 1000)}, orders.StatusCompleted},
		{"paid, delivered and cancelled", a, orders.StatusPaid, []orders.Line{l(ful, 0), l(can, 0)}, orders.StatusCompleted},
		{"paid, a line marked", a, orders.StatusPaid, []orders.Line{l(ful, 0), l(mar, 0)}, orders.StatusInFulfilment},
		{"in fulfilment, a line disputed", a, orders.StatusInFulfilment, []orders.Line{l(ful, 0), l(dis, 0)}, orders.StatusDisputed},
		{"paid, nothing started", a, orders.StatusPaid, []orders.Line{l(pen, 0)}, orders.StatusPaid},
		{"refunded is kept", a, orders.StatusRefunded, []orders.Line{l(ful, 1000)}, orders.StatusRefunded},
		{"cancelled is kept", a, orders.StatusCancelled, []orders.Line{l(can, 0)}, orders.StatusCancelled},
	} {
		o := orders.Order{Status: tc.from, CollectionMethod: tc.method}
		if got := orders.ProgressStatus(o, tc.lines); got != tc.want {
			t.Errorf("%s: %s → %s, want %s", tc.name, tc.from, got, tc.want)
		}
	}
}

// TestPartialRefund_RestDelivered_CompletesAndFreesBuyer (T-382): a cart's
// line is cancelled after payment and refunded, the order is partially
// refunded and still holds its buyer and the other line's units; once the
// other line is delivered the order completes with the refund kept, its
// buyer may be closed and the units leave the per-buyer count. The other
// way round, an order that completed while a cancelled line waited for
// its refund stays completed (completed_at kept) when the refund arrives.
func TestPartialRefund_RestDelivered_CompletesAndFreesBuyer(t *testing.T) {
	t.Parallel()
	buyer, cma, club := org("company_a"), actor(t, "company_admin"), actor(t, "club_admin")
	setup := func(t *testing.T) (*env, orders.OrderView, orders.Line, orders.Line) {
		t.Helper()
		e := newEnv(t)
		kept, gone := e.offering(t, nil), e.offering(t, nil)
		v := e.place(t, line(kept, 1), line(gone, 1))[0]
		e.pay(t, v.Order.ID)
		if _, err := e.catalog.Service.CancelOffering(ctxOf(t, club), club, gone, "Etkinlik ertelendi"); err != nil {
			t.Fatal(err)
		}
		e.deliver(t, domainevent.TopicOfferingCancelled)
		return e, v, lineOf(t, v, kept), e.line(t, lineOf(t, v, gone).ID)
	}
	open := func(t *testing.T, e *env, want bool, step string) {
		t.Helper()
		if got, err := e.mod.OpenCommerce(t.Context(), buyer); err != nil || got != want {
			t.Fatalf("%s: buyer open = %v, %v; want %v", step, got, err, want)
		}
	}
	deliver := func(t *testing.T, e *env, id uuid.UUID) {
		t.Helper()
		e.mark(t, id)
		if _, err := e.svc.ConfirmLine(ctxOf(t, cma), cma, id); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("refund, then the rest delivered", func(t *testing.T) {
		t.Parallel()
		e, v, kept, gone := setup(t)
		if err := e.mod.Orders.ApplyRefund(t.Context(), v.Order.ID, &gone.ID, gone.PayableAmount); err != nil {
			t.Fatal(err)
		}
		if o := e.order(t, v.Order.ID); o.Status != orders.StatusPartiallyRefunded || o.CompletedAt != nil {
			t.Fatalf("order after the refund = %s (completed %v), want partially_refunded", o.Status, o.CompletedAt)
		}
		open(t, e, true, "a line still to deliver")
		if n, err := e.mod.BuyerHeld(t.Context(), kept.OfferingID, buyer); err != nil || n != 1 {
			t.Fatalf("units held while the order runs = %d, %v", n, err)
		}
		deliver(t, e, kept.ID)
		o := e.order(t, v.Order.ID)
		if o.Status != orders.StatusCompleted || o.CompletedAt == nil || !o.CompletedAt.Equal(e.clk.Now()) ||
			o.RefundedAmount != gone.PayableAmount || o.PaidAmount != v.Order.PayableAmount {
			t.Fatalf("order after the delivery = %+v, want completed with its refund", o)
		}
		if got := e.line(t, gone.ID); got.RefundedAmount != gone.PayableAmount || got.FulfillmentStatus != orders.FulfillCancelled {
			t.Fatalf("refunded line = %+v", got)
		}
		open(t, e, false, "every line final")
		if n, err := e.mod.BuyerHeld(t.Context(), kept.OfferingID, buyer); err != nil || n != 0 {
			t.Fatalf("units held by the completed order = %d, %v", n, err)
		}
		if due, err := e.mod.Orders.RefundsDue(t.Context(), 10); err != nil || len(due) != 0 {
			t.Fatalf("refunds due = %+v, %v", due, err)
		}
	})

	t.Run("delivered, then the refund", func(t *testing.T) {
		t.Parallel()
		e, v, kept, gone := setup(t)
		deliver(t, e, kept.ID)
		done := e.order(t, v.Order.ID)
		if done.Status != orders.StatusCompleted || done.CompletedAt == nil {
			t.Fatalf("order = %s, want completed", done.Status)
		}
		open(t, e, true, "a cancelled line's refund due")
		e.clk.Advance(6 * time.Hour)
		if err := e.mod.Orders.ApplyRefund(t.Context(), v.Order.ID, &gone.ID, gone.PayableAmount); err != nil {
			t.Fatal(err)
		}
		o := e.order(t, v.Order.ID)
		if o.Status != orders.StatusCompleted || !o.CompletedAt.Equal(*done.CompletedAt) || o.RefundedAmount != gone.PayableAmount {
			t.Fatalf("order after the refund = %+v, want still completed at %v", o, done.CompletedAt)
		}
		open(t, e, false, "refunded")
	})
}

// TestPartialRefund_FullyRefundedLine_IsFinal (T-309, T-382): a line whose
// service was refunded in full while still pending is final: once the other
// line is delivered the order completes, the refunded line is still never
// delivered, and nothing holds its seller (another club) or the buyer.
func TestPartialRefund_FullyRefundedLine_IsFinal(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	payeeB := e.payees.add("club_b", parties.PayeeActive)
	kept := e.offering(t, nil)
	other := e.offering(t, func(o *catalog.Offering) { o.OrgID, o.DefaultPayeeID = org("club_b"), &payeeB })
	v := e.place(t, line(kept, 1), line(other, 1))[0]
	e.pay(t, v.Order.ID)
	refunded := lineOf(t, v, other)
	if err := e.mod.Orders.ApplyRefund(t.Context(), v.Order.ID, &refunded.ID, refunded.PayableAmount); err != nil {
		t.Fatal(err)
	}
	if o := e.order(t, v.Order.ID); o.Status != orders.StatusPartiallyRefunded {
		t.Fatalf("order = %s, want partially_refunded (a line still to deliver)", o.Status)
	}
	k := lineOf(t, v, kept)
	e.mark(t, k.ID)
	cma := actor(t, "company_admin")
	if _, err := e.svc.ConfirmLine(ctxOf(t, cma), cma, k.ID); err != nil {
		t.Fatal(err)
	}
	if o := e.order(t, v.Order.ID); o.Status != orders.StatusCompleted || o.RefundedAmount != refunded.PayableAmount {
		t.Fatalf("order = %+v, want completed with the refund", o)
	}
	clubB := actor(t, "other_club_admin")
	_, err := e.svc.MarkLineFulfilled(ctxOf(t, clubB), clubB, refunded.ID, orders.MarkInput{})
	problemtest.WantProblem(t, err, 409, "orders.invalid_state")
	if got := e.line(t, refunded.ID); got.FulfillmentStatus != orders.FulfillPending || got.FulfilledAt != nil {
		t.Fatalf("refunded line = %+v", got)
	}
	for name, want := range map[string]bool{"company_a": false, "club_b": false, "club_a": true} { // club_a: its line waits for a payout
		if got, err := e.mod.OpenCommerce(t.Context(), org(name)); err != nil || got != want {
			t.Errorf("%s open = %v, %v; want %v", name, got, err, want)
		}
	}
}
