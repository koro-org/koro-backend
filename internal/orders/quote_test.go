package orders_test

import (
	"net/http"
	"reflect"
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/catalog"
	"koro/internal/entitlements"
	"koro/internal/orders"
	"koro/internal/orgs"
	"koro/internal/parties"
	"koro/internal/platform/problem/problemtest"
)

// quote previews the lines as company_admin for company_a, billed to party A.
func (e *env) quote(t *testing.T, lines ...orders.LineInput) []orders.OrderView {
	t.Helper()
	a := actor(t, "company_admin")
	vs, err := e.svc.QuoteOrder(ctxOf(t, a), a, org("company_a"), orders.CreateInput{BuyerPartyID: e.partyA, Lines: lines})
	if err != nil {
		t.Fatalf("QuoteOrder: %v", err)
	}
	return vs
}

// frozen is an order or a line without what only a write gives it: its ID,
// its reference code, its row times (and the time zone a database read
// gives the instants).
func frozenOrder(o orders.Order) orders.Order {
	o.ID, o.OrderNo, o.CreatedAt, o.UpdatedAt = uuid.Nil(), "", time.Time{}, time.Time{}
	o.ExpiresAt, o.PlacedAt = o.ExpiresAt.UTC(), o.PlacedAt.UTC()
	return o
}

func frozenLine(l orders.Line) orders.Line {
	l.ID, l.OrderID, l.CreatedAt, l.UpdatedAt = uuid.Nil(), uuid.Nil(), time.Time{}, time.Time{}
	return l
}

// wroteNothing checks that e holds no order, sold unit, right, audit
// record, event or Idempotency-Key.
func (e *env) wroteNothing(t *testing.T, offerings ...uuid.UUID) {
	t.Helper()
	if page, err := e.store.ListOrgOrders(t.Context(), org("company_a"), orders.OrderFilter{}, nil, 10); err != nil || len(page) != 0 {
		t.Fatalf("orders = %d (%v), want none", len(page), err)
	}
	for _, off := range offerings {
		if n := e.sold(t, off); n != 0 {
			t.Errorf("offering %s: %d units sold, want none", off, n)
		}
	}
	if es, err := e.entStore.List(t.Context(), org("company_a"), entitlements.ListFilter{}, nil, 10); err != nil || len(es) != 0 {
		t.Errorf("rights = %d (%v), want none", len(es), err)
	}
	if a := e.audit.All(); len(a) != 0 {
		t.Errorf("audit records %v, want none", e.audit.Actions())
	}
	if ev := e.outbox.Events(); len(ev) != 0 {
		t.Errorf("events = %d, want none", len(ev))
	}
}

// TestQuoteOrder_SameCart_EqualsPlacement (BE-136): the quote of the mixed
// cart (20 %, 10 %, exempt and withheld VAT, two sellers and payees,
// Koro's own quota, a model C line) is the placement of the same cart,
// field by field — the split into one order per model, every frozen line
// amount (net, VAT, withholding, gross, commission, buyer fee, seller net,
// payable), payee, policy, title and deliverable, every order total
// (koro_collect, direct_to_payee) — except what only a write gives: IDs,
// the reference code, the row times. The quote itself writes nothing.
func TestQuoteOrder_SameCart_EqualsPlacement(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.cartLines(t)
	cart := []orders.LineInput{line(c.booth, 2), line(c.reduced, 3), line(c.exempt, 1), line(c.direct, 1), line(c.withheld, 1), line(c.quota, 1)}
	qs := e.quote(t, cart...)
	e.wroteNothing(t, c.booth, c.reduced, c.exempt, c.direct, c.withheld)
	again := e.quote(t, cart...) // a quote takes no stock: the same cart quotes the same
	vs := e.place(t, cart...)
	if len(qs) != 2 || len(vs) != 2 || len(again) != 2 {
		t.Fatalf("quote %d orders, again %d, placement %d; want A and C", len(qs), len(again), len(vs))
	}
	for i := range vs {
		q, p := qs[i], vs[i]
		if got, want := frozenOrder(q.Order), frozenOrder(p.Order); !reflect.DeepEqual(got, want) {
			t.Errorf("order %d: quote %+v\nplacement %+v", i, got, want)
		}
		if !reflect.DeepEqual(frozenOrder(again[i].Order), frozenOrder(p.Order)) {
			t.Errorf("order %d: the second quote differs", i)
		}
		if q.Order.OrderNo != "" || len(q.Lines) != len(p.Lines) {
			t.Fatalf("order %d: quote has reference %q and %d lines, placement %d", i, q.Order.OrderNo, len(q.Lines), len(p.Lines))
		}
		for j := range p.Lines {
			if got, want := frozenLine(q.Lines[j]), frozenLine(p.Lines[j]); !reflect.DeepEqual(got, want) {
				t.Errorf("order %d line %d: quote %+v\nplacement %+v", i, j, got, want)
			}
		}
		if !reflect.DeepEqual(q.Sellers, p.Sellers) || q.SellerView || q.DirectStatus != "" {
			t.Errorf("order %d: quote sellers %v view %v/%q, placement %v", i, q.Sellers, q.SellerView, q.DirectStatus, p.Sellers)
		}
		sumsMatch(t, q)
	}
}

// TestQuoteOrder_RefusalsMatchCreateOrder (BE-136): every refusal of a cart
// line is the one createOrder answers — status, code and the line named in
// errors[] (body.lines[i].offering_id) — including the stock the quote only
// reads: more units than are left (catalog.sold_out) and the per-buyer
// limit counting the buyer's open orders (catalog.per_buyer_limit), with
// the lines named in the order the placement names them, and no policy in
// effect. A cart of several refused lines names every one, under the first
// refusal's status and code (BE-139). A refused quote writes nothing either.
func TestQuoteOrder_RefusalsMatchCreateOrder(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	open := e.offering(t, func(o *catalog.Offering) { o.QuantityTotal = nil })
	draft := e.offering(t, func(o *catalog.Offering) {
		o.Status, o.ApprovedBy, o.ApprovedAt, o.SubmittedAt = catalog.OfferingDraft, nil, nil, nil
	})
	payeeB := e.payees.add("club_b", parties.PayeeSuspended)
	inactive := e.offering(t, func(o *catalog.Offering) { o.OrgID, o.DefaultPayeeID = org("club_b"), &payeeB })
	// small < tiny (UUIDv7): with both sold out, the stock of small is
	// reserved (placement) or read (quote) first, wherever its line is.
	small := e.offering(t, func(o *catalog.Offering) { o.QuantityTotal = ptr(1) })
	tiny := e.offering(t, func(o *catalog.Offering) { o.QuantityTotal = ptr(1) })
	limited := e.offering(t, func(o *catalog.Offering) { o.PerBuyerLimit = ptr(2) })
	unknown := uuid.NewV7()
	a := actor(t, "company_admin")
	ctx := ctxOf(t, a)
	cases := []struct {
		name  string
		lines []orders.LineInput
		at    []string
	}{
		{"not on sale", []orders.LineInput{line(draft, 1), line(open, 1)}, []string{"body.lines[0].offering_id"}},
		{"one line per offering", []orders.LineInput{line(open, 1), line(open, 1)}, []string{"body.lines[1].offering_id"}},
		{"inactive payee", []orders.LineInput{line(open, 1), line(inactive, 1)}, []string{"body.lines[1].offering_id"}},
		{"more units than are left", []orders.LineInput{line(open, 1), line(small, 2)}, []string{"body.lines[1].offering_id"}},
		{"two lines sold out", []orders.LineInput{line(tiny, 2), line(open, 1), line(small, 2)}, []string{"body.lines[2].offering_id", "body.lines[0].offering_id"}},
		{"over the per-buyer limit", []orders.LineInput{line(limited, 3)}, []string{"body.lines[0].offering_id"}},
		{"unknown offering", []orders.LineInput{line(open, 1), line(uuid.NewV7(), 1)}, []string{"body.lines[1].offering_id"}},
		// BE-139: every refused line is named — the lines that cannot be
		// priced in request order, then stock in offering ID order.
		{"every refused line", []orders.LineInput{line(draft, 1), line(open, 1), line(inactive, 1), line(limited, 3), line(small, 2), line(unknown, 1)},
			[]string{"body.lines[0].offering_id", "body.lines[2].offering_id", "body.lines[5].offering_id", "body.lines[4].offering_id", "body.lines[3].offering_id"}},
		{"quantity out of range", []orders.LineInput{line(open, 1001)}, []string{"body.lines[0].quantity"}},
		{"no line", nil, []string{"body.lines"}},
	}
	quoted := make([]error, len(cases))
	for i, tc := range cases {
		_, quoted[i] = e.svc.QuoteOrder(ctx, a, org("company_a"), orders.CreateInput{BuyerPartyID: e.partyA, Lines: tc.lines})
		if quoted[i] == nil {
			t.Fatalf("%s: quote accepted", tc.name)
		}
		if f := problemFields(t, quoted[i]); !slices.Equal(f, tc.at) {
			t.Errorf("%s: quote names %v, want %v", tc.name, f, tc.at)
		}
	}
	e.wroteNothing(t, open, small, tiny, limited)
	// The cart's problem is its first refusal's (status, code, errors[0]).
	problemtest.WantProblem(t, quoted[len(cases)-3], 409, "catalog.not_on_sale")
	for i, tc := range cases {
		_, err := e.svc.CreateOrder(ctx, a, org("company_a"), orders.CreateInput{BuyerPartyID: e.partyA, Lines: tc.lines})
		if err == nil {
			t.Fatalf("%s: placement accepted", tc.name)
		}
		qs, qc := problemtest.StatusCode(quoted[i])
		ps, pc := problemtest.StatusCode(err)
		if qs != ps || qc != pc || !slices.Equal(problemFields(t, quoted[i]), problemFields(t, err)) {
			t.Errorf("%s: quote %d %s %v, placement %d %s %v", tc.name, qs, qc, problemFields(t, quoted[i]), ps, pc, problemFields(t, err))
		}
	}
	// The per-buyer limit counts the units the buyer holds on its open orders.
	e.place(t, line(limited, 1))
	if vs := e.quote(t, line(limited, 1)); len(vs) != 1 {
		t.Fatal("one more unit is within the limit")
	}
	_, err := e.svc.QuoteOrder(ctx, a, org("company_a"), orders.CreateInput{BuyerPartyID: e.partyA, Lines: []orders.LineInput{line(open, 1), line(limited, 2)}})
	problemtest.WantProblem(t, err, 409, "catalog.per_buyer_limit")
	if f := problemFields(t, err); len(f) != 1 || f[0] != "body.lines[1].offering_id" {
		t.Fatalf("per-buyer limit fields = %v", f)
	}

	// No policy in effect (the env's default starts a day before t0).
	n := newEnv(t)
	n.clk.Set(t0.Add(-48 * time.Hour))
	early := n.offering(t, func(o *catalog.Offering) {
		o.SubmittedAt, o.ApprovedAt = ptr(t0.Add(-72*time.Hour)), ptr(t0.Add(-72*time.Hour))
	})
	in := orders.CreateInput{BuyerPartyID: n.partyA, Lines: []orders.LineInput{line(early, 1)}}
	_, qErr := n.svc.QuoteOrder(ctx, a, org("company_a"), in)
	problemtest.WantProblem(t, qErr, 409, "catalog.no_commerce_policy")
	_, pErr := n.svc.CreateOrder(ctx, a, org("company_a"), in)
	problemtest.WantProblem(t, pErr, 409, "catalog.no_commerce_policy")
	if f, g := problemFields(t, qErr), problemFields(t, pErr); !slices.Equal(f, g) || len(f) != 1 {
		t.Fatalf("no policy: quote names %v, placement %v", f, g)
	}
}

// TestQuoteOrder_CheckoutRules (BE-136): the quote is createOrder's
// checkout — a recruiter is 403 (no purchase, no preview), a company admin
// without an MFA session 403 authz.mfa_required, another company's admin
// 404, the k25 party 422, a suspended company 403 — and the cell is its
// own action (orders.order.quote).
func TestQuoteOrder_CheckoutRules(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	off := e.offering(t, nil)
	in := orders.CreateInput{BuyerPartyID: e.partyA, Lines: []orders.LineInput{line(off, 1)}}
	quote := func(persona string, mut func(*orders.CreateInput)) error {
		var a authz.Actor
		if persona == "company_admin_no_mfa" {
			a = actor(t, "company_admin")
			a.MFAVerified = false
		} else {
			a = actor(t, persona)
		}
		x := in
		if mut != nil {
			mut(&x)
		}
		_, err := e.svc.QuoteOrder(ctxOf(t, a), a, org("company_a"), x)
		return err
	}
	problemtest.WantErr(t, quote("company_member", nil), authz.ErrForbidden)
	problemtest.WantErr(t, quote("company_admin_no_mfa", nil), authz.ErrMFARequired)
	problemtest.WantErr(t, quote("other_company_member", nil), orders.ErrNotFound)
	other := e.parties.add(buyerParty("company_b", parties.StatusVerified))
	problemtest.WantProblem(t, quote("company_admin", func(x *orders.CreateInput) { x.BuyerPartyID = other }), 422, "orders.buyer_party_required")
	unverified := e.parties.add(buyerParty("company_a", parties.StatusPendingVerification))
	problemtest.WantProblem(t, quote("company_admin", func(x *orders.CreateInput) { x.BuyerPartyID = unverified }), 422, "orders.buyer_party_required")
	e.orgs.setStatus("company_a", orgs.StatusSuspended)
	problemtest.WantProblem(t, quote("company_admin", nil), 403, "orgs.org_suspended")
	e.wroteNothing(t, off)
}

// TestQuoteOrder_HTTP_SameAmountsAsCreateOrder (BE-136): POST
// /orgs/{org_id}/orders/quote answers 200 with the orders createOrder then
// places — every field of the quote has the same name and value in
// createOrder's answer — without an Idempotency-Key, never shows the
// commission or the seller's net share, and a refused line is named as
// createOrder names it.
func TestQuoteOrder_HTTP_SameAmountsAsCreateOrder(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	app := newHTTP(t, e)
	c := e.cartLines(t)
	base := "/api/v1/orgs/" + org("company_a").String() + "/orders"
	body := map[string]any{"buyer_party_id": e.partyA.String(), "lines": []map[string]any{
		{"offering_id": c.booth.String(), "quantity": 2}, {"offering_id": c.direct.String(), "quantity": 1},
		{"offering_id": c.withheld.String(), "quantity": 1}, {"offering_id": c.quota.String(), "quantity": 3},
	}}
	rec, quoted := call(t, app, "company_admin", http.MethodPost, base+"/quote", body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("quote = %d %s", rec.Code, rec.Body)
	}
	if n := len(e.outbox.Events()); n != 0 || e.sold(t, c.booth) != 0 {
		t.Fatalf("the quote wrote: %d events, %d units", n, e.sold(t, c.booth))
	}
	rec, placed := call(t, app, "company_admin", http.MethodPost, base, body, map[string]string{"Idempotency-Key": "quote-then-place"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("createOrder = %d %s", rec.Code, rec.Body)
	}
	qos, pos := quoted["orders"].([]any), placed["orders"].([]any)
	if len(qos) != 2 || len(pos) != 2 {
		t.Fatalf("quote %d orders, placement %d", len(qos), len(pos))
	}
	for i := range pos {
		q, p := qos[i].(map[string]any), pos[i].(map[string]any)
		for k, v := range q {
			if k == "lines" {
				continue
			}
			if !reflect.DeepEqual(v, p[k]) {
				t.Errorf("order %d %s: quote %v, placement %v", i, k, v, p[k])
			}
		}
		ql, pl := q["lines"].([]any), p["lines"].([]any)
		if len(ql) != len(pl) {
			t.Fatalf("order %d: quote %d lines, placement %d", i, len(ql), len(pl))
		}
		for j := range pl {
			for k, v := range ql[j].(map[string]any) {
				if !reflect.DeepEqual(v, pl[j].(map[string]any)[k]) {
					t.Errorf("order %d line %d %s: quote %v, placement %v", i, j, k, v, pl[j].(map[string]any)[k])
				}
			}
			for _, hidden := range []string{"commission_amount", "commission_vat_amount", "seller_net_amount", "payee_id", "line_id"} {
				if _, ok := ql[j].(map[string]any)[hidden]; ok {
					t.Errorf("order %d line %d: the quote shows %s", i, j, hidden)
				}
			}
		}
		for _, absent := range []string{"order_id", "order_no", "status", "expires_at", "buyer_snapshot"} {
			if _, ok := q[absent]; ok {
				t.Errorf("order %d: the quote has %s", i, absent)
			}
		}
	}
	// The amounts the cart shows: model A booth line 2 × 1,000,000 + 20 % VAT; model C buyer fee.
	a := qos[0].(map[string]any)
	if a["model_code"] != "A" || a["payable_amount"] != float64(2_400_000+110_000+3*300_000) {
		t.Fatalf("model A quote = %v", a)
	}
	if l := a["lines"].([]any)[0].(map[string]any); l["title_snapshot"] != "Bahar Şenliği Stant Paketi" || l["unit_price_amount"] != float64(1_000_000) ||
		l["vat_rate_bp"] != float64(2000) || l["seller"].(map[string]any)["name"] != "Org club_a" {
		t.Fatalf("quote line = %v", l)
	}
	// A refused line is named as createOrder names it.
	body["lines"] = []map[string]any{{"offering_id": c.booth.String(), "quantity": 1}, {"offering_id": c.booth.String(), "quantity": 1}}
	rec, refused := call(t, app, "company_admin", http.MethodPost, base+"/quote", body, nil)
	errs, _ := refused["errors"].([]any)
	if rec.Code != 422 || refused["code"] != "validation_failed" || len(errs) != 1 || errs[0].(map[string]any)["location"] != "body.lines[1].offering_id" {
		t.Fatalf("refused quote = %d %s", rec.Code, rec.Body)
	}
}
