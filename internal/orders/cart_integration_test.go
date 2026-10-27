//go:build integration

package orders_test

import (
	"slices"
	"sync"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/orders"
	"koro/internal/parties"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/problem/problemtest"
)

// cartVAT is the VAT profile of a cart offering's service.
type cartVAT struct {
	rate     int
	class    string
	num, den *int16
}

// cartOffering inserts an active offering of seller s (its payee) priced at
// price excluding VAT, with a service of the VAT profile, stock (nil:
// unlimited) and its own model A policy (10 % from the seller).
func (p *pgEnv) cartOffering(t *testing.T, s sellerRef, v cartVAT, price int64, stock *int) uuid.UUID {
	t.Helper()
	n := nextN()
	svc, off, by, approver := uuid.NewV7(), uuid.NewV7(), pgUser(t, p.d), pgUser(t, p.d)
	var exemption, withholding *string
	switch v.class {
	case "exempt":
		exemption = ptr("325")
	case "withheld":
		withholding = ptr("627")
	}
	pgExec(t, p.d, `INSERT INTO services (id, code, name_tr, category, fulfillment_type, default_vat_rate_bp, vat_class, vat_exemption_code,
		vat_withholding_code, vat_withholding_num, vat_withholding_den, accounting_reviewed_at)
		VALUES ($1, $2, 'Hizmet', 'booth', 'org_bound', $3, $4, $5, $6, $7, $8, now())`,
		svc, "cart_o"+n, v.rate, v.class, exemption, withholding, v.num, v.den)
	pgExec(t, p.d, `INSERT INTO offerings (id, service_id, seller_kind, org_id, default_payee_id, title, unit_price_amount, vat_rate_bp,
		quantity_total, status, submitted_at, approved_by, approved_at, created_by)
		VALUES ($1, $2, 'org', $3, $4, $5, $6, $7, $8, 'active', now(), $9, now(), $10)`,
		off, svc, s.org, s.payee, "Sepet kalemi "+n, price, v.rate, stock, approver, by)
	pgExec(t, p.d, `INSERT INTO commerce_policies (id, scope, scope_id, model_code, collection_method, commission_rate_bp, commission_basis,
		commission_payer, valid_from, created_by)
		VALUES ($1, 'offering', $2, 'A', 'bank_transfer', 1000, 'net_excl_vat', 'seller', $3, $4)`, uuid.NewV7(), off, policyFrom, by)
	return off
}

// pgCart is a pgEnv whose payees port knows a second seller (s2).
func pgCart(t *testing.T) (*pgEnv, sellerRef) {
	t.Helper()
	payees := &fakePayees{m: map[uuid.UUID]parties.Payee{}}
	p := newPGEnv(t, func(d *orders.Deps) { d.Payees = payees })
	s2 := p.w.seller(t)
	for _, s := range []sellerRef{p.seller, s2} {
		payees.m[s.payee] = parties.Payee{ID: s.payee, OrgID: s.org, Status: parties.PayeeActive, IsDefault: true}
	}
	return p, s2
}

func (p *pgEnv) sold(t *testing.T, offering uuid.UUID) int {
	t.Helper()
	o, err := p.cat.GetOffering(t.Context(), offering, false)
	if err != nil {
		t.Fatal(err)
	}
	return o.QuantitySold
}

func (p *pgEnv) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := p.d.Pool.QueryRow(t.Context(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// TestCreateOrder_Cart_MixedVATTwoPayees_Postgres (Q-25, H-6): one cart of
// 20 %, 10 %, exempt and withheld lines of two sellers (two payees) and
// Koro's quota commits as one model A order on PostgreSQL — the deferred
// orders_totals_match_lines check and every line CHECK accept the per-line
// kuruş amounts —, is paid in two transfers, and takes line refunds (one
// whole, one partial) under the line and order CHECKs while the other
// lines keep their amounts.
func TestCreateOrder_Cart_MixedVATTwoPayees_Postgres(t *testing.T) {
	t.Parallel()
	p, s2 := pgCart(t)
	booth := p.cartOffering(t, p.seller, cartVAT{rate: 2000, class: "standard"}, 1_000_000, ptr(5))
	reduced := p.cartOffering(t, s2, cartVAT{rate: 1000, class: "standard"}, 333_333, nil)
	exempt := p.cartOffering(t, s2, cartVAT{rate: 0, class: "exempt"}, 500_000, nil)
	withheld := p.cartOffering(t, p.seller, cartVAT{rate: 2000, class: "withheld", num: ptr(int16(5)), den: ptr(int16(10))}, 100_000, nil)
	quota := p.platformOffering(t)
	vs, err := p.create(p.buyerActor(t), line(booth, 2), line(reduced, 3), line(exempt, 1), line(withheld, 1), line(quota, 1))
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if len(vs) != 1 || len(vs[0].Lines) != 5 {
		t.Fatalf("orders = %d, want one model A order of five lines", len(vs))
	}
	v := vs[0]
	sumsMatch(t, v)
	o := v.Order
	// 2,000,000 + 999,999 + 500,000 + 100,000 + 250,000 net; 400,000 + 100,000 + 0 + 20,000 + 50,000 VAT; 10,000 withheld.
	if o.SubtotalAmount != 3_849_999 || o.VATAmount != 570_000 || o.VATWithheldAmount != 10_000 || o.PayableAmount != 4_409_999 {
		t.Fatalf("order = %+v", o)
	}
	for off, want := range map[uuid.UUID]struct {
		payee     *uuid.UUID
		vat       int64
		sellerNet int64
	}{
		booth: {&p.seller.payee, 400_000, 2_160_000}, reduced: {&s2.payee, 100_000, 979_999}, exempt: {&s2.payee, 0, 440_000},
		withheld: {&p.seller.payee, 20_000, 98_000}, quota: {nil, 50_000, 300_000},
	} {
		l := lineOf(t, v, off)
		if (want.payee == nil) != (l.PayeeID == nil) || want.payee != nil && *l.PayeeID != *want.payee || l.VATAmount != want.vat ||
			l.SellerNetAmount != want.sellerNet {
			t.Errorf("line %s: payee %v VAT %d seller net %d", l.TitleSnapshot, l.PayeeID, l.VATAmount, l.SellerNetAmount)
		}
	}
	if eff, err := p.mod.Orders.ApplyPayment(t.Context(), o.ID, uuid.NewV7(), 1_000_000); err != nil || eff.Paid {
		t.Fatalf("first transfer = %+v, %v", eff, err)
	}
	eff, err := p.mod.Orders.ApplyPayment(t.Context(), o.ID, uuid.NewV7(), 3_409_999)
	if err != nil || !eff.Paid || len(eff.Lines) != 5 {
		t.Fatalf("second transfer = %+v, %v", eff, err)
	}
	ex, red := lineOf(t, v, exempt), lineOf(t, v, reduced)
	if err := p.mod.Orders.ApplyRefund(t.Context(), o.ID, &ex.ID, 500_000); err != nil {
		t.Fatalf("refund of the exempt line: %v", err)
	}
	if err := p.mod.Orders.ApplyRefund(t.Context(), o.ID, &red.ID, 99_999); err != nil {
		t.Fatalf("partial refund of the reduced line: %v", err)
	}
	problemtest.WantErr(t, p.mod.Orders.ApplyRefund(t.Context(), o.ID, &ex.ID, 1), orders.ErrRefundExceedsPaid)
	got, err := p.mod.Orders.ForPayment(t.Context(), o.ID)
	if err != nil || got.Status != orders.StatusPartiallyRefunded || got.PaidAmount != 4_409_999 {
		t.Fatalf("order after the refunds = %+v, %v", got, err)
	}
	if n := p.count(t, `SELECT refunded_amount FROM orders WHERE id = $1`, o.ID); n != 599_999 {
		t.Fatalf("order refunded = %d, want 599,999", n)
	}
	for id, want := range map[uuid.UUID]int{ex.ID: 500_000, red.ID: 99_999, lineOf(t, v, booth).ID: 0, lineOf(t, v, withheld).ID: 0} {
		if n := p.count(t, `SELECT refunded_amount FROM order_lines WHERE id = $1`, id); n != want {
			t.Errorf("line %s refunded %d, want %d", id, n, want)
		}
	}
}

// TestCreateOrder_Cart_OneLineSoldOut_ReservesNothing_Postgres (BE-131): a
// cart whose last line is sold out is refused with 409 catalog.sold_out
// naming that line; the reservation the earlier line took in the same
// transaction is rolled back, no order, line or right is written and the
// Idempotency-Key is free again, so the corrected cart is placed under it.
func TestCreateOrder_Cart_OneLineSoldOut_ReservesNothing_Postgres(t *testing.T) {
	t.Parallel()
	p, _ := pgCart(t)
	first := p.cartOffering(t, p.seller, cartVAT{rate: 2000, class: "standard"}, 1_000_000, ptr(5))
	quota := p.platformOffering(t)
	last := p.cartOffering(t, p.seller, cartVAT{rate: 2000, class: "standard"}, 1_000_000, ptr(1))
	ctx := idempotency.WithKey(p.buyerActor(t), idempotency.Key{Scope: p.buyer.user.String() + ":POST /orgs/{org_id}/orders",
		Value: "pg-cart-sold-out", Status: 201})
	_, err := p.create(ctx, line(first, 2), line(quota, 1), line(last, 2))
	problemtest.WantProblem(t, err, 409, "catalog.sold_out")
	if f := problemFields(t, err); len(f) != 1 || f[0] != "body.lines[2].offering_id" {
		t.Fatalf("fields = %v, want the third line", f)
	}
	if n := p.sold(t, first); n != 0 {
		t.Fatalf("the refused cart kept %d units of its first line", n)
	}
	if n := p.count(t, `SELECT count(*) FROM orders WHERE org_id = $1`, p.buyer.org); n != 0 {
		t.Fatalf("the refused cart wrote %d orders", n)
	}
	if n := p.count(t, `SELECT count(*) FROM order_lines WHERE org_id = $1`, p.buyer.org); n != 0 {
		t.Fatalf("the refused cart wrote %d lines", n)
	}
	if n := p.count(t, `SELECT count(*) FROM entitlements WHERE org_id = $1`, p.buyer.org); n != 0 {
		t.Fatalf("the refused cart granted %d rights", n)
	}
	vs, err := p.create(ctx, line(first, 2), line(quota, 1), line(last, 1))
	if err != nil || len(vs) != 1 || len(vs[0].Lines) != 3 {
		t.Fatalf("corrected cart under the same key = %d orders, %v", len(vs), err)
	}
	if p.sold(t, first) != 2 || p.sold(t, last) != 1 {
		t.Fatalf("units after the corrected cart = %d / %d", p.sold(t, first), p.sold(t, last))
	}
}

// TestCreateOrder_Cart_SeveralRefusedLines_ReservesNothing_Postgres
// (BE-139): a cart whose second and fourth lines are sold out goes on
// reserving past the first refusal in the same transaction (a refusal is
// no SQL error), names both lines in offering ID order under 409
// catalog.sold_out, keeps none of the units its other lines took, writes
// no order, and quoteOrder answers the same problem.
func TestCreateOrder_Cart_SeveralRefusedLines_ReservesNothing_Postgres(t *testing.T) {
	t.Parallel()
	p, _ := pgCart(t)
	first := p.cartOffering(t, p.seller, cartVAT{rate: 2000, class: "standard"}, 1_000_000, ptr(5))
	mid := p.cartOffering(t, p.seller, cartVAT{rate: 2000, class: "standard"}, 1_000_000, ptr(1))
	quota := p.platformOffering(t)
	last := p.cartOffering(t, p.seller, cartVAT{rate: 2000, class: "standard"}, 1_000_000, ptr(1))
	cart := []orders.LineInput{line(last, 2), line(first, 2), line(mid, 2), line(quota, 1)}
	want := []string{"body.lines[2].offering_id", "body.lines[0].offering_id"} // mid < last (UUIDv7)
	ctx := idempotency.WithKey(p.buyerActor(t), idempotency.Key{Scope: p.buyer.user.String() + ":POST /orgs/{org_id}/orders",
		Value: "pg-cart-several-refused", Status: 201})
	_, err := p.create(ctx, cart...)
	problemtest.WantProblem(t, err, 409, "catalog.sold_out")
	if f := problemFields(t, err); !slices.Equal(f, want) {
		t.Fatalf("fields = %v, want %v", f, want)
	}
	if c := problemCodes(t, err); !slices.Equal(c, []string{"catalog.sold_out", "catalog.sold_out"}) {
		t.Fatalf("codes = %v", c)
	}
	if p.sold(t, first) != 0 || p.sold(t, mid) != 0 || p.sold(t, last) != 0 {
		t.Fatalf("the refused cart kept units: %d / %d / %d", p.sold(t, first), p.sold(t, mid), p.sold(t, last))
	}
	if n := p.count(t, `SELECT count(*) FROM orders WHERE org_id = $1`, p.buyer.org); n != 0 {
		t.Fatalf("the refused cart wrote %d orders", n)
	}
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, qErr := p.mod.Service.QuoteOrder(ctx, a, p.buyer.org, orders.CreateInput{BuyerPartyID: p.buyer.party, Lines: cart})
	problemtest.WantProblem(t, qErr, 409, "catalog.sold_out")
	if f := problemFields(t, qErr); !slices.Equal(f, want) {
		t.Fatalf("quote fields = %v, want %v", f, want)
	}
}

// TestCreateOrder_Cart_RefusalsOfTwoReasons_CarryTheirCodes_Postgres
// (BE-140): a cart refused for two different reasons — one line sold out,
// another over its per-buyer limit — names both lines in offering ID
// order, each entry with its own refusal's code; the problem's code is
// errors[0]'s; quoteOrder answers the same entries and codes.
func TestCreateOrder_Cart_RefusalsOfTwoReasons_CarryTheirCodes_Postgres(t *testing.T) {
	t.Parallel()
	p, _ := pgCart(t)
	scarce := p.cartOffering(t, p.seller, cartVAT{rate: 2000, class: "standard"}, 1_000_000, ptr(1))
	limited := p.cartOffering(t, p.seller, cartVAT{rate: 2000, class: "standard"}, 1_000_000, nil) // scarce < limited (UUIDv7)
	pgExec(t, p.d, `UPDATE offerings SET per_buyer_limit = 1 WHERE id = $1`, limited)
	cart := []orders.LineInput{line(limited, 2), line(scarce, 2)}
	wantAt := []string{"body.lines[1].offering_id", "body.lines[0].offering_id"}
	wantCodes := []string{"catalog.sold_out", "catalog.per_buyer_limit"}
	ctx := idempotency.WithKey(p.buyerActor(t), idempotency.Key{Scope: p.buyer.user.String() + ":POST /orgs/{org_id}/orders",
		Value: "pg-cart-two-reasons", Status: 201})
	_, err := p.create(ctx, cart...)
	problemtest.WantProblem(t, err, 409, wantCodes[0])
	if f, c := problemFields(t, err), problemCodes(t, err); !slices.Equal(f, wantAt) || !slices.Equal(c, wantCodes) {
		t.Fatalf("errors = %v %v, want %v %v", f, c, wantAt, wantCodes)
	}
	if p.sold(t, scarce) != 0 || p.sold(t, limited) != 0 {
		t.Fatalf("the refused cart kept units: %d / %d", p.sold(t, scarce), p.sold(t, limited))
	}
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, qErr := p.mod.Service.QuoteOrder(ctx, a, p.buyer.org, orders.CreateInput{BuyerPartyID: p.buyer.party, Lines: cart})
	problemtest.WantProblem(t, qErr, 409, wantCodes[0])
	if f, c := problemFields(t, qErr), problemCodes(t, qErr); !slices.Equal(f, wantAt) || !slices.Equal(c, wantCodes) {
		t.Fatalf("quote errors = %v %v, want %v %v", f, c, wantAt, wantCodes)
	}
}

// TestCreateOrder_Cart_ReversedLines_Concurrent_Postgres (BE-131): carts of
// the same two offerings in opposite line orders run concurrently; each
// reserves in offering ID order, so none waits on another in a cycle — all
// are placed (no 40P01 deadlock answered as 409) and every unit is counted.
func TestCreateOrder_Cart_ReversedLines_Concurrent_Postgres(t *testing.T) {
	t.Parallel()
	p, s2 := pgCart(t)
	x := p.cartOffering(t, p.seller, cartVAT{rate: 2000, class: "standard"}, 1_000_000, nil)
	y := p.cartOffering(t, s2, cartVAT{rate: 1000, class: "standard"}, 500_000, nil)
	// A per-buyer limit makes each reservation read the buyer's units under
	// the row lock: the lock is held longer, a crossing more likely.
	pgExec(t, p.d, `UPDATE offerings SET per_buyer_limit = 1000 WHERE id IN ($1, $2)`, x, y)
	ctx := p.buyerActor(t)
	const carts = 12
	var wg sync.WaitGroup
	errs := make([]error, carts)
	start := make(chan struct{})
	for i := range carts {
		wg.Go(func() {
			<-start
			lines := []orders.LineInput{line(x, 1), line(y, 1)}
			if i%2 == 1 {
				lines[0], lines[1] = lines[1], lines[0]
			}
			_, errs[i] = p.create(ctx, lines...)
		})
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("cart %d: %v", i, err)
		}
	}
	if p.sold(t, x) != carts || p.sold(t, y) != carts {
		t.Fatalf("units sold = %d / %d, want %d each", p.sold(t, x), p.sold(t, y), carts)
	}
	if n := p.count(t, `SELECT count(*) FROM orders WHERE org_id = $1`, p.buyer.org); n != carts {
		t.Fatalf("orders = %d, want %d", n, carts)
	}
}
