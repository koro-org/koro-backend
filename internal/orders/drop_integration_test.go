//go:build integration

package orders_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"uuid"

	"koro/internal/orders"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
	"koro/internal/platform/problem/problemtest"
)

// PostgreSQL tests of a cancelled offering in an unpaid cart (Q-28c,
// BE-134): the drop and the recomputed amounts commit under 000157's
// guards and the deferred orders_totals_match_lines check.

// cancelledEvent is offering.cancelled of the offering (catalog's
// cancellation itself is not under test here).
func cancelledEvent(t *testing.T, offering uuid.UUID) domainevent.Envelope {
	t.Helper()
	env, err := domainevent.New(domainevent.OfferingCancelled{OfferingID: offering, Reason: "seller_cancelled"}, "offering", offering)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

// deliverCancelled hands the event to the orders consumer.
func (p *pgEnv) deliverCancelled(ctx context.Context, env domainevent.Envelope) error {
	r := outbox.NewRegistry()
	p.mod.Subscribe(r)
	for _, s := range r.For(domainevent.TopicOfferingCancelled) {
		if err := s.Handle(ctx, env); err != nil {
			return err
		}
	}
	return nil
}

func (p *pgEnv) mustCancelOffering(t *testing.T, offering uuid.UUID) {
	t.Helper()
	if err := p.deliverCancelled(t.Context(), cancelledEvent(t, offering)); err != nil {
		t.Fatalf("offering.cancelled: %v", err)
	}
}

// TestOfferingCancelled_UnpaidCart_DropsLine_Postgres (Q-28c, H-6): a cart
// of 20 %, 10 % (another seller) and withheld lines and Koro's quota loses
// its 10 % line and then its quota line: each drop commits with the new
// amounts (the sums of the lines left), the reference code stays, the units
// go back and the quota's pending right is revoked. The old amount no
// longer fits; the new one pays the order, whose payment, collected lines
// and refund queue leave the dropped lines out, and nothing holds the
// dropped line's seller.
func TestOfferingCancelled_UnpaidCart_DropsLine_Postgres(t *testing.T) {
	t.Parallel()
	p, s2 := pgCart(t)
	booth := p.cartOffering(t, p.seller, cartVAT{rate: 2000, class: "standard"}, 1_000_000, ptr(5))
	reduced := p.cartOffering(t, s2, cartVAT{rate: 1000, class: "standard"}, 333_333, nil)
	withheld := p.cartOffering(t, p.seller, cartVAT{rate: 2000, class: "withheld", num: ptr(int16(5)), den: ptr(int16(10))}, 100_000, nil)
	quota := p.platformOffering(t)
	vs, err := p.create(p.buyerActor(t), line(booth, 2), line(reduced, 3), line(withheld, 1), line(quota, 1))
	if err != nil || len(vs) != 1 {
		t.Fatalf("CreateOrder: %d orders, %v", len(vs), err)
	}
	v := vs[0]
	if v.Order.PayableAmount != 3_909_999 {
		t.Fatalf("cart payable = %d", v.Order.PayableAmount)
	}
	red, q := lineOf(t, v, reduced), lineOf(t, v, quota)

	p.mustCancelOffering(t, reduced)
	p.mustCancelOffering(t, reduced) // at least once: idempotent
	o, err := p.mod.Orders.ForPayment(t.Context(), v.Order.ID)
	if err != nil || o.Status != orders.StatusAwaitingPayment || o.OrderNo != v.Order.OrderNo || o.KoroCollectAmount != 2_810_000 {
		t.Fatalf("order after the first drop = %+v, %v", o, err)
	}
	// 2,000,000 + 100,000 + 250,000 net; 400,000 + 20,000 + 50,000 VAT; 10,000 withheld.
	if n := p.count(t, `SELECT count(*) FROM orders WHERE id = $1 AND subtotal_amount = 2350000 AND vat_amount = 470000
		AND vat_withheld_amount = 10000 AND total_amount = 2820000 AND payable_amount = 2810000`, v.Order.ID); n != 1 {
		t.Fatal("the recomputed totals are not stored")
	}
	if n := p.count(t, `SELECT count(*) FROM order_lines WHERE order_id = $1 AND dropped_at IS NOT NULL AND fulfillment_status = 'cancelled'`, v.Order.ID); n != 1 {
		t.Fatalf("dropped lines = %d, want 1", n)
	}
	if p.sold(t, reduced) != 0 || p.sold(t, booth) != 2 {
		t.Fatalf("units sold = %d / %d", p.sold(t, reduced), p.sold(t, booth))
	}

	p.mustCancelOffering(t, quota)
	if n := p.count(t, `SELECT count(*) FROM entitlements WHERE source_type = 'order_line' AND source_id = $1 AND status = 'revoked'
		AND revoke_reason = 'order_line_dropped'`, q.ID); n != 1 {
		t.Fatal("the dropped quota line's right is not revoked")
	}
	o, err = p.mod.Orders.ForPayment(t.Context(), v.Order.ID)
	if err != nil || o.DueAmount != 2_510_000 {
		t.Fatalf("order after the second drop = %+v, %v", o, err)
	}
	_, err = p.mod.Orders.ApplyPayment(t.Context(), o.ID, uuid.NewV7(), 2_810_000)
	problemtest.WantErr(t, err, orders.ErrAmountExceedsDue)
	eff, err := p.mod.Orders.ApplyPayment(t.Context(), o.ID, uuid.NewV7(), o.DueAmount)
	if err != nil || !eff.Paid || len(eff.Lines) != 2 {
		t.Fatalf("payment = %+v, %v", eff, err)
	}
	for _, l := range eff.Lines {
		if l.LineID == red.ID || l.LineID == q.ID {
			t.Fatalf("the payment carries a dropped line: %+v", l)
		}
	}
	collected, err := p.mod.Orders.CollectedLines(t.Context(), uuid.Nil(), false, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range collected {
		if l.LineID == red.ID || l.LineID == q.ID {
			t.Fatalf("a dropped line is collected: %+v", l)
		}
	}
	due, err := p.mod.Orders.RefundsDue(t.Context(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range due {
		if d.OrderID == o.ID {
			t.Fatalf("refund due of the paid cart: %+v", d)
		}
	}
	problemtest.WantErr(t, p.mod.Orders.ApplyRefund(t.Context(), o.ID, &red.ID, 1), orders.ErrRefundExceedsPaid)
	if open, err := p.mod.OpenCommerce(t.Context(), s2.org); err != nil || open {
		t.Fatalf("the dropped line's seller open = %v, %v", open, err)
	}
}

// TestOfferingCancelled_LastLineOrMoney_CancelsWhole_Postgres (Q-28c): the
// last live line is not dropped — the order is cancelled, the earlier
// dropped line released once —, and a cart a transfer was matched to in
// part is cancelled whole, its money waiting for a refund.
func TestOfferingCancelled_LastLineOrMoney_CancelsWhole_Postgres(t *testing.T) {
	t.Parallel()
	p, s2 := pgCart(t)
	x := p.cartOffering(t, p.seller, cartVAT{rate: 2000, class: "standard"}, 1_000_000, ptr(5))
	y := p.cartOffering(t, s2, cartVAT{rate: 1000, class: "standard"}, 500_000, ptr(5))
	vs, err := p.create(p.buyerActor(t), line(x, 1), line(y, 2))
	if err != nil {
		t.Fatal(err)
	}
	cart := vs[0].Order
	p.mustCancelOffering(t, x)
	p.mustCancelOffering(t, y)
	if n := p.count(t, `SELECT count(*) FROM orders WHERE id = $1 AND status = 'cancelled' AND payable_amount = 1100000`, cart.ID); n != 1 {
		t.Fatal("the cart is not cancelled with the amounts of its last line")
	}
	if n := p.count(t, `SELECT count(*) FROM order_lines WHERE order_id = $1 AND fulfillment_status = 'cancelled' AND dropped_at IS NULL
		AND offering_id = $2`, cart.ID, y); n != 1 {
		t.Fatal("the last line is not cancelled with its order")
	}
	if p.sold(t, x) != 0 || p.sold(t, y) != 0 {
		t.Fatalf("units sold = %d / %d", p.sold(t, x), p.sold(t, y))
	}

	u := p.cartOffering(t, p.seller, cartVAT{rate: 2000, class: "standard"}, 1_000_000, ptr(5))
	w := p.cartOffering(t, s2, cartVAT{rate: 1000, class: "standard"}, 500_000, ptr(5))
	vs, err = p.create(p.buyerActor(t), line(u, 1), line(w, 1))
	if err != nil {
		t.Fatal(err)
	}
	partial := vs[0].Order
	if _, err := p.mod.Orders.ApplyPayment(t.Context(), partial.ID, uuid.NewV7(), 1000); err != nil {
		t.Fatal(err)
	}
	p.mustCancelOffering(t, u)
	if n := p.count(t, `SELECT count(*) FROM orders WHERE id = $1 AND status = 'cancelled' AND payable_amount = $2 AND paid_amount = 1000`,
		partial.ID, partial.PayableAmount); n != 1 {
		t.Fatal("the partly paid cart is not cancelled whole")
	}
	if n := p.count(t, `SELECT count(*) FROM order_lines WHERE order_id = $1 AND dropped_at IS NOT NULL`, partial.ID); n != 0 {
		t.Fatal("a line of the partly paid cart dropped")
	}
	due, err := p.mod.Orders.RefundsDue(t.Context(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range due {
		found = found || d.OrderID == partial.ID && d.LineID == nil && d.Amount == 1000
	}
	if !found {
		t.Fatalf("the partial payment is not due for a refund: %+v", due)
	}
}

// TestOfferingCancelled_DropRacesCapture_Postgres (Q-28c, the transfer
// race): the offering.cancelled consumer and a capture of the old amount
// (payments' rule: apply what the order waits for, read it again once when
// orders refuses a stale amount) run at the same time on one cart. The
// order row lock orders them: either the capture came first — the cart is
// paid in full and the cancelled line waits for a refund — or the drop did
// — the capture applies the new amount and the rest is an overpayment.
// Never both, never a dropped line on money paid against it.
func TestOfferingCancelled_DropRacesCapture_Postgres(t *testing.T) {
	t.Parallel()
	p, s2 := pgCart(t)
	const rounds = 6
	for i := range rounds {
		keep := p.cartOffering(t, p.seller, cartVAT{rate: 2000, class: "standard"}, 1_000_000, nil)
		gone := p.cartOffering(t, s2, cartVAT{rate: 1000, class: "standard"}, 500_000, nil)
		vs, err := p.create(p.buyerActor(t), line(keep, 1), line(gone, 1))
		if err != nil {
			t.Fatal(err)
		}
		o := vs[0].Order
		goneLine := lineOf(t, vs[0], gone)
		const transfer = 1_750_000 // the old amount: 1,200,000 + 550,000
		var (
			wg          sync.WaitGroup
			dropErr     error
			applied     int64
			captureErrs error
		)
		start, env := make(chan struct{}), cancelledEvent(t, gone)
		wg.Go(func() { <-start; dropErr = p.deliverCancelled(t.Context(), env) })
		wg.Go(func() {
			<-start
			for range 2 {
				po, err := p.mod.Orders.ForPayment(t.Context(), o.ID)
				if err != nil {
					captureErrs = err
					return
				}
				applied = min(transfer, po.DueAmount)
				_, err = p.mod.Orders.ApplyPayment(t.Context(), o.ID, uuid.NewV7(), applied)
				if !errors.Is(err, orders.ErrAmountExceedsDue) {
					captureErrs = err
					return
				}
			}
		})
		close(start)
		wg.Wait()
		if dropErr != nil || captureErrs != nil {
			t.Fatalf("round %d: drop %v, capture %v", i, dropErr, captureErrs)
		}
		got, err := p.mod.Orders.ForPayment(t.Context(), o.ID)
		if err != nil {
			t.Fatal(err)
		}
		l, err := p.mod.Orders.GetLine(t.Context(), goneLine.ID)
		if err != nil {
			t.Fatal(err)
		}
		switch applied {
		case transfer: // the capture first
			if got.Status != orders.StatusPaid || got.PaidAmount != transfer || l.Dropped() || l.FulfillmentStatus != orders.FulfillCancelled {
				t.Fatalf("round %d, capture first: order %+v, line %+v", i, got, l)
			}
		case 1_200_000: // the drop first: 550,000 is an overpayment for payments to refund
			if got.Status != orders.StatusPaid || got.PaidAmount != 1_200_000 || got.KoroCollectAmount != 1_200_000 || !l.Dropped() {
				t.Fatalf("round %d, drop first: order %+v, line %+v", i, got, l)
			}
		default:
			t.Fatalf("round %d: applied %d", i, applied)
		}
	}
}
