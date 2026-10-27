//go:build integration

package orders_test

import (
	"reflect"
	"testing"

	"koro/internal/authz"
	"koro/internal/orders"
	"koro/internal/platform/problem/problemtest"
)

// TestQuoteOrder_SameCartAsPlacement_Postgres (BE-136): on PostgreSQL the
// quote of a cart of 20 %, 10 %, exempt and withheld lines of two sellers
// and Koro's quota writes no order, line, right, reservation, event or
// Idempotency-Key, and equals the placement that follows — as read back
// from the database, so the frozen kuruş amounts the CHECKs accept are the
// quoted ones. The per-buyer limit the quote reads counts the committed
// lines of the buyer's open orders (BuyerHeld over GORM).
func TestQuoteOrder_SameCartAsPlacement_Postgres(t *testing.T) {
	t.Parallel()
	p, s2 := pgCart(t)
	booth := p.cartOffering(t, p.seller, cartVAT{rate: 2000, class: "standard"}, 1_000_000, ptr(5))
	reduced := p.cartOffering(t, s2, cartVAT{rate: 1000, class: "standard"}, 333_333, nil)
	exempt := p.cartOffering(t, s2, cartVAT{rate: 0, class: "exempt"}, 500_000, nil)
	withheld := p.cartOffering(t, p.seller, cartVAT{rate: 2000, class: "withheld", num: ptr(int16(5)), den: ptr(int16(10))}, 100_000, nil)
	quota := p.platformOffering(t)
	pgExec(t, p.d, `UPDATE offerings SET per_buyer_limit = 3 WHERE id = $1`, booth)
	cart := []orders.LineInput{line(booth, 2), line(reduced, 3), line(exempt, 1), line(withheld, 1), line(quota, 1)}
	ctx := p.buyerActor(t)
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		t.Fatal(err)
	}
	in := orders.CreateInput{BuyerPartyID: p.buyer.party, Lines: cart}
	keys := p.count(t, `SELECT count(*) FROM idempotency_keys`)
	qs, err := p.mod.Service.QuoteOrder(ctx, a, p.buyer.org, in)
	if err != nil || len(qs) != 1 || len(qs[0].Lines) != 5 {
		t.Fatalf("QuoteOrder = %d orders, %v", len(qs), err)
	}
	for table, sql := range map[string]string{
		"orders": `SELECT count(*) FROM orders WHERE org_id = $1`, "order_lines": `SELECT count(*) FROM order_lines WHERE org_id = $1`,
		"entitlements": `SELECT count(*) FROM entitlements WHERE org_id = $1`,
	} {
		if n := p.count(t, sql, p.buyer.org); n != 0 {
			t.Errorf("the quote wrote %d rows of %s", n, table)
		}
	}
	if n := p.count(t, `SELECT count(*) FROM idempotency_keys`); n != keys {
		t.Errorf("the quote stored %d Idempotency-Keys", n-keys)
	}
	if p.sold(t, booth) != 0 || len(p.outbox.Events()) != 0 {
		t.Fatalf("the quote reserved %d units, wrote %d events", p.sold(t, booth), len(p.outbox.Events()))
	}
	vs, err := p.create(ctx, cart...)
	if err != nil || len(vs) != 1 {
		t.Fatalf("CreateOrder = %d orders, %v", len(vs), err)
	}
	q, v := qs[0], vs[0]
	if got, want := frozenOrder(q.Order), frozenOrder(v.Order); !reflect.DeepEqual(got, want) {
		t.Errorf("order: quote %+v\nplacement %+v", got, want)
	}
	for i := range v.Lines {
		if got, want := frozenLine(q.Lines[i]), frozenLine(v.Lines[i]); !reflect.DeepEqual(got, want) {
			t.Errorf("line %d: quote %+v\nplacement %+v", i, got, want)
		}
	}
	if v.Order.SubtotalAmount != 3_849_999 || v.Order.PayableAmount != 4_409_999 {
		t.Fatalf("placement = %+v", v.Order)
	}
	// company holds 2 of booth's 3 per buyer: one more is quoted, two are not.
	if _, err := p.mod.Service.QuoteOrder(ctx, a, p.buyer.org, orders.CreateInput{BuyerPartyID: p.buyer.party,
		Lines: []orders.LineInput{line(booth, 1)}}); err != nil {
		t.Fatalf("one more unit within the limit: %v", err)
	}
	_, err = p.mod.Service.QuoteOrder(ctx, a, p.buyer.org, orders.CreateInput{BuyerPartyID: p.buyer.party,
		Lines: []orders.LineInput{line(quota, 1), line(booth, 2)}})
	problemtest.WantProblem(t, err, 409, "catalog.per_buyer_limit")
	if f := problemFields(t, err); len(f) != 1 || f[0] != "body.lines[1].offering_id" {
		t.Fatalf("per-buyer limit fields = %v", f)
	}
}
