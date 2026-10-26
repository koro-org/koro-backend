package orders_test

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/catalog"
	"koro/internal/entitlements"
	"koro/internal/notify"
	"koro/internal/orders"
	"koro/internal/parties"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/money"
	"koro/internal/platform/problem/problemtest"
)

// Tests of a cancelled offering in an unpaid cart (Q-28c, BE-134; drop.go).

// cancelAs cancels the offering as persona (its seller's admin or a
// platform admin) and delivers offering.cancelled to orders.
func (e *env) cancelAs(t *testing.T, persona string, offering uuid.UUID) {
	t.Helper()
	a := actor(t, persona)
	if _, err := e.catalog.Service.CancelOffering(ctxOf(t, a), a, offering, "Etkinlik iptal edildi"); err != nil {
		t.Fatalf("CancelOffering: %v", err)
	}
	e.deliver(t, domainevent.TopicOfferingCancelled)
}

// runDropNotices runs the queued notice tasks of the dropped lines.
func (e *env) runDropNotices(t *testing.T) {
	t.Helper()
	for _, q := range e.queue.Tasks() {
		for _, task := range e.mod.Tasks() {
			if task.Name == q.Type {
				if err := task.Handle(t.Context(), q.Payload); err != nil {
					t.Fatalf("%s: %v", q.Type, err)
				}
			}
		}
	}
}

// sameTotals reports whether two orders carry the same frozen amounts.
func sameTotals(a, b orders.Order) bool {
	return a.SubtotalAmount == b.SubtotalAmount && a.VATAmount == b.VATAmount && a.VATWithheldAmount == b.VATWithheldAmount &&
		a.BuyerFeeAmount == b.BuyerFeeAmount && a.TotalAmount == b.TotalAmount && a.PayableAmount == b.PayableAmount &&
		a.KoroCollectAmount == b.KoroCollectAmount && a.DirectToPayeeAmount == b.DirectToPayeeAmount
}

// TestOfferingCancelled_UnpaidCart_DropsOnlyThatLine (Q-28c): in an unpaid
// cart of 20 %, 10 %, exempt and withheld lines and Koro's quota, the
// cancelled 10 % offering's line drops out — cancelled with dropped_at, its
// frozen amounts kept, its units back — while the order keeps its other
// lines, its reference code and its payment window; its amounts are
// exactly those of a cart of the other lines (the same per-line kuruş
// amounts summed by the same code). The drop is audited with the amounts
// before and after, a repeated event changes nothing, and the buyer
// (e-mail with the new amount + in-app) and the seller of the line (in-app)
// are told; nobody else.
func TestOfferingCancelled_UnpaidCart_DropsOnlyThatLine(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.cartLines(t)
	v := e.place(t, line(c.booth, 2), line(c.reduced, 3), line(c.exempt, 1), line(c.withheld, 1), line(c.quota, 1))[0]
	if v.Order.PayableAmount != 4_409_999 {
		t.Fatalf("cart payable = %d", v.Order.PayableAmount)
	}
	reduced := lineOf(t, v, c.reduced)
	e.cancelAs(t, "other_club_admin", c.reduced)
	e.deliver(t, domainevent.TopicOfferingCancelled) // at least once: idempotent

	got := e.order(t, v.Order.ID)
	if got.Status != orders.StatusAwaitingPayment || got.OrderNo != v.Order.OrderNo || !got.ExpiresAt.Equal(v.Order.ExpiresAt) {
		t.Fatalf("order after the drop = %+v", got)
	}
	// 2,000,000 + 500,000 + 100,000 + 250,000 net; 400,000 + 0 + 20,000 + 50,000 VAT; 10,000 withheld.
	if got.SubtotalAmount != 2_850_000 || got.VATAmount != 470_000 || got.VATWithheldAmount != 10_000 || got.BuyerFeeAmount != 0 ||
		got.TotalAmount != 3_320_000 || got.PayableAmount != 3_310_000 || got.KoroCollectAmount != 3_310_000 || got.DirectToPayeeAmount != 0 {
		t.Fatalf("recomputed amounts = %+v", got)
	}
	twin := e.place(t, line(c.booth, 2), line(c.exempt, 1), line(c.withheld, 1), line(c.quota, 1))[0].Order
	if !sameTotals(got, twin) {
		t.Fatalf("amounts after the drop %+v differ from a cart of the other lines %+v", got, twin)
	}
	dropped := e.line(t, reduced.ID)
	if !dropped.Dropped() || dropped.FulfillmentStatus != orders.FulfillCancelled || dropped.PayableAmount != reduced.PayableAmount ||
		dropped.NetAmount != reduced.NetAmount || !dropped.DroppedAt.Equal(e.clk.Now().UTC().Truncate(time.Microsecond)) {
		t.Fatalf("dropped line = %+v", dropped)
	}
	for _, off := range []uuid.UUID{c.booth, c.exempt, c.withheld, c.quota} {
		if l := e.line(t, lineOf(t, v, off).ID); l.Dropped() || l.FulfillmentStatus == orders.FulfillCancelled {
			t.Errorf("line %s = %s, dropped %v", l.TitleSnapshot, l.FulfillmentStatus, l.Dropped())
		}
	}
	if n := e.sold(t, c.reduced); n != 0 {
		t.Fatalf("units of the cancelled offering sold = %d, want 0", n)
	}
	if rs := e.rights(t, lineOf(t, v, c.quota).ID); len(rs) != 1 || rs[0].Status != entitlements.StatusPending {
		t.Fatalf("the quota line's right = %+v", rs)
	}
	if n := len(e.events(domainevent.TopicOrderCancelled)); n != 0 {
		t.Fatalf("order.cancelled events = %d, want 0", n)
	}
	var drops []map[string]any
	for _, ev := range e.audit.Of(authz.OrdersOrderCancel) {
		if ev.Metadata["effect"] == "line_dropped" {
			drops = append(drops, ev.Metadata)
			if *ev.ResourceID != reduced.ID || ev.ResourceType != authz.TypeOrderLine || *ev.OrgID != org("company_a") {
				t.Errorf("drop audit = %+v", ev)
			}
		}
	}
	if len(drops) != 1 || drops[0]["payable_before"] != int64(4_409_999) || drops[0]["payable_after"] != int64(3_310_000) ||
		drops[0]["from"] != "pending" || drops[0]["offering_cause"] != catalog.CancelBySeller || drops[0]["order_no"] != v.Order.OrderNo {
		t.Fatalf("drop audit records = %+v", drops)
	}
	if qs := e.queue.Tasks(); len(qs) != 1 || qs[0].Type != orders.NotifyLineDroppedTaskName {
		t.Fatalf("queued tasks = %+v, want one notice", qs)
	}

	e.runDropNotices(t)
	buyer, seller := actor(t, "company_admin").UserID, actor(t, "other_club_admin").UserID
	var mail, buyerInApp, sellerInApp int
	for _, m := range e.sender.all() {
		switch {
		case m.Template == "orders.line_dropped" && m.Channel == notify.ChannelEmail && *m.UserID == buyer:
			mail++
			if m.Data["amount"] != money.Decimal(3_310_000) || m.Data["reference_code"] != v.Order.OrderNo || m.Data["line_title"] != "Atölye" {
				t.Errorf("e-mail data = %v", m.Data)
			}
		case m.Template == "orders.line_dropped" && m.Channel == notify.ChannelInApp && *m.UserID == buyer:
			buyerInApp++
			if m.LinkPath != notify.OrderLink(v.Order.ID) {
				t.Errorf("buyer link = %v", m.LinkPath)
			}
		case m.Template == "orders.sale_line_dropped" && *m.UserID == seller:
			sellerInApp++
			if m.LinkPath != notify.OrgSalesLink(org("club_b")) || *m.SourceOrgID != org("club_b") {
				t.Errorf("seller link = %v", m.LinkPath)
			}
		default:
			t.Errorf("unexpected notice %s to %s", m.Template, m.UserID)
		}
	}
	if mail != 1 || buyerInApp != 1 || sellerInApp != 1 {
		t.Fatalf("notices: e-mail %d, buyer in-app %d, seller in-app %d", mail, buyerInApp, sellerInApp)
	}
}

// TestOfferingCancelled_LastLine_CancelsOrder (Q-28c): when the dropped
// line would be the order's last, the order is cancelled instead
// (order.cancelled); its earlier dropped line is not released twice — not
// by that cancellation, nor by the buyer's cancellation or the expiry of
// another cart.
func TestOfferingCancelled_LastLine_CancelsOrder(t *testing.T) {
	t.Parallel()
	rec := &lockRecorder{}
	e := newEnv(t, func(d *orders.Deps) { rec.Offerings = d.Offerings; d.Offerings = rec })
	a, b := e.offering(t, nil), e.offering(t, nil)
	v := e.place(t, line(a, 1), line(b, 2))[0]
	rec.take()
	e.cancelAs(t, "club_admin", a)
	if got := rec.take(); !slices.Equal(got, []uuid.UUID{a}) {
		t.Fatalf("releases of the drop = %v, want only %s", got, a)
	}
	e.cancelAs(t, "club_admin", b)
	if got := rec.take(); !slices.Equal(got, []uuid.UUID{b}) {
		t.Fatalf("releases of the cancellation = %v, want only %s", got, b)
	}
	o := e.order(t, v.Order.ID)
	if o.Status != orders.StatusCancelled || *o.CancelReason != orders.CancelOfferingClosed || o.PayableAmount != lineOf(t, v, b).PayableAmount {
		t.Fatalf("order = %+v, want cancelled with the amounts of its last line", o)
	}
	if l := e.line(t, lineOf(t, v, b).ID); l.Dropped() || l.FulfillmentStatus != orders.FulfillCancelled {
		t.Fatalf("last line = %+v, want cancelled with its order, not dropped", l)
	}
	if n := len(e.events(domainevent.TopicOrderCancelled)); n != 1 {
		t.Fatalf("order.cancelled events = %d", n)
	}
	if e.sold(t, a) != 0 || e.sold(t, b) != 0 {
		t.Fatalf("units sold = %d / %d", e.sold(t, a), e.sold(t, b))
	}

	// The buyer cancels, the payment window closes: the dropped line stays released once.
	x, y, z := e.offering(t, nil), e.offering(t, nil), e.offering(t, nil)
	byBuyer := e.place(t, line(x, 1), line(y, 1))[0]
	expiring := e.place(t, line(z, 1), line(y, 1))[0]
	e.cancelAs(t, "club_admin", y)
	rec.take()
	buyer := actor(t, "company_admin")
	if _, err := e.svc.CancelOrder(ctxOf(t, buyer), buyer, byBuyer.Order.ID, "Sepet değişti"); err != nil {
		t.Fatal(err)
	}
	if got := rec.take(); !slices.Equal(got, []uuid.UUID{x}) {
		t.Fatalf("releases of the buyer's cancellation = %v, want only %s", got, x)
	}
	e.clk.Advance(8 * 24 * time.Hour)
	for _, j := range e.mod.Jobs() {
		if j.Name == orders.ExpireJobName {
			if err := j.Handle(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
	}
	if got := rec.take(); !slices.Equal(got, []uuid.UUID{z}) {
		t.Fatalf("releases of the expiry = %v, want only %s", got, z)
	}
	if o := e.order(t, expiring.Order.ID); o.Status != orders.StatusExpired {
		t.Fatalf("expiring cart = %s", o.Status)
	}
	if e.sold(t, x) != 0 || e.sold(t, y) != 0 || e.sold(t, z) != 0 {
		t.Fatalf("units sold = %d / %d / %d", e.sold(t, x), e.sold(t, y), e.sold(t, z))
	}
}

// TestOfferingCancelled_MoneyArrived_CancelsWhole (Q-28c, the transfer
// race): once a transfer was matched to the cart, even in part, or a model
// C direct payment was confirmed, no line drops — the order is cancelled
// whole and the money paid waits for a refund, as before BE-134.
func TestOfferingCancelled_MoneyArrived_CancelsWhole(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a, b := e.offering(t, nil), e.offering(t, nil)
	v := e.place(t, line(a, 1), line(b, 1))[0]
	if _, err := e.mod.Orders.ApplyPayment(t.Context(), v.Order.ID, uuid.NewV7(), 100_000); err != nil {
		t.Fatal(err)
	}
	e.cancelAs(t, "club_admin", a)
	o := e.order(t, v.Order.ID)
	if o.Status != orders.StatusCancelled || o.PayableAmount != v.Order.PayableAmount || o.PaidAmount != 100_000 {
		t.Fatalf("partly paid cart = %+v, want cancelled whole", o)
	}
	for _, l := range v.Lines {
		if got := e.line(t, l.ID); got.Dropped() || got.FulfillmentStatus != orders.FulfillCancelled {
			t.Fatalf("line %s = %+v", l.ID, got)
		}
	}
	due, err := e.mod.Orders.RefundsDue(t.Context(), 10)
	if err != nil || len(due) != 1 || due[0].LineID != nil || due[0].Amount != 100_000 {
		t.Fatalf("refunds due = %+v, %v", due, err)
	}
	if e.sold(t, a) != 0 || e.sold(t, b) != 0 {
		t.Fatal("the cancelled cart kept units")
	}

	// Model C: the direct payment was confirmed before the cancellation.
	c, d := e.offering(t, nil), e.offering(t, nil)
	for _, id := range []uuid.UUID{c, d} {
		e.addPolicy(t, catalog.ScopeOffering, &id, modelCPolicy)
	}
	mc := e.place(t, line(c, 1), line(d, 1))[0]
	if _, err := e.mod.Orders.ConfirmDirectPayment(t.Context(), mc.Order.ID, uuid.NewV7()); err != nil {
		t.Fatal(err)
	}
	e.cancelAs(t, "club_admin", c)
	if o := e.order(t, mc.Order.ID); o.Status != orders.StatusCancelled || o.DirectToPayeeAmount != mc.Order.DirectToPayeeAmount {
		t.Fatalf("confirmed model C cart = %+v, want cancelled whole", o)
	}
	if qs := e.queue.Tasks(); len(qs) != 0 {
		t.Fatalf("drop notices = %+v, want none", qs)
	}
}

// TestDroppedLine_NeverPaidRefundedOrCollected (Q-28c, the transfer race):
// the old amount no longer fits the order (orders.amount_exceeds_due: payments
// applies the new one and refunds the rest); paid in full, the order's
// payment effect, the collected lines and the refund queue leave the
// dropped line out, a refund of it is refused, nothing holds its seller,
// and the order completes once its live lines are delivered.
func TestDroppedLine_NeverPaidRefundedOrCollected(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.cartLines(t)
	v := e.place(t, line(c.booth, 1), line(c.reduced, 1), line(c.quota, 1))[0]
	reduced := lineOf(t, v, c.reduced)
	e.cancelAs(t, "other_club_admin", c.reduced)
	o := e.order(t, v.Order.ID)
	if o.PayableAmount != v.Order.PayableAmount-reduced.PayableAmount {
		t.Fatalf("payable %d, want %d", o.PayableAmount, v.Order.PayableAmount-reduced.PayableAmount)
	}
	_, err := e.mod.Orders.ApplyPayment(t.Context(), o.ID, uuid.NewV7(), v.Order.PayableAmount)
	problemtest.WantProblem(t, err, 409, "orders.amount_exceeds_due")
	p, err := e.mod.Orders.ForPayment(t.Context(), o.ID)
	if err != nil || p.DueAmount != o.PayableAmount || p.KoroHeld(e.line(t, reduced.ID)) != 0 {
		t.Fatalf("payable order = %+v, %v", p, err)
	}
	eff, err := e.mod.Orders.ApplyPayment(t.Context(), o.ID, uuid.NewV7(), p.DueAmount)
	if err != nil || !eff.Paid || len(eff.Lines) != 2 {
		t.Fatalf("payment = %+v, %v", eff, err)
	}
	var paidSum int64
	for _, l := range eff.Lines {
		if l.LineID == reduced.ID {
			t.Fatal("the payment effect carries the dropped line")
		}
		paidSum += l.PayableAmount
	}
	if paidSum != eff.PaidAmount {
		t.Fatalf("lines of the payment %d, paid %d", paidSum, eff.PaidAmount)
	}
	collected, err := e.mod.Orders.CollectedLines(t.Context(), uuid.Nil(), false, 100)
	if err != nil || len(collected) != 2 || slices.ContainsFunc(collected, func(l orders.CollectedLine) bool { return l.LineID == reduced.ID }) {
		t.Fatalf("collected lines = %+v, %v", collected, err)
	}
	problemtest.WantErr(t, db.NopTx{}.WithTx(t.Context(), func(ctx context.Context) error {
		_, err := e.mod.Orders.ForAllocation(ctx, reduced.ID)
		return err
	}), orders.ErrInvalidState)
	if due, err := e.mod.Orders.RefundsDue(t.Context(), 10); err != nil || len(due) != 0 {
		t.Fatalf("refunds due = %+v, %v", due, err)
	}
	problemtest.WantErr(t, e.mod.Orders.ApplyRefund(t.Context(), o.ID, &reduced.ID, 1), orders.ErrRefundExceedsPaid)
	if open, err := e.mod.OpenCommerce(t.Context(), org("club_b")); err != nil || open {
		t.Fatalf("the dropped line's seller open = %v, %v", open, err)
	}
	booth := lineOf(t, v, c.booth)
	e.mark(t, booth.ID)
	buyer := actor(t, "company_admin")
	if _, err := e.svc.ConfirmLine(ctxOf(t, buyer), buyer, booth.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.order(t, o.ID); got.Status != orders.StatusCompleted {
		t.Fatalf("order = %s, want completed (delivered, Koro's quota, the dropped line)", got.Status)
	}
	if open, err := e.mod.OpenCommerce(t.Context(), org("company_a")); err != nil || open {
		t.Fatalf("buyer open = %v, %v (nothing to refund)", open, err)
	}
}

// TestOfferingCancelled_ModelC_DirectTransfersFollowLines (Q-28c): in an
// unpaid model C cart of two payees, the dropped line's payee leaves the
// direct transfers; Koro's fee and the direct amount are those of the
// line left.
func TestOfferingCancelled_ModelC_DirectTransfersFollowLines(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	b := e.payees.add("club_b", parties.PayeeActive)
	e.payees.accounts = map[uuid.UUID]parties.BankAccount{}
	for _, id := range []uuid.UUID{e.payeeA, b} {
		py := e.payees.m[id]
		py.LegalPartyID = e.parties.add(parties.LegalParty{Kind: parties.KindPayee, LegalType: parties.LegalFoundation, Title: "Synthetic Foundation", Status: parties.StatusVerified})
		e.payees.m[id] = py
		e.payees.accounts[id] = parties.BankAccount{ID: uuid.NewV7(), PayeeID: id, IBAN: "TR809999900000000000000001", HolderName: "Synthetic Foundation", Status: parties.AccountApproved}
	}
	aOff := e.offering(t, nil)
	bOff := e.offering(t, func(o *catalog.Offering) { o.OrgID = org("club_b"); o.DefaultPayeeID = &b })
	for _, id := range []uuid.UUID{aOff, bOff} {
		e.addPolicy(t, catalog.ScopeOffering, &id, modelCPolicy)
	}
	v := e.place(t, line(aOff, 2), line(bOff, 1))[0]
	e.acct = &orders.CollectionAccount{IBAN: "TR039999901234567890123456", HolderName: "Synthetic Koro"}
	e.cancelAs(t, "other_club_admin", bOff)
	o := e.order(t, v.Order.ID)
	// Line A: 2 × 1,000,000 + 20 % VAT = 2,400,000 to the payee; 10 % + 20 % VAT fee = 240,000 to Koro.
	if o.KoroCollectAmount != 240_000 || o.DirectToPayeeAmount != 2_400_000 || o.PayableAmount != 2_640_000 || o.ModelCode != orders.ModelC {
		t.Fatalf("model C amounts after the drop = %+v", o)
	}
	buyer := actor(t, "company_admin")
	got, err := e.svc.PaymentInstructions(ctxOf(t, buyer), buyer, o.ID)
	if err != nil || got.AmountDue != 240_000 || len(got.DirectPayments) != 1 || got.DirectPayments[0].PayeeID != e.payeeA ||
		got.DirectPayments[0].Amount != 2_400_000 {
		t.Fatalf("instructions after the drop = %+v, %v", got, err)
	}
	if v, err := e.svc.GetOrder(ctxOf(t, buyer), buyer, o.ID); err != nil || v.DirectStatus != orders.DirectAvailable {
		t.Fatalf("direct status = %q, %v", v.DirectStatus, err)
	}
	if eff, err := e.mod.Orders.ConfirmDirectPayment(t.Context(), o.ID, uuid.NewV7()); err != nil || eff.Paid {
		t.Fatalf("confirmation = %+v, %v", eff, err)
	}
	if eff, err := e.mod.Orders.ApplyPayment(t.Context(), o.ID, uuid.NewV7(), 240_000); err != nil || !eff.Paid || len(eff.Lines) != 1 {
		t.Fatalf("fee = %+v, %v", eff, err)
	}
}

// TestDroppedLine_HTTP (OpenAPI): getOrder and listOrgSales show the
// dropped line with dropped_at; the buyer's totals are the live lines'.
func TestDroppedLine_HTTP(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.cartLines(t)
	v := e.place(t, line(c.booth, 1), line(c.reduced, 1))[0]
	e.cancelAs(t, "other_club_admin", c.reduced)
	app := newHTTP(t, e)
	rec, body := call(t, app, "company_admin", http.MethodGet, "/api/v1/orders/"+v.Order.ID.String(), nil, nil)
	if rec.Code != http.StatusOK || body["payable_amount"] != float64(1_200_000) || body["status"] != "awaiting_payment" {
		t.Fatalf("getOrder = %d %v", rec.Code, body)
	}
	var dropped, live int
	for _, x := range body["lines"].([]any) {
		l := x.(map[string]any)
		switch {
		case l["offering_id"] == c.reduced.String() && l["dropped_at"] != nil && l["fulfillment_status"] == "cancelled":
			dropped++
		case l["offering_id"] == c.booth.String() && l["dropped_at"] == nil && l["fulfillment_status"] == "pending":
			live++
		}
	}
	if dropped != 1 || live != 1 {
		t.Fatalf("lines = %v", body["lines"])
	}
	rec, body = call(t, app, "other_club_admin", http.MethodGet, "/api/v1/orgs/"+org("club_b").String()+"/sales", nil, nil)
	items, _ := body["items"].([]any)
	if rec.Code != http.StatusOK || len(items) != 1 || items[0].(map[string]any)["dropped_at"] == nil {
		t.Fatalf("listOrgSales = %d %v", rec.Code, body)
	}
}

// TestLineDroppedNotice_Stale: the notice of a dropped line whose order is
// paid by now leaves the e-mail out (the amount is no longer due); a task
// of an unknown line does nothing; a broken payload is permanent.
func TestLineDroppedNotice_Stale(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a, b := e.offering(t, nil), e.offering(t, nil)
	v := e.place(t, line(a, 1), line(b, 1))[0]
	e.cancelAs(t, "club_admin", a)
	e.pay(t, v.Order.ID)
	e.runDropNotices(t)
	for _, m := range e.sender.all() {
		if m.Channel == notify.ChannelEmail {
			t.Fatalf("e-mail of a paid order: %+v", m)
		}
	}
	if n := len(e.sender.all()); n != 2 {
		t.Fatalf("notices = %d, want the buyer's and the club admin's in-app", n)
	}
	task := e.mod.Tasks()[0]
	if err := task.Handle(t.Context(), []byte(`{"order_id":"`+v.Order.ID.String()+`","order_line_id":"`+uuid.NewV7().String()+`"}`)); err != nil {
		t.Fatalf("unknown line: %v", err)
	}
	if err := task.Handle(t.Context(), []byte(`{`)); err == nil {
		t.Fatal("a broken payload was accepted")
	}
}
