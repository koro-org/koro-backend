package orders_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync"
	"testing"
	"uuid"

	"koro/internal/catalog"
	"koro/internal/orders"
	"koro/internal/parties"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/problem"
	"koro/internal/platform/problem/problemtest"
)

// cartLines are the offerings of the cart tests (BE-131, Q-25): every VAT
// profile a line can have, two sellers with their own payees, Koro's own
// product and a model C offering.
type cartLines struct {
	booth, reduced, exempt, withheld, quota, direct uuid.UUID
	payeeB                                          uuid.UUID
}

func (e *env) cartLines(t *testing.T) cartLines {
	t.Helper()
	c := cartLines{payeeB: e.payees.add("club_b", parties.PayeeActive)}
	clubB := func(o *catalog.Offering) { o.OrgID, o.DefaultPayeeID = org("club_b"), &c.payeeB }
	reducedSvc := e.service(t, "workshop_10", catalog.FulfillOrgBound, func(s *catalog.ServiceItem) { s.DefaultVATRateBP = 1000 })
	exemptSvc := e.service(t, "education_exempt", catalog.FulfillOrgBound, func(s *catalog.ServiceItem) {
		s.VATClass, s.VATExemptionCode = catalog.VATExempt, ptr("325")
	})
	withheldSvc := e.service(t, "cleaning_withheld", catalog.FulfillOrgBound, func(s *catalog.ServiceItem) {
		s.VATClass, s.VATWithholdingCode, s.VATWithholdingNum, s.VATWithholdingDen = catalog.VATWithheld, ptr("627"), ptr(int16(5)), ptr(int16(10))
	})
	c.booth = e.offering(t, nil)
	c.reduced = e.offering(t, func(o *catalog.Offering) {
		clubB(o)
		o.ServiceID, o.Title, o.UnitPriceAmount, o.VATRateBP = reducedSvc, "Atölye", 333_333, 1000
	})
	c.exempt = e.offering(t, func(o *catalog.Offering) {
		clubB(o)
		o.ServiceID, o.Title, o.UnitPriceAmount = exemptSvc, "Eğitim", 500_000
	})
	c.withheld = e.offering(t, func(o *catalog.Offering) {
		o.ServiceID, o.Title, o.UnitPriceAmount = withheldSvc, "Temizlik", 100_000
	})
	c.quota = e.platformOffering(t)
	c.direct = e.offering(t, nil)
	e.addPolicy(t, catalog.ScopeOffering, &c.direct, func(p *catalog.CommercePolicy) {
		p.ModelCode, p.CollectionMethod, p.CommissionPayer = catalog.ModelC, catalog.CollectDirectToPayee, catalog.PayerBuyer
	})
	return c
}

// lineOf returns the line of the offering in v.
func lineOf(t *testing.T, v orders.OrderView, offering uuid.UUID) orders.Line {
	t.Helper()
	i := slices.IndexFunc(v.Lines, func(l orders.Line) bool { return l.OfferingID == offering })
	if i < 0 {
		t.Fatalf("order %s has no line of offering %s", v.Order.OrderNo, offering)
	}
	return v.Lines[i]
}

// sumsMatch checks that the frozen order totals are the sums of its lines
// (orders_totals_match_lines) and what Koro collects (02 §18.3).
func sumsMatch(t *testing.T, v orders.OrderView) {
	t.Helper()
	var net, vat, withheld, fee, payable int64
	for _, l := range v.Lines {
		net, vat, withheld, fee, payable = net+l.NetAmount, vat+l.VATAmount, withheld+l.VATWithheldAmount, fee+l.BuyerFeeAmount, payable+l.PayableAmount
		if l.GrossAmount != l.NetAmount+l.VATAmount || l.PayableAmount != l.GrossAmount-l.VATWithheldAmount+l.BuyerFeeAmount {
			t.Errorf("line %s: gross/payable do not add up: %+v", l.TitleSnapshot, l)
		}
	}
	o := v.Order
	koro := payable
	if o.ModelCode == orders.ModelC {
		koro = fee
	}
	if o.SubtotalAmount != net || o.VATAmount != vat || o.VATWithheldAmount != withheld || o.BuyerFeeAmount != fee ||
		o.PayableAmount != payable || o.TotalAmount != net+vat+fee || o.KoroCollectAmount != koro ||
		o.KoroCollectAmount+o.DirectToPayeeAmount != o.PayableAmount {
		t.Errorf("order %s totals %+v, lines sum net %d vat %d withheld %d fee %d payable %d", o.OrderNo, o, net, vat, withheld, fee, payable)
	}
}

// TestCreateOrder_Cart_MixedVATSellersAndModels (Q-25, H-6, T-12): one cart
// of six offerings — 20 %, 10 %, exempt and withheld VAT, two sellers with
// their own payees, Koro's own quota and a model C offering — becomes one
// model A order of five lines (one reference code, one transfer to Koro)
// and one model C order. Every amount is priced per line in kuruş (half
// away from zero), each line keeps its offering's payee, the totals are
// the sums of the lines, and every line's stock is taken.
func TestCreateOrder_Cart_MixedVATSellersAndModels(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.cartLines(t)
	vs := e.place(t, line(c.booth, 2), line(c.reduced, 3), line(c.exempt, 1), line(c.direct, 1), line(c.withheld, 1), line(c.quota, 1))
	if len(vs) != 2 || vs[0].Order.ModelCode != orders.ModelA || vs[1].Order.ModelCode != orders.ModelC {
		t.Fatalf("orders = %d, want A then C", len(vs))
	}
	a, cc := vs[0], vs[1]
	if len(a.Lines) != 5 || len(cc.Lines) != 1 || a.Order.OrderNo == cc.Order.OrderNo {
		t.Fatalf("split: A %d lines (%s), C %d lines (%s)", len(a.Lines), a.Order.OrderNo, len(cc.Lines), cc.Order.OrderNo)
	}
	want := []uuid.UUID{c.booth, c.reduced, c.exempt, c.withheld, c.quota}
	for i, l := range a.Lines {
		if l.OfferingID != want[i] {
			t.Fatalf("line %d is offering %s, want the request order", i, l.OfferingID)
		}
	}
	for _, tc := range []struct {
		name                                    string
		offering                                uuid.UUID
		payee                                   *uuid.UUID
		seller                                  uuid.UUID
		net, rate, vat, withheld, payable, comm int64
		sellerNet                               int64
	}{
		// 2 × 1,000,000; 20 %; 10 % commission + 20 % VAT from the seller.
		{"booth", c.booth, &e.payeeA, org("club_a"), 2_000_000, 2000, 400_000, 0, 2_400_000, 200_000, 2_160_000},
		// 3 × 333,333 = 999,999; 10 % = 99,999.9 → 100,000; commission 99,999.9 → 100,000.
		{"reduced", c.reduced, &c.payeeB, org("club_b"), 999_999, 1000, 100_000, 0, 1_099_999, 100_000, 979_999},
		// Exempt: the effective rate is 0 whatever the offering says.
		{"exempt", c.exempt, &c.payeeB, org("club_b"), 500_000, 0, 0, 0, 500_000, 50_000, 440_000},
		// 20 % = 20,000, 5/10 withheld by the buyer: 110,000 payable.
		{"withheld", c.withheld, &e.payeeA, org("club_a"), 100_000, 2000, 20_000, 10_000, 110_000, 10_000, 98_000},
		// Koro's own product: no payee.
		{"quota", c.quota, nil, org("platform_org"), 250_000, 2000, 50_000, 0, 300_000, 25_000, 270_000},
	} {
		l := lineOf(t, a, tc.offering)
		switch {
		case (tc.payee == nil) != (l.PayeeID == nil) || tc.payee != nil && *l.PayeeID != *tc.payee || l.SellerOrgID != tc.seller:
			t.Errorf("%s: payee %v seller %s", tc.name, l.PayeeID, l.SellerOrgID)
		case l.NetAmount != tc.net || int64(l.VATRateBP) != tc.rate || l.VATAmount != tc.vat || l.VATWithheldAmount != tc.withheld ||
			l.PayableAmount != tc.payable || l.CommissionAmount != tc.comm || l.SellerNetAmount != tc.sellerNet:
			t.Errorf("%s: amounts %+v", tc.name, l)
		}
	}
	sumsMatch(t, a)
	sumsMatch(t, cc)
	// 3,849,999 net + 570,000 VAT − 10,000 withheld: one transfer to Koro.
	if a.Order.SubtotalAmount != 3_849_999 || a.Order.VATAmount != 570_000 || a.Order.PayableAmount != 4_409_999 {
		t.Fatalf("model A order = %+v", a.Order)
	}
	if cc.Order.KoroCollectAmount != 120_000 || cc.Order.DirectToPayeeAmount != 1_200_000 {
		t.Fatalf("model C order = %+v", cc.Order)
	}
	for off, qty := range map[uuid.UUID]int{c.booth: 2, c.reduced: 3, c.exempt: 1, c.withheld: 1, c.direct: 1} {
		if n := e.sold(t, off); n != qty {
			t.Errorf("offering %s: %d units sold, want %d", off, n, qty)
		}
	}
	if n := len(e.events(domainevent.TopicOrderPlaced)); n != 2 {
		t.Fatalf("order.placed = %d, want one per order", n)
	}
	// Each seller sees only its own lines of the shared order (03 k26).
	club := actor(t, "club_admin")
	sv, err := e.svc.GetOrder(ctxOf(t, club), club, a.Order.ID)
	if err != nil || !sv.SellerView || len(sv.Lines) != 2 {
		t.Fatalf("club_a's view = %d lines, %v", len(sv.Lines), err)
	}
	for _, l := range sv.Lines {
		if l.SellerOrgID != org("club_a") {
			t.Fatalf("club_a sees a line of %s", l.SellerOrgID)
		}
	}
}

// TestCreateOrder_Cart_RefusedLineIsNamed (BE-131): a refusal of one line
// of the cart keeps catalog's status and code (06 H-52) and names the line
// in errors[] (body.lines[i].offering_id); no order is placed. (Nothing
// stays reserved either: the placement is one transaction, pinned on
// PostgreSQL by TestCreateOrder_Cart_OneLineSoldOut_ReservesNothing_Postgres.)
func TestCreateOrder_Cart_RefusedLineIsNamed(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	app := newHTTP(t, e)
	path := "/api/v1/orgs/" + org("company_a").String() + "/orders"
	open := e.offering(t, func(o *catalog.Offering) { o.QuantityTotal = nil })
	draft := e.offering(t, func(o *catalog.Offering) {
		o.Status, o.ApprovedBy, o.ApprovedAt, o.SubmittedAt = catalog.OfferingDraft, nil, nil, nil
	})
	payeeB := e.payees.add("club_b", parties.PayeeSuspended)
	inactive := e.offering(t, func(o *catalog.Offering) { o.OrgID, o.DefaultPayeeID = org("club_b"), &payeeB })
	small := e.offering(t, func(o *catalog.Offering) { o.QuantityTotal = ptr(1) })
	n := 0
	for _, tc := range []struct {
		name   string
		lines  []uuid.UUID
		status int
		code   string
		at     string
	}{
		{"not on sale", []uuid.UUID{draft, open}, 409, "catalog.not_on_sale", "body.lines[0].offering_id"},
		{"one line per offering", []uuid.UUID{open, open, inactive}, 422, "validation_failed", "body.lines[1].offering_id"},
		{"inactive payee", []uuid.UUID{open, inactive}, 409, "catalog.payee_not_active", "body.lines[1].offering_id"},
		{"sold out", []uuid.UUID{open, small}, 409, "catalog.sold_out", "body.lines[1].offering_id"},
		{"unknown", []uuid.UUID{open, uuid.NewV7()}, 422, "validation_failed", "body.lines[1].offering_id"},
	} {
		n++
		lines := make([]map[string]any, len(tc.lines))
		for i, id := range tc.lines {
			lines[i] = map[string]any{"offering_id": id.String(), "quantity": 1}
		}
		if tc.code == "catalog.sold_out" {
			lines[1]["quantity"] = 2
		}
		rec, body := call(t, app, "company_admin", http.MethodPost, path, map[string]any{"buyer_party_id": e.partyA.String(), "lines": lines},
			map[string]string{"Idempotency-Key": "cart-refusal-" + string(rune('a'+n))})
		errs, _ := body["errors"].([]any)
		if rec.Code != tc.status || body["code"] != tc.code || len(errs) != 1 || errs[0].(map[string]any)["location"] != tc.at {
			t.Errorf("%s: %d %s", tc.name, rec.Code, rec.Body)
		}
	}
	if page, err := e.store.ListOrgOrders(t.Context(), org("company_a"), orders.OrderFilter{}, nil, 10); err != nil || len(page) != 0 {
		t.Fatalf("refused carts placed %d orders (%v)", len(page), err)
	}
	// The per-buyer limit names the line too (BuyerHeld counts the first order).
	limited := e.offering(t, func(o *catalog.Offering) { o.PerBuyerLimit = ptr(1) })
	e.place(t, line(limited, 1))
	a := actor(t, "company_admin")
	_, err := e.svc.CreateOrder(ctxOf(t, a), a, org("company_a"), orders.CreateInput{BuyerPartyID: e.partyA,
		Lines: []orders.LineInput{line(open, 1), line(limited, 1)}})
	problemtest.WantProblem(t, err, 409, "catalog.per_buyer_limit")
	if f := problemFields(t, err); len(f) != 1 || f[0] != "body.lines[1].offering_id" {
		t.Fatalf("per-buyer limit fields = %v", f)
	}
}

// problemFields returns the locations of the problem's field errors.
func problemFields(t *testing.T, err error) []string {
	t.Helper()
	pe, ok := errors.AsType[*problem.Error](err)
	if !ok {
		t.Fatalf("not a problem: %v", err)
	}
	var out []string
	for _, f := range pe.Fields() {
		out = append(out, f.Location)
	}
	return out
}

// lockRecorder is catalog's Offerings recording the order in which the
// offering rows are locked (Reserve, Release).
type lockRecorder struct {
	orders.Offerings
	mu    sync.Mutex
	locks []uuid.UUID
}

func (r *lockRecorder) Reserve(ctx context.Context, offeringID uuid.UUID, qty int, buyer uuid.UUID) error {
	r.mu.Lock()
	r.locks = append(r.locks, offeringID)
	r.mu.Unlock()
	return r.Offerings.Reserve(ctx, offeringID, qty, buyer)
}

func (r *lockRecorder) Release(ctx context.Context, offeringID uuid.UUID, qty int) error {
	r.mu.Lock()
	r.locks = append(r.locks, offeringID)
	r.mu.Unlock()
	return r.Offerings.Release(ctx, offeringID, qty)
}

func (r *lockRecorder) take() []uuid.UUID {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.locks
	r.locks = nil
	return out
}

// TestCreateOrder_Cart_LocksOfferingsInIDOrder (BE-131): a cart reserves
// its offerings in offering ID order whatever the order of its lines, and
// the end of an unpaid order (cancel, expiry) releases them in the same
// order, so carts that share offerings never deadlock; the lines keep the
// request order.
func TestCreateOrder_Cart_LocksOfferingsInIDOrder(t *testing.T) {
	t.Parallel()
	rec := &lockRecorder{}
	e := newEnv(t, func(d *orders.Deps) { rec.Offerings = d.Offerings; d.Offerings = rec })
	ids := []uuid.UUID{e.offering(t, nil), e.offering(t, nil), e.offering(t, nil)}
	sorted := slices.Clone(ids)
	slices.SortFunc(sorted, func(a, b uuid.UUID) int { return a.Compare(b) })
	v := e.place(t, line(ids[2], 1), line(ids[0], 1), line(ids[1], 1))[0]
	if got := rec.take(); !slices.Equal(got, sorted) {
		t.Fatalf("reservations in %v, want offering ID order %v", got, sorted)
	}
	if v.Lines[0].OfferingID != ids[2] || v.Lines[1].OfferingID != ids[0] || v.Lines[2].OfferingID != ids[1] {
		t.Fatal("the lines lost the request order")
	}
	a := actor(t, "company_admin")
	if _, err := e.svc.CancelOrder(ctxOf(t, a), a, v.Order.ID, "Sepet değişti"); err != nil {
		t.Fatal(err)
	}
	if got := rec.take(); !slices.Equal(got, sorted) {
		t.Fatalf("releases in %v, want offering ID order %v", got, sorted)
	}
	for _, id := range ids {
		if n := e.sold(t, id); n != 0 {
			t.Fatalf("offering %s keeps %d units after the cancellation", id, n)
		}
	}
}

// TestOpenCommerce_CartLineRefundDue_KeepsBuyerOpen (BE-131): a cart whose
// other lines were delivered completes while a cancelled line still waits
// for its refund; the buyer company cannot be closed before Koro gave the
// money back (the same rule as RefundsDue).
func TestOpenCommerce_CartLineRefundDue_KeepsBuyerOpen(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	kept, dropped := e.offering(t, nil), e.offering(t, nil)
	v := e.place(t, line(kept, 1), line(dropped, 1))[0]
	e.pay(t, v.Order.ID)
	club := actor(t, "club_admin")
	if _, err := e.catalog.Service.CancelOffering(ctxOf(t, club), club, dropped, "Etkinlik ertelendi"); err != nil {
		t.Fatal(err)
	}
	e.deliver(t, domainevent.TopicOfferingCancelled)
	k := lineOf(t, v, kept)
	e.mark(t, k.ID)
	a := actor(t, "company_admin")
	if _, err := e.svc.ConfirmLine(ctxOf(t, a), a, k.ID); err != nil {
		t.Fatal(err)
	}
	if o := e.order(t, v.Order.ID); o.Status != orders.StatusCompleted {
		t.Fatalf("order = %s, want completed (one line delivered, one cancelled)", o.Status)
	}
	due, err := e.mod.Orders.RefundsDue(t.Context(), 10)
	if err != nil || len(due) != 1 || *due[0].LineID != lineOf(t, v, dropped).ID {
		t.Fatalf("refunds due = %+v, %v", due, err)
	}
	if ok, err := e.mod.OpenCommerce(t.Context(), org("company_a")); err != nil || !ok {
		t.Fatalf("buyer with a refund due: open = %v, %v", ok, err)
	}
	if ok, _ := e.mod.OpenCommerce(t.Context(), org("company_b")); ok {
		t.Fatal("another company has open commerce")
	}
}
