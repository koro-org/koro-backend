package orders_test

import (
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/catalog"
	"koro/internal/entitlements"
	"koro/internal/notify"
	"koro/internal/orders"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
	"koro/internal/platform/problem/problemtest"
	"koro/internal/platform/queue"
)

// job returns the scheduled job called name.
func (e *env) job(t *testing.T, name string) queue.Job {
	t.Helper()
	i := slices.IndexFunc(e.mod.Jobs(), func(j queue.Job) bool { return j.Name == name })
	if i < 0 {
		t.Fatalf("job %s is not declared", name)
	}
	return e.mod.Jobs()[i]
}

// deliver runs the orders subscriptions of topic on every event of it the
// outbox holds (the worker).
func (e *env) deliver(t *testing.T, topic string) {
	t.Helper()
	r := outbox.NewRegistry()
	e.mod.Subscribe(r)
	for _, ev := range e.events(topic) {
		for _, s := range r.For(topic) {
			if err := s.Handle(t.Context(), ev); err != nil {
				t.Fatalf("%s: %v", s.Name, err)
			}
		}
	}
}

// paidLine places one line of a fresh offering, pays the order and
// returns the line.
func (e *env) paidLine(t *testing.T) orders.Line {
	t.Helper()
	v := e.placeOne(t)
	e.pay(t, v.Order.ID)
	return v.Lines[0]
}

func (e *env) mark(t *testing.T, lineID uuid.UUID) orders.LineView {
	t.Helper()
	a := actor(t, "club_admin")
	v, err := e.svc.MarkLineFulfilled(ctxOf(t, a), a, lineID, orders.MarkInput{Note: "Stant kuruldu"})
	if err != nil {
		t.Fatalf("MarkLineFulfilled: %v", err)
	}
	return v
}

// TestMarkFulfilled_UnpaidOrder_409 (04 §2.15 "Sipariş paid olmalı"): the
// seller cannot mark a line of an unpaid order; a paid one is marked, its
// dispute window opens for the policy's days, the order is in fulfilment
// and order_line.fulfillment_marked (A) asks the buyer to confirm; marking
// again is 409; the buyer cannot mark.
func TestMarkFulfilled_UnpaidOrder_409(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	v := e.placeOne(t)
	club := actor(t, "club_admin")
	_, err := e.svc.MarkLineFulfilled(ctxOf(t, club), club, v.Lines[0].ID, orders.MarkInput{})
	problemtest.WantErr(t, err, orders.ErrOrderNotPaid)
	problemtest.WantProblem(t, err, 409, "orders.order_not_paid")
	e.pay(t, v.Order.ID)
	buyer := actor(t, "company_admin")
	_, err = e.svc.MarkLineFulfilled(ctxOf(t, buyer), buyer, v.Lines[0].ID, orders.MarkInput{})
	problemtest.WantErr(t, err, authz.ErrForbidden)
	got := e.mark(t, v.Lines[0].ID)
	l := got.Line
	if l.FulfillmentStatus != orders.FulfillMarked || l.FulfillmentMarkedBy == nil || *l.FulfillmentMarkedBy != club.UserID ||
		l.DisputeWindowEndsAt == nil || !l.DisputeWindowEndsAt.Equal(t0.AddDate(0, 0, 7)) || got.OrderStatus != orders.StatusInFulfilment {
		t.Fatalf("marked = %+v (%s)", l, got.OrderStatus)
	}
	if evs := e.events(domainevent.TopicOrderLineFulfillmentMarked); len(evs) != 1 {
		t.Fatalf("fulfillment_marked events = %d", len(evs))
	}
	if ev := e.audit.Of(authz.OrdersLineMarkFulfilled); len(ev) != 1 || ev[0].Reason != "Stant kuruldu" {
		t.Fatalf("audit = %+v", ev)
	}
	_, err = e.svc.MarkLineFulfilled(ctxOf(t, club), club, v.Lines[0].ID, orders.MarkInput{})
	problemtest.WantProblem(t, err, 409, "orders.invalid_state")
	other := actor(t, "other_club_admin")
	_, err = e.svc.MarkLineFulfilled(ctxOf(t, other), other, v.Lines[0].ID, orders.MarkInput{})
	problemtest.WantErr(t, err, authz.ErrNotFound)
}

// TestConfirmLine_CompletesOrder: the buyer's admin confirms a marked line
// (buyer_confirmed), order_line.fulfilled (O) is written and the order is
// completed; a line that is not marked cannot be confirmed.
func TestConfirmLine_CompletesOrder(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	l := e.paidLine(t)
	a := actor(t, "company_admin")
	_, err := e.svc.ConfirmLine(ctxOf(t, a), a, l.ID)
	problemtest.WantProblem(t, err, 409, "orders.invalid_state")
	e.mark(t, l.ID)
	v, err := e.svc.ConfirmLine(ctxOf(t, a), a, l.ID)
	if err != nil || v.Line.FulfillmentStatus != orders.FulfillFulfilled || *v.Line.FulfilledVia != orders.ViaBuyerConfirmed ||
		v.OrderStatus != orders.StatusCompleted {
		t.Fatalf("confirmed = %+v (%s), %v", v.Line, v.OrderStatus, err)
	}
	evs := e.events(domainevent.TopicOrderLineFulfilled)
	if len(evs) != 1 {
		t.Fatalf("order_line.fulfilled events = %d", len(evs))
	}
	if p, _ := domainevent.Decode[domainevent.OrderLineFulfilled](evs[0]); p.PayeeID != e.payeeA || p.OrderLineID != l.ID {
		t.Fatalf("fulfilled payload = %+v", p)
	}
	rec := actor(t, "company_member")
	_, err = e.svc.ConfirmLine(ctxOf(t, rec), rec, l.ID)
	problemtest.WantErr(t, err, authz.ErrForbidden)
}

// TestDispute_OutsideWindow_409 (04 §2.15 "Pencere dışı 409"): inside its
// window the buyer disputes a marked line (the order is disputed); after
// the window it is 409 orders.dispute_window_closed; a dispute needs a
// reason.
func TestDispute_OutsideWindow_409(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	inside, outside := e.paidLine(t), e.paidLine(t)
	e.mark(t, inside.ID)
	e.mark(t, outside.ID)
	a := actor(t, "company_admin")
	_, err := e.svc.DisputeLine(ctxOf(t, a), a, inside.ID, "")
	problemtest.WantProblem(t, err, 422, "validation_failed")
	e.clk.Advance(6 * 24 * time.Hour)
	v, err := e.svc.DisputeLine(ctxOf(t, a), a, inside.ID, "Stant kurulmadı")
	if err != nil || v.Line.FulfillmentStatus != orders.FulfillDisputed || *v.Line.DisputeReason != "Stant kurulmadı" ||
		v.OrderStatus != orders.StatusDisputed {
		t.Fatalf("disputed = %+v (%s), %v", v.Line, v.OrderStatus, err)
	}
	e.clk.Advance(24 * time.Hour) // exactly the end of the window: closed
	_, err = e.svc.DisputeLine(ctxOf(t, a), a, outside.ID, "Geç kaldık")
	problemtest.WantErr(t, err, orders.ErrDisputeWindowClosed)
	problemtest.WantProblem(t, err, 409, "orders.dispute_window_closed")
}

// TestDisputeWindowClose_EmitsFulfilled (01 §7 orders.close_dispute_windows):
// the job fulfils the marked lines whose window passed (window_elapsed),
// writes order_line.fulfilled (O) and completes the order; a line still in
// its window, or disputed, stays; a second run changes nothing.
func TestDisputeWindowClose_EmitsFulfilled(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	due, disputed := e.paidLine(t), e.paidLine(t)
	e.mark(t, due.ID)
	e.mark(t, disputed.ID)
	a := actor(t, "company_admin")
	if _, err := e.svc.DisputeLine(ctxOf(t, a), a, disputed.ID, "Eksik teslim"); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(3 * 24 * time.Hour)
	young := e.paidLine(t)
	e.mark(t, young.ID)
	e.clk.Advance(4 * 24 * time.Hour) // due's window ends now; young's in three days
	job := e.job(t, orders.CloseWindowsJobName)
	if job.Cron != "55 * * * *" || job.Queue != queue.Default || job.Unique <= 0 {
		t.Fatalf("job = %+v", job)
	}
	if err := job.Handle(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := e.line(t, due.ID)
	if got.FulfillmentStatus != orders.FulfillFulfilled || *got.FulfilledVia != orders.ViaWindowElapsed {
		t.Fatalf("due line = %+v", got)
	}
	if o := e.order(t, due.OrderID); o.Status != orders.StatusCompleted {
		t.Fatalf("its order = %s, want completed", o.Status)
	}
	if s := e.line(t, young.ID).FulfillmentStatus; s != orders.FulfillMarked {
		t.Fatalf("young line = %s, want marked", s)
	}
	if s := e.line(t, disputed.ID).FulfillmentStatus; s != orders.FulfillDisputed {
		t.Fatalf("disputed line = %s", s)
	}
	evs := e.events(domainevent.TopicOrderLineFulfilled)
	if len(evs) != 1 || evs[0].AggregateID != due.ID {
		t.Fatalf("order_line.fulfilled events = %+v", evs)
	}
	if err := job.Handle(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := len(e.events(domainevent.TopicOrderLineFulfilled)); n != 1 {
		t.Fatalf("a second run wrote %d events", n)
	}
	if ev := e.audit.Of(authz.OrdersLineAutoFulfill); len(ev) != 1 || ev[0].SystemJob != orders.CloseWindowsJobName {
		t.Fatalf("audit = %+v", ev)
	}
}

// TestResolveDispute: a platform or finance admin resolves a disputed line:
// delivered (admin, order_line.fulfilled) or cancelled for a refund (its
// money waits in RefundsDue); the admin surface is the gate (403 for a
// company); a line that is not disputed is 409; refund_amount at most the
// refundable amount, and the whole of it (no partial refund, T-291).
func TestResolveDispute(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := actor(t, "company_admin")
	dispute := func() orders.Line {
		l := e.paidLine(t)
		e.mark(t, l.ID)
		if _, err := e.svc.DisputeLine(ctxOf(t, a), a, l.ID, "Teslim edilmedi"); err != nil {
			t.Fatal(err)
		}
		return l
	}
	pad, fin := actor(t, "platform_admin"), actor(t, "finance_admin")
	delivered := dispute()
	_, err := e.svc.AdminResolveDispute(ctxOf(t, a), a, delivered.ID, orders.ResolveInput{Outcome: orders.OutcomeFulfilled, Note: "Bakıldı"})
	problemtest.WantErr(t, err, authz.ErrForbidden)
	v, err := e.svc.AdminResolveDispute(ctxOf(t, pad), pad, delivered.ID, orders.ResolveInput{Outcome: orders.OutcomeFulfilled, Note: "Fotoğraflar teslimi gösteriyor"})
	if err != nil || v.Line.FulfillmentStatus != orders.FulfillFulfilled || *v.Line.FulfilledVia != orders.ViaAdmin || v.OrderStatus != orders.StatusCompleted {
		t.Fatalf("resolved delivered = %+v (%s), %v", v.Line, v.OrderStatus, err)
	}
	_, err = e.svc.AdminResolveDispute(ctxOf(t, pad), pad, delivered.ID, orders.ResolveInput{Outcome: orders.OutcomeFulfilled, Note: "Tekrar"})
	problemtest.WantProblem(t, err, 409, "orders.invalid_state")
	refund := dispute()
	_, err = e.svc.AdminResolveDispute(ctxOf(t, fin), fin, refund.ID, orders.ResolveInput{Outcome: orders.OutcomeRefund, Note: "İade", RefundAmount: ptr(int64(1_200_001))})
	problemtest.WantProblem(t, err, 422, "orders.refund_exceeds_paid")
	_, err = e.svc.AdminResolveDispute(ctxOf(t, fin), fin, refund.ID, orders.ResolveInput{Outcome: orders.OutcomeRefund, Note: "Yarısı", RefundAmount: ptr(int64(600_000))})
	problemtest.WantProblem(t, err, 422, "validation_failed")
	v, err = e.svc.AdminResolveDispute(ctxOf(t, fin), fin, refund.ID, orders.ResolveInput{Outcome: orders.OutcomeRefund, Note: "Stant yoktu", RefundAmount: ptr(int64(1_200_000))})
	if err != nil || v.Line.FulfillmentStatus != orders.FulfillCancelled || v.OrderStatus != orders.StatusPaid {
		t.Fatalf("resolved refund = %+v (%s), %v", v.Line, v.OrderStatus, err)
	}
	due, err := e.mod.Orders.RefundsDue(t.Context(), 10)
	if err != nil || len(due) != 1 || due[0].LineID == nil || *due[0].LineID != refund.ID || due[0].Amount != 1_200_000 {
		t.Fatalf("refunds due = %+v, %v", due, err)
	}
	if ev := e.audit.Of(authz.OrdersLineResolveDispute); len(ev) != 2 || ev[1].Metadata["refund_amount"] != int64(1_200_000) {
		t.Fatalf("audit = %+v", ev)
	}
	_, err = e.svc.AdminResolveDispute(ctxOf(t, pad), pad, uuid.NewV7(), orders.ResolveInput{Outcome: orders.OutcomeFulfilled, Note: "x"})
	problemtest.WantErr(t, err, orders.ErrNotFound)
}

// TestExpireUnpaid_ReleasesStockAndRevokesEntitlements (01 §7
// orders.expire_unpaid): an order unpaid at the end of its window expires:
// its units go back to the offerings, its pending rights are revoked, its
// lines are cancelled, order.expired (A) tells the buyer; a paid order and
// one still in its window stay; a second run is a no-op; money paid in
// part waits for a refund.
func TestExpireUnpaid_ReleasesStockAndRevokesEntitlements(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	booth, quota := e.offering(t, nil), e.platformOffering(t)
	stale := e.place(t, line(booth, 2), line(quota, 1))[0]
	partial := e.placeOne(t)
	if _, err := e.mod.Orders.ApplyPayment(t.Context(), partial.Order.ID, uuid.NewV7(), 100_000); err != nil {
		t.Fatal(err)
	}
	paid := e.placeOne(t)
	e.pay(t, paid.Order.ID)
	e.clk.Advance(orders.DefaultPaymentWindow - time.Hour)
	fresh := e.placeOne(t)
	e.clk.Advance(2 * time.Hour)
	job := e.job(t, orders.ExpireJobName)
	if job.Cron != "50 * * * *" || job.Queue != queue.Default || job.Unique <= 0 {
		t.Fatalf("job = %+v", job)
	}
	if err := job.Handle(t.Context()); err != nil {
		t.Fatal(err)
	}
	if o := e.order(t, stale.Order.ID); o.Status != orders.StatusExpired {
		t.Fatalf("stale order = %s, want expired", o.Status)
	}
	if n := e.sold(t, booth); n != 0 {
		t.Fatalf("units still sold = %d, want 0", n)
	}
	for _, l := range stale.Lines {
		if got := e.line(t, l.ID).FulfillmentStatus; got != orders.FulfillCancelled {
			t.Errorf("line %s = %s, want cancelled", l.ID, got)
		}
		if l.OfferingID == quota {
			if rs := e.rights(t, l.ID); len(rs) != 1 || rs[0].Status != entitlements.StatusRevoked {
				t.Errorf("right = %+v, want revoked", rs)
			}
		}
	}
	for name, id := range map[string]uuid.UUID{"paid": paid.Order.ID, "fresh": fresh.Order.ID} {
		if o := e.order(t, id); o.Status == orders.StatusExpired {
			t.Errorf("%s order expired", name)
		}
	}
	if o := e.order(t, partial.Order.ID); o.Status != orders.StatusExpired {
		t.Fatalf("partially paid order = %s", o.Status)
	}
	evs := e.events(domainevent.TopicOrderExpired)
	if len(evs) != 2 {
		t.Fatalf("order.expired events = %d, want 2", len(evs))
	}
	due, err := e.mod.Orders.RefundsDue(t.Context(), 0)
	if err != nil || len(due) != 1 || due[0].OrderID != partial.Order.ID || due[0].LineID != nil || due[0].Amount != 100_000 {
		t.Fatalf("refunds due = %+v, %v", due, err)
	}
	if err := job.Handle(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := len(e.events(domainevent.TopicOrderExpired)); n != 2 {
		t.Fatalf("a second run wrote %d events", n)
	}
	if ev := e.audit.Of(authz.OrdersOrderExpire); len(ev) != 2 || ev[0].SystemJob != orders.ExpireJobName {
		t.Fatalf("audit = %+v", ev)
	}
	// A transfer that arrives after expiry is not applied (payments refunds it).
	_, err = e.mod.Orders.ApplyPayment(t.Context(), stale.Order.ID, uuid.NewV7(), 1)
	problemtest.WantProblem(t, err, 409, "orders.invalid_state")
}

// TestOfferingCancelled_UnpaidCancelled_PaidQueuedForRefund (01 §6.2
// offering.cancelled ⇠ orders): when an offering is cancelled, an unpaid
// order of it alone is cancelled (units back, order.cancelled), an unpaid
// cart keeps its other line and drops only this one (Q-28c, BE-134;
// drop_test.go), and its undelivered lines on paid orders are cancelled —
// their money waits for a refund (RefundsDue; BE-043) — while delivered
// lines and other offerings' lines stay. Delivering the event twice
// changes nothing.
func TestOfferingCancelled_UnpaidCancelled_PaidQueuedForRefund(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	off, other := e.offering(t, nil), e.offering(t, nil)
	unpaid := e.place(t, line(off, 1))[0]
	cart := e.place(t, line(off, 1), line(other, 1))[0]
	paid := e.place(t, line(off, 1), line(other, 1))[0]
	e.pay(t, paid.Order.ID)
	delivered := e.place(t, line(off, 1))[0]
	e.pay(t, delivered.Order.ID)
	e.mark(t, delivered.Lines[0].ID)
	a := actor(t, "company_admin")
	if _, err := e.svc.ConfirmLine(ctxOf(t, a), a, delivered.Lines[0].ID); err != nil {
		t.Fatal(err)
	}
	club := actor(t, "club_admin")
	if _, err := e.catalog.Service.CancelOffering(ctxOf(t, club), club, off, "Etkinlik ertelendi"); err != nil {
		t.Fatalf("CancelOffering: %v", err)
	}
	e.deliver(t, domainevent.TopicOfferingCancelled)
	e.deliver(t, domainevent.TopicOfferingCancelled) // at least once: idempotent
	if o := e.order(t, unpaid.Order.ID); o.Status != orders.StatusCancelled || *o.CancelReason != orders.CancelOfferingClosed {
		t.Fatalf("unpaid order = %+v", o)
	}
	if o := e.order(t, cart.Order.ID); o.Status != orders.StatusAwaitingPayment || o.PayableAmount != 1_200_000 {
		t.Fatalf("unpaid cart = %+v, want awaiting payment of its other line", o)
	}
	if n := e.sold(t, other); n != 2 {
		t.Fatalf("units of the other offering sold = %d, want 2 (the cart's and the paid order's)", n)
	}
	if evs := e.events(domainevent.TopicOrderCancelled); len(evs) != 1 {
		t.Fatalf("order.cancelled events = %d, want 1", len(evs))
	}
	byOffering := map[uuid.UUID]orders.Line{}
	for _, l := range paid.Lines {
		byOffering[l.OfferingID] = e.line(t, l.ID)
	}
	if byOffering[off].FulfillmentStatus != orders.FulfillCancelled || byOffering[other].FulfillmentStatus != orders.FulfillPending {
		t.Fatalf("paid order lines = %s / %s", byOffering[off].FulfillmentStatus, byOffering[other].FulfillmentStatus)
	}
	if o := e.order(t, paid.Order.ID); o.Status != orders.StatusPaid {
		t.Fatalf("paid order = %s, want paid (the other line still to deliver)", o.Status)
	}
	if got := e.line(t, delivered.Lines[0].ID); got.FulfillmentStatus != orders.FulfillFulfilled {
		t.Fatalf("delivered line = %s", got.FulfillmentStatus)
	}
	due, err := e.mod.Orders.RefundsDue(t.Context(), 10)
	if err != nil || len(due) != 1 || *due[0].LineID != byOffering[off].ID || due[0].Amount != 1_200_000 || due[0].OrgID != org("company_a") {
		t.Fatalf("refunds due = %+v, %v", due, err)
	}
	// The refund completes (BE-043 → ApplyRefund): the line leaves the queue.
	cancelledID := byOffering[off].ID
	if err := e.mod.Orders.ApplyRefund(t.Context(), paid.Order.ID, &cancelledID, 1_200_000); err != nil {
		t.Fatal(err)
	}
	if due, _ := e.mod.Orders.RefundsDue(t.Context(), 10); len(due) != 0 {
		t.Fatalf("refunds due after the refund = %+v", due)
	}
	if o := e.order(t, paid.Order.ID); o.Status != orders.StatusPartiallyRefunded || o.RefundedAmount != 1_200_000 {
		t.Fatalf("refunded order = %+v", o)
	}
}

// TestApplyRefund_Limits: refunds never exceed what was paid (422
// orders.refund_exceeds_paid) nor a line's payable amount; a partial refund
// of the completed order (its platform line was fulfilled at payment) keeps
// it completed with the refund in refunded_amount (BE-135); a full refund
// makes the order refunded and revokes its rights.
func TestApplyRefund_Limits(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	v := e.place(t, line(e.platformOffering(t), 1))[0]
	e.pay(t, v.Order.ID)
	ord := e.mod.Orders
	problemtest.WantProblem(t, ord.ApplyRefund(t.Context(), v.Order.ID, nil, v.Order.PayableAmount+1), 422, "orders.refund_exceeds_paid")
	other := e.placeOne(t)
	problemtest.WantErr(t, ord.ApplyRefund(t.Context(), v.Order.ID, &other.Lines[0].ID, 1), orders.ErrNotFound)
	if err := ord.ApplyRefund(t.Context(), v.Order.ID, nil, 100); err != nil {
		t.Fatal(err)
	}
	if o := e.order(t, v.Order.ID); o.Status != orders.StatusCompleted || o.RefundedAmount != 100 || o.CompletedAt == nil {
		t.Fatalf("partly refunded completed order = %+v", o)
	}
	if err := ord.ApplyRefund(t.Context(), v.Order.ID, nil, v.Order.PayableAmount-100); err != nil {
		t.Fatal(err)
	}
	if o := e.order(t, v.Order.ID); o.Status != orders.StatusRefunded || o.RefundedAmount != o.PaidAmount {
		t.Fatalf("fully refunded order = %+v", o)
	}
	if rs := e.rights(t, v.Lines[0].ID); rs[0].Status != entitlements.StatusRevoked {
		t.Fatalf("right of a refunded order = %s", rs[0].Status)
	}
}

// TestPayableLines_AndPaidOut (01 §4.18; BE-044): a fulfilled line of a paid
// order is payable once its frozen payout hold passed; a payout takes each
// line once; an unfulfilled line cannot be paid out.
func TestPayableLines_AndPaidOut(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	off := e.offering(t, nil)
	e.addPolicy(t, catalog.ScopeOffering, &off, func(p *catalog.CommercePolicy) { p.PayoutHoldDays = 2 })
	v := e.place(t, line(off, 1))[0]
	e.pay(t, v.Order.ID)
	open := e.paidLine(t)
	l := v.Lines[0]
	e.mark(t, l.ID)
	a := actor(t, "company_admin")
	if _, err := e.svc.ConfirmLine(ctxOf(t, a), a, l.ID); err != nil {
		t.Fatal(err)
	}
	ord := e.mod.Orders
	if ls, err := ord.PayableLines(t.Context(), e.payeeA, e.clk.Now().Add(24*time.Hour)); err != nil || len(ls) != 0 {
		t.Fatalf("payable within the hold = %+v, %v", ls, err)
	}
	ls, err := ord.PayableLines(t.Context(), e.payeeA, e.clk.Now().Add(48*time.Hour))
	if err != nil || len(ls) != 1 || ls[0].LineID != l.ID || ls[0].SellerNetAmount != 1_080_000 || ls[0].OrderNo != v.Order.OrderNo {
		t.Fatalf("payable = %+v, %v", ls, err)
	}
	payout := uuid.NewV7()
	problemtest.WantErr(t, ord.MarkLinesPaidOut(t.Context(), []uuid.UUID{l.ID, open.ID}, payout), orders.ErrInvalidState)
	if err := ord.MarkLinesPaidOut(t.Context(), []uuid.UUID{l.ID}, payout); err != nil {
		t.Fatal(err)
	}
	if got := e.line(t, l.ID); got.PayoutID == nil || *got.PayoutID != payout || got.PaidOutAt == nil {
		t.Fatalf("paid out line = %+v", got)
	}
	problemtest.WantErr(t, ord.MarkLinesPaidOut(t.Context(), []uuid.UUID{l.ID}, uuid.NewV7()), orders.ErrInvalidState)
	if ls, _ := ord.PayableLines(t.Context(), e.payeeA, e.clk.Now().Add(48*time.Hour)); len(ls) != 0 {
		t.Fatalf("payable after the payout = %+v", ls)
	}
	if got, err := ord.GetLine(t.Context(), l.ID); err != nil || got.ID != l.ID {
		t.Fatalf("GetLine = %+v, %v", got, err)
	}
	// billing (BE-042): the order with its lines, and the lines of the payout.
	o, lines, err := ord.ForBilling(t.Context(), v.Order.ID)
	if err != nil || o.ID != v.Order.ID || len(lines) != 1 || lines[0].ID != l.ID {
		t.Fatalf("ForBilling = %+v, %d lines, %v", o, len(lines), err)
	}
	_, _, err = ord.ForBilling(t.Context(), uuid.NewV7())
	problemtest.WantErr(t, err, orders.ErrNotFound)
	if ls, err := ord.PayoutLines(t.Context(), payout); err != nil || len(ls) != 1 || ls[0].ID != l.ID {
		t.Fatalf("PayoutLines = %+v, %v", ls, err)
	}
}

// TestOpenCommerce_BlocksClose (hooks.openCommerce → orgs closeOrg): a
// company with an open order, and a club with a line to deliver or to pay
// out, cannot close; once delivered and paid out the club can.
func TestOpenCommerce_BlocksClose(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	open := func(name string) bool {
		t.Helper()
		ok, err := e.mod.OpenCommerce(t.Context(), org(name))
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if open("company_a") || open("club_a") {
		t.Fatal("open commerce without orders")
	}
	l := e.paidLine(t)
	if !open("company_a") || !open("club_a") || open("club_b") {
		t.Fatalf("paid, undelivered: company %v club %v club_b %v", open("company_a"), open("club_a"), open("club_b"))
	}
	e.mark(t, l.ID)
	a := actor(t, "company_admin")
	if _, err := e.svc.ConfirmLine(ctxOf(t, a), a, l.ID); err != nil {
		t.Fatal(err)
	}
	if open("company_a") {
		t.Error("a completed order keeps the company open")
	}
	if !open("club_a") {
		t.Error("a fulfilled line not paid out lets the club close")
	}
	if err := e.mod.Orders.MarkLinesPaidOut(t.Context(), []uuid.UUID{l.ID}, uuid.NewV7()); err != nil {
		t.Fatal(err)
	}
	if open("club_a") {
		t.Error("a paid out line keeps the club open")
	}
}

// TestNotices pins the worker notices of 01 §6.2: order.placed sends the
// placer the payment instructions (e-mail with Koro's account, the
// reference and the amount; in-app) and tells the seller's admins and the
// finance admins in-app; without a collection account the e-mail is left
// out; the other topics notify their recipients; every notice carries IDs
// only and a repeated task sends nothing new.
func TestNotices(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	v := e.placeOne(t)
	e.deliver(t, domainevent.TopicOrderPlaced)
	sent := e.sender.all()
	if slices.ContainsFunc(sent, func(m notify.Message) bool { return m.Channel == notify.ChannelEmail }) {
		t.Fatal("an instructions e-mail without a collection account")
	}
	e.acct = &orders.CollectionAccount{ID: uuid.NewV7(), IBAN: "TR330006100519786457841326", HolderName: "Koro Teknoloji A.Ş.", BankName: "Örnek Bank"}
	e.deliver(t, domainevent.TopicOrderPlaced)
	sent = e.sender.all()
	to := func(tmpl string, ch notify.Channel) []uuid.UUID {
		var out []uuid.UUID
		for _, m := range sent {
			if m.Template == tmpl && m.Channel == ch {
				out = append(out, *m.UserID)
			}
		}
		return out
	}
	mail := slices.IndexFunc(sent, func(m notify.Message) bool { return m.Channel == notify.ChannelEmail })
	if mail < 0 {
		t.Fatal("no instructions e-mail")
	}
	m := sent[mail]
	if m.Template != "orders.payment_instructions" || m.ToEmail != "company_admin@koro.test" || m.Data["reference_code"] != v.Order.OrderNo ||
		m.Data["amount"] != "12000.00" || m.Data["iban"] != e.acct.IBAN || !strings.Contains(m.Data["expires_at"].(string), "2026") {
		t.Fatalf("instructions e-mail = %+v", m)
	}
	if got := to("orders.payment_instructions", notify.ChannelInApp); !slices.Equal(got, []uuid.UUID{org("company_admin")}) {
		t.Errorf("instructions in-app to %v", got)
	}
	placed := to("orders.order_placed", notify.ChannelInApp)
	for _, who := range []string{"club_admin", "finance_admin", "finance_admin_2"} {
		if !slices.Contains(placed, org(who)) {
			t.Errorf("order_placed did not reach %s (%v)", who, placed)
		}
	}
	for _, m := range sent {
		if m.Channel == notify.ChannelInApp {
			if len(m.Data) != 1 || m.Data["order_id"] != v.Order.ID.String() {
				t.Errorf("in-app data = %v, want the order id only", m.Data)
			}
		}
	}
	n := len(sent)
	e.deliver(t, domainevent.TopicOrderPlaced)
	if len(e.sender.all()) != n {
		t.Fatal("a repeated task sent new notices")
	}
	e.pay(t, v.Order.ID)
	e.mark(t, v.Lines[0].ID)
	e.deliver(t, domainevent.TopicOrderLineFulfillmentMarked)
	sent = e.sender.all()
	if got := to("orders.delivery_marked", notify.ChannelInApp); !slices.Contains(got, org("company_admin")) || !slices.Contains(got, org("company_owner_a")) {
		t.Errorf("delivery_marked to %v", got)
	}
	a := actor(t, "company_admin")
	if _, err := e.svc.ConfirmLine(ctxOf(t, a), a, v.Lines[0].ID); err != nil {
		t.Fatal(err)
	}
	e.deliver(t, domainevent.TopicOrderLineFulfilled)
	sent = e.sender.all()
	if got := to("orders.line_fulfilled", notify.ChannelInApp); !slices.Equal(got, []uuid.UUID{org("club_admin")}) {
		t.Errorf("line_fulfilled to %v", got)
	}
	c := e.placeOne(t)
	if _, err := e.svc.CancelOrder(ctxOf(t, a), a, c.Order.ID, "Vazgeçtik"); err != nil {
		t.Fatal(err)
	}
	e.deliver(t, domainevent.TopicOrderCancelled)
	sent = e.sender.all()
	if got := to("orders.order_cancelled", notify.ChannelInApp); !slices.Contains(got, org("company_admin")) || !slices.Contains(got, org("club_admin")) {
		t.Errorf("order_cancelled to %v", got)
	}
	x := e.placeOne(t)
	e.clk.Advance(orders.DefaultPaymentWindow + time.Hour)
	if err := e.job(t, orders.ExpireJobName).Handle(t.Context()); err != nil {
		t.Fatal(err)
	}
	e.deliver(t, domainevent.TopicOrderExpired)
	sent = e.sender.all()
	if !slices.ContainsFunc(sent, func(m notify.Message) bool {
		return m.Template == "orders.order_expired" && m.Data["order_id"] == x.Order.ID.String()
	}) {
		t.Error("no order_expired notice")
	}
}

// TestPrivacy_RetainsFinancialRecords (01 §3.3, 02 §19): Export lists the
// orders a user placed and the lines a user marked; Erase keeps them
// (financial records, 10 years) and says why.
func TestPrivacy_RetainsFinancialRecords(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	l := e.paidLine(t)
	e.mark(t, l.ID)
	p := e.mod.Privacy
	if p.Name() != "orders" {
		t.Fatalf("name = %s", p.Name())
	}
	sec, err := p.Export(t.Context(), org("company_admin"))
	if err != nil || len(sec.Items) != 1 || sec.Items[0]["record"] != "order_placed" {
		t.Fatalf("buyer export = %+v, %v", sec, err)
	}
	sec, err = p.Export(t.Context(), org("club_admin"))
	if err != nil || len(sec.Items) != 1 || sec.Items[0]["record"] != "order_line_marked_fulfilled" {
		t.Fatalf("seller export = %+v, %v", sec, err)
	}
	res, err := p.Erase(t.Context(), org("company_admin"))
	if err != nil || res.Retained != 1 || res.Deleted != 0 || !strings.Contains(res.RetainedReason, "financial") {
		t.Fatalf("erase = %+v, %v", res, err)
	}
	if res, _ := p.Erase(t.Context(), uuid.NewV7()); res.Retained != 0 || res.RetainedReason != "" {
		t.Fatalf("erase of a stranger = %+v", res)
	}
	if _, err := e.store.GetOrder(t.Context(), l.OrderID, false); err != nil {
		t.Fatalf("the order is gone after erase: %v", err)
	}
}
