//go:build integration

package orders_test

import (
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/orders"
)

// TestPartialRefund_CartCompletes_Postgres (T-382, BE-135): a cart of a
// booth (seller 1), a workshop and a stand (seller 2) is paid; the workshop's
// offering is cancelled and its line refunded, the stand is refunded in full
// while still pending. The order stays partially refunded — holding its
// buyer and the booth's units — until the booth is delivered; then it
// completes under the CHECKs with both refunds kept, its buyer is free, the
// units leave the per-buyer count and seller 2 (nothing left to deliver or
// pay out) is free too, while seller 1 waits for its payout. A second cart
// that completed while its cancelled line waited for the refund stays
// completed, its completion time kept, when the refund arrives.
func TestPartialRefund_CartCompletes_Postgres(t *testing.T) {
	t.Parallel()
	p, s2 := pgCart(t)
	std := cartVAT{rate: 2000, class: "standard"}
	booth := p.cartOffering(t, p.seller, std, 1_000_000, nil)
	workshop := p.cartOffering(t, s2, cartVAT{rate: 1000, class: "standard"}, 500_000, nil)
	stand := p.cartOffering(t, s2, std, 250_000, nil)
	bctx := p.buyerActor(t)
	buyer, err := authz.ActorFrom(bctx)
	if err != nil {
		t.Fatal(err)
	}
	vs, err := p.create(bctx, line(booth, 2), line(workshop, 1), line(stand, 1))
	if err != nil || len(vs) != 1 {
		t.Fatalf("CreateOrder: %d orders, %v", len(vs), err)
	}
	v := vs[0]
	if _, err := p.mod.Orders.ApplyPayment(t.Context(), v.Order.ID, uuid.NewV7(), v.Order.PayableAmount); err != nil {
		t.Fatal(err)
	}
	p.mustCancelOffering(t, workshop)
	ws, st, bo := lineOf(t, v, workshop), lineOf(t, v, stand), lineOf(t, v, booth)
	for _, l := range []orders.Line{ws, st} {
		if err := p.mod.Orders.ApplyRefund(t.Context(), v.Order.ID, &l.ID, l.PayableAmount); err != nil {
			t.Fatalf("refund of %s: %v", l.TitleSnapshot, err)
		}
	}
	refunded := int(ws.PayableAmount + st.PayableAmount)
	if n := p.count(t, `SELECT count(*) FROM orders WHERE id = $1 AND status = 'partially_refunded' AND completed_at IS NULL
		AND refunded_amount = $2`, v.Order.ID, refunded); n != 1 {
		t.Fatal("the order after the refunds is not partially_refunded")
	}
	open := func(step string, org uuid.UUID, want bool) {
		t.Helper()
		if got, err := p.mod.OpenCommerce(t.Context(), org); err != nil || got != want {
			t.Fatalf("%s: open(%s) = %v, %v; want %v", step, org, got, err, want)
		}
	}
	open("a booth still to deliver", p.buyer.org, true)
	if n, err := p.mod.BuyerHeld(t.Context(), booth, p.buyer.org); err != nil || n != 2 {
		t.Fatalf("booth units held while the order runs = %d, %v", n, err)
	}

	seller, sctx := p.sellerActor(t)
	if _, err := p.mod.Service.MarkLineFulfilled(sctx, seller, bo.ID, orders.MarkInput{}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.mod.Service.ConfirmLine(bctx, buyer, bo.ID); err != nil {
		t.Fatal(err)
	}
	if n := p.count(t, `SELECT count(*) FROM orders WHERE id = $1 AND status = 'completed' AND completed_at IS NOT NULL
		AND refunded_amount = $2 AND paid_amount = $3`, v.Order.ID, refunded, v.Order.PayableAmount); n != 1 {
		t.Fatal("the order is not completed with its refunds")
	}
	for id, want := range map[uuid.UUID]string{ws.ID: "cancelled", st.ID: "pending", bo.ID: "fulfilled"} {
		if n := p.count(t, `SELECT count(*) FROM order_lines WHERE id = $1 AND fulfillment_status = $2`, id, want); n != 1 {
			t.Errorf("line %s is not %s", id, want)
		}
	}
	open("every line final", p.buyer.org, false)
	if n, err := p.mod.BuyerHeld(t.Context(), booth, p.buyer.org); err != nil || n != 0 {
		t.Fatalf("booth units held by the completed order = %d, %v", n, err)
	}
	open("seller 2: cancelled and refunded in full", s2.org, false)
	open("seller 1: the booth waits for its payout", p.seller.org, true)

	// Delivered first, refunded after: the order stays completed.
	other, gone := p.cartOffering(t, p.seller, std, 100_000, nil), p.cartOffering(t, s2, std, 100_000, nil)
	vs, err = p.create(bctx, line(other, 1), line(gone, 1))
	if err != nil {
		t.Fatal(err)
	}
	w := vs[0]
	if _, err := p.mod.Orders.ApplyPayment(t.Context(), w.Order.ID, uuid.NewV7(), w.Order.PayableAmount); err != nil {
		t.Fatal(err)
	}
	p.mustCancelOffering(t, gone)
	ol := lineOf(t, w, other)
	if _, err := p.mod.Service.MarkLineFulfilled(sctx, seller, ol.ID, orders.MarkInput{}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.mod.Service.ConfirmLine(bctx, buyer, ol.ID); err != nil {
		t.Fatal(err)
	}
	done, err := p.mod.Orders.ForPayment(t.Context(), w.Order.ID)
	if err != nil || done.Status != orders.StatusCompleted {
		t.Fatalf("second cart = %+v, %v", done, err)
	}
	open("a cancelled line's refund due", p.buyer.org, true)
	var completedAt string
	if err := p.d.Pool.QueryRow(t.Context(), `SELECT completed_at::text FROM orders WHERE id = $1`, w.Order.ID).Scan(&completedAt); err != nil {
		t.Fatal(err)
	}
	p.clk.Advance(time.Hour)
	gl := lineOf(t, w, gone)
	if err := p.mod.Orders.ApplyRefund(t.Context(), w.Order.ID, &gl.ID, gl.PayableAmount); err != nil {
		t.Fatal(err)
	}
	if n := p.count(t, `SELECT count(*) FROM orders WHERE id = $1 AND status = 'completed' AND completed_at::text = $2
		AND refunded_amount = $3`, w.Order.ID, completedAt, gl.PayableAmount); n != 1 {
		t.Fatal("the refund moved the completed order")
	}
	open("the refund arrived", p.buyer.org, false)
}
