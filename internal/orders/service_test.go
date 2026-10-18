package orders_test

import (
	"errors"
	"regexp"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/catalog"
	"koro/internal/entitlements"
	"koro/internal/orders"
	"koro/internal/orgs"
	"koro/internal/parties"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
)

var orderNoRe = regexp.MustCompile(`^KR[0-9A-HJKMNP-TV-Z]{8}$`)

// TestCreateOrder_PlacesOneOrder pins the placement chain of 01 §6.3: an
// awaiting_payment order of company_a with the 02 example amounts, the
// payee and policy frozen on its line, the billed party's snapshot, the
// payment window, one unit reserved, the audit record and order.placed (O).
func TestCreateOrder_PlacesOneOrder(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	off := e.offering(t, nil)
	vs := e.place(t, line(off, 1))
	if len(vs) != 1 {
		t.Fatalf("orders = %d, want 1", len(vs))
	}
	o, l := vs[0].Order, vs[0].Lines[0]
	switch {
	case o.Status != orders.StatusAwaitingPayment || o.OrgID != org("company_a") || o.PlacedByUserID != org("company_admin"):
		t.Fatalf("order = %+v", o)
	case o.SubtotalAmount != 1_000_000 || o.VATAmount != 200_000 || o.TotalAmount != 1_200_000 || o.PayableAmount != 1_200_000 ||
		o.KoroCollectAmount != 1_200_000 || o.DirectToPayeeAmount != 0 || o.PaidAmount != 0:
		t.Fatalf("order amounts = %+v", o)
	case !o.ExpiresAt.Equal(t0.Add(orders.DefaultPaymentWindow)) || o.ModelCode != orders.ModelA || o.CollectionMethod != orders.CollectBankTransfer:
		t.Fatalf("order window / model = %s / %s %s", o.ExpiresAt, o.ModelCode, o.CollectionMethod)
	case o.BuyerSnapshot.Title != "Örnek Teknoloji A.Ş." || o.BuyerSnapshot.TaxNo != "1234567890" || o.BuyerSnapshot.Address.City != "İstanbul":
		t.Fatalf("buyer snapshot = %+v", o.BuyerSnapshot)
	case l.PayeeID == nil || *l.PayeeID != e.payeeA || l.SellerOrgID != org("club_a") || l.CommercePolicyID != e.policy.ID:
		t.Fatalf("line parties = %+v", l)
	case l.SellerNetAmount != 1_080_000 || l.CommissionAmount != 100_000 || l.CommissionVATAmount != 20_000 ||
		l.FulfillmentStatus != orders.FulfillPending || l.DisputeWindowDays != 7:
		t.Fatalf("line amounts = %+v", l)
	case len(l.DeliverablesSnapshot) != 1 || l.DeliverablesSnapshot[0].Key != "booth_3x3" || l.TitleSnapshot != "Bahar Şenliği Stant Paketi":
		t.Fatalf("line snapshot = %+v", l)
	}
	if n := e.sold(t, off); n != 1 {
		t.Errorf("units sold = %d, want 1", n)
	}
	if got := e.audit.of(authz.OrdersOrderCreate); len(got) != 1 || *got[0].ResourceID != o.ID {
		t.Errorf("audit = %+v", got)
	}
	placed := e.events(domainevent.TopicOrderPlaced)
	if len(placed) != 1 || placed[0].AggregateID != o.ID {
		t.Fatalf("order.placed events = %+v", placed)
	}
	if p, err := domainevent.Decode[domainevent.OrderPlaced](placed[0]); err != nil || p.OrgID != o.OrgID || p.ModelCode != "A" {
		t.Fatalf("order.placed payload = %+v, %v", p, err)
	}
}

// TestCreateOrder_ServerSidePricing_IgnoresClientAmounts (06 H-6): the
// order's amounts come from the offering and the policy, never from the
// request — the service input has no amount at all, and an HTTP body that
// smuggles amounts is refused by the schema (no unknown fields) without
// placing anything; the same body without them is priced by the server.
func TestCreateOrder_ServerSidePricing_IgnoresClientAmounts(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	off := e.offering(t, func(o *catalog.Offering) { o.UnitPriceAmount = 333_333; o.QuantityTotal = nil })
	app := newHTTP(t, e)
	path := "/api/v1/orgs/" + org("company_a").String() + "/orders"
	smuggled := map[string]any{"buyer_party_id": e.partyA.String(), "total_amount": 1, "payable_amount": 1,
		"lines": []map[string]any{{"offering_id": off.String(), "quantity": 2, "unit_price_amount": 1, "vat_amount": 0}}}
	rec, body := call(t, app, "company_admin", "POST", path, smuggled, map[string]string{"Idempotency-Key": "smuggle-amounts-1"})
	if rec.Code != 422 {
		t.Fatalf("smuggled amounts: status %d, want 422 (body %v)", rec.Code, body)
	}
	if n := e.sold(t, off); n != 0 {
		t.Fatalf("a refused request reserved %d units", n)
	}
	clean := map[string]any{"buyer_party_id": e.partyA.String(), "lines": []map[string]any{{"offering_id": off.String(), "quantity": 2}}}
	rec, body = call(t, app, "company_admin", "POST", path, clean, map[string]string{"Idempotency-Key": "clean-amounts-1"})
	if rec.Code != 201 {
		t.Fatalf("clean body: status %d (body %v)", rec.Code, body)
	}
	got := body["orders"].([]any)[0].(map[string]any)
	// 2 × 333,333 = 666,666; VAT 20 % = 133,333.2 → 133,333; payable 799,999.
	if got["subtotal_amount"].(float64) != 666_666 || got["vat_amount"].(float64) != 133_333 || got["payable_amount"].(float64) != 799_999 {
		t.Fatalf("server amounts = %v", got)
	}
	ln := got["lines"].([]any)[0].(map[string]any)
	if _, ok := ln["commission_amount"]; ok {
		t.Fatalf("the buyer's line shows the commission: %v", ln)
	}
	if _, ok := ln["seller_net_amount"]; ok {
		t.Fatalf("the buyer's line shows the seller's share: %v", ln)
	}
	if got["view"] != "buyer" || !orderNoRe.MatchString(got["order_no"].(string)) {
		t.Fatalf("order DTO = %v", got)
	}
}

// TestCreateOrder_SameIdempotencyKey_SingleOrderSingleReservation (06 H-5):
// the key is stored in the placement transaction, so a retry replays the
// first orders — one order, one reserved unit — and the same key with
// another body is 422 idempotency.key_reused.
func TestCreateOrder_SameIdempotencyKey_SingleOrderSingleReservation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	off := e.offering(t, nil)
	a := actor(t, "company_admin")
	ctx := idempotency.WithKey(ctxOf(t, a), idempotency.Key{Scope: a.UserID.String() + ":POST /orgs/{org_id}/orders", Value: "order-key-1", Status: 201})
	in := orders.CreateInput{BuyerPartyID: e.partyA, Lines: []orders.LineInput{line(off, 1)}}
	first, err := e.svc.CreateOrder(ctx, a, org("company_a"), in)
	if err != nil {
		t.Fatal(err)
	}
	again, err := e.svc.CreateOrder(ctx, a, org("company_a"), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 1 || again[0].Order.ID != first[0].Order.ID {
		t.Fatalf("replay = %v, want order %s", again, first[0].Order.ID)
	}
	page, err := e.store.ListOrgOrders(t.Context(), org("company_a"), orders.OrderFilter{}, nil, 10)
	if err != nil || len(page) != 1 {
		t.Fatalf("orders of company_a = %d (%v), want 1", len(page), err)
	}
	if n := e.sold(t, off); n != 1 {
		t.Fatalf("units reserved = %d, want 1", n)
	}
	if n := len(e.events(domainevent.TopicOrderPlaced)); n != 1 {
		t.Fatalf("order.placed events = %d, want 1", n)
	}
	in.Lines[0].Quantity = 2
	_, err = e.svc.CreateOrder(ctx, a, org("company_a"), in)
	wantProblem(t, err, 422, "idempotency.key_reused")
}

// TestCreateOrder_RecruiterRole_403 (03 §5.15, §1.1: company_member has no
// purchase): a recruiter of the company is refused before anything is read.
func TestCreateOrder_RecruiterRole_403(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	off := e.offering(t, nil)
	a := actor(t, "company_member")
	_, err := e.svc.CreateOrder(ctxOf(t, a), a, org("company_a"), orders.CreateInput{BuyerPartyID: e.partyA, Lines: []orders.LineInput{line(off, 1)}})
	wantErr(t, err, authz.ErrForbidden)
	wantProblem(t, err, 403, "forbidden")
	if n := e.sold(t, off); n != 0 {
		t.Fatalf("a refused order reserved %d units", n)
	}
	// Another company's admin does not even see company_a (IDOR: 404).
	b := actor(t, "other_company_member")
	_, err = e.svc.CreateOrder(ctxOf(t, b), b, org("company_a"), orders.CreateInput{BuyerPartyID: e.partyA, Lines: []orders.LineInput{line(off, 1)}})
	wantErr(t, err, authz.ErrNotFound)
}

// TestCreateOrder_CompanyAdminWithoutMFA_403 (03 §6.4: company_admin is an
// MFA role): a session without MFA is 403 authz.mfa_required.
func TestCreateOrder_CompanyAdminWithoutMFA_403(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	off := e.offering(t, nil)
	a := actor(t, "company_admin")
	a.MFAVerified = false
	_, err := e.svc.CreateOrder(ctxOf(t, a), a, org("company_a"), orders.CreateInput{BuyerPartyID: e.partyA, Lines: []orders.LineInput{line(off, 1)}})
	wantErr(t, err, authz.ErrMFARequired)
	wantProblem(t, err, 403, "authz.mfa_required")
	// The owner holds company_admin too (k-own adds rights, never removes them).
	owner := actor(t, "company_owner_a")
	if _, err := e.svc.CreateOrder(ctxOf(t, owner), owner, org("company_a"),
		orders.CreateInput{BuyerPartyID: e.partyA, Lines: []orders.LineInput{line(off, 1)}}); err != nil {
		t.Fatalf("company_owner: %v", err)
	}
}

// TestCreateOrder_K25 pins 03 k25: an active company and a verified
// buyer_company party of the company itself (422
// orders.buyer_party_required), the company states of 03 §2.3.
func TestCreateOrder_K25(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	off := e.offering(t, func(o *catalog.Offering) { o.QuantityTotal = nil })
	a := actor(t, "company_admin")
	try := func(party uuid.UUID) error {
		_, err := e.svc.CreateOrder(ctxOf(t, a), a, org("company_a"), orders.CreateInput{BuyerPartyID: party, Lines: []orders.LineInput{line(off, 1)}})
		return err
	}
	pending := e.parties.add(buyerParty("company_a", parties.StatusPendingVerification))
	foreign := e.parties.add(buyerParty("company_b", parties.StatusVerified))
	payeeParty := e.parties.add(parties.LegalParty{Kind: parties.KindPayee, LegalType: parties.LegalFoundation, Title: "Vakıf",
		Status: parties.StatusVerified})
	for name, party := range map[string]uuid.UUID{"unverified": pending, "another company's": foreign, "a payee party": payeeParty,
		"unknown": uuid.NewV7()} {
		if s, c := problemOf(try(party)); s != 422 || c != "orders.buyer_party_required" {
			t.Errorf("%s party: %d %s, want 422 orders.buyer_party_required", name, s, c)
		}
	}
	for st, code := range map[orgs.Status]string{orgs.StatusPendingReview: "orgs.org_not_verified",
		orgs.StatusSuspended: "orgs.org_suspended", orgs.StatusClosed: "orgs.invalid_state"} {
		e.orgs.setStatus("company_a", st)
		_, s := problemOf(try(e.partyA))
		if s != code {
			t.Errorf("company %s: code %s, want %s", st, s, code)
		}
	}
	e.orgs.setStatus("company_a", orgs.StatusActive)
	if err := try(e.partyA); err != nil {
		t.Fatalf("active company with a verified party: %v", err)
	}
	if n := e.sold(t, off); n != 1 {
		t.Fatalf("units sold = %d, want only the accepted order's", n)
	}
}

// TestCreateOrder_NoCommercePolicy_409 (T-10, koro-docs#2): production has
// no default policy until the team decides the commission rate; without a
// policy in effect nothing is ordered (409 catalog.no_commerce_policy) and
// nothing is reserved.
func TestCreateOrder_NoCommercePolicy_409(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	// The env's default policy starts a day before t0; an offering checked
	// out before it has no policy in effect.
	e.clk.Set(t0.Add(-48 * time.Hour))
	off := e.offering(t, func(o *catalog.Offering) {
		o.SubmittedAt, o.ApprovedAt = ptr(t0.Add(-72*time.Hour)), ptr(t0.Add(-72*time.Hour))
	})
	a := actor(t, "company_admin")
	_, err := e.svc.CreateOrder(ctxOf(t, a), a, org("company_a"), orders.CreateInput{BuyerPartyID: e.partyA, Lines: []orders.LineInput{line(off, 1)}})
	wantErr(t, err, catalog.ErrNoPolicy)
	wantProblem(t, err, 409, "catalog.no_commerce_policy")
	if n := e.sold(t, off); n != 0 {
		t.Fatalf("units reserved without a policy = %d", n)
	}
}

// TestCreateOrder_CheckoutRefusals: the catalog's refusals reach the buyer
// as catalog defines them (06 H-52) and nothing stays reserved: an
// offering not on sale, sold out, the per-buyer limit across orders
// (BuyerHeld, BE-038's late binding), an inactive payee; one line per
// offering.
func TestCreateOrder_CheckoutRefusals(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := actor(t, "company_admin")
	try := func(lines ...orders.LineInput) error {
		_, err := e.svc.CreateOrder(ctxOf(t, a), a, org("company_a"), orders.CreateInput{BuyerPartyID: e.partyA, Lines: lines})
		return err
	}
	draft := e.offering(t, func(o *catalog.Offering) {
		o.Status, o.ApprovedBy, o.ApprovedAt, o.SubmittedAt = catalog.OfferingDraft, nil, nil, nil
	})
	wantProblem(t, try(line(draft, 1)), 409, "catalog.not_on_sale")
	small := e.offering(t, func(o *catalog.Offering) { o.QuantityTotal = ptr(1) })
	wantProblem(t, try(line(small, 2)), 409, "catalog.sold_out")
	limited := e.offering(t, func(o *catalog.Offering) { o.PerBuyerLimit = ptr(1) })
	if err := try(line(limited, 1)); err != nil {
		t.Fatal(err)
	}
	wantProblem(t, try(line(limited, 1)), 409, "catalog.per_buyer_limit") // the first order holds the buyer's unit
	other := e.offering(t, nil)
	wantProblem(t, try(line(other, 1), line(other, 1)), 422, "validation_failed")
	e.payees.set(e.payeeA, parties.PayeeSuspended)
	wantProblem(t, try(line(other, 1)), 409, "catalog.payee_not_active")
	if n := e.sold(t, other); n != 0 {
		t.Fatalf("refused orders reserved %d units", n)
	}
	wantProblem(t, try(line(uuid.NewV7(), 1)), 422, "validation_failed")
}

// TestCreateOrder_MixedModels_Split (T-12): lines that fall under different
// models become one order each, with their own reference codes; the model
// C order collects only Koro's fee (the buyer pays the payee the rest).
func TestCreateOrder_MixedModels_Split(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a1, a2 := e.offering(t, nil), e.offering(t, nil)
	c := e.offering(t, nil)
	e.addPolicy(t, catalog.ScopeOffering, &c, func(p *catalog.CommercePolicy) {
		p.ModelCode, p.CollectionMethod, p.CommissionPayer = catalog.ModelC, catalog.CollectDirectToPayee, catalog.PayerBuyer
	})
	vs := e.place(t, line(a1, 1), line(c, 1), line(a2, 2))
	if len(vs) != 2 {
		t.Fatalf("orders = %d, want 2 (A and C)", len(vs))
	}
	a, cc := vs[0].Order, vs[1].Order
	if a.ModelCode != orders.ModelA || len(vs[0].Lines) != 2 || cc.ModelCode != orders.ModelC || len(vs[1].Lines) != 1 {
		t.Fatalf("split = %s×%d, %s×%d", a.ModelCode, len(vs[0].Lines), cc.ModelCode, len(vs[1].Lines))
	}
	if a.OrderNo == cc.OrderNo || !orderNoRe.MatchString(a.OrderNo) || !orderNoRe.MatchString(cc.OrderNo) {
		t.Fatalf("reference codes %s, %s", a.OrderNo, cc.OrderNo)
	}
	if a.SubtotalAmount != 3_000_000 || a.PayableAmount != 3_600_000 || a.KoroCollectAmount != 3_600_000 {
		t.Fatalf("model A order = %+v", a)
	}
	// Model C, 10 % charged to the buyer: fee 100,000 + 20,000 VAT at Koro, 1,200,000 to the payee.
	if cc.CollectionMethod != orders.CollectDirectToPayee || cc.BuyerFeeAmount != 120_000 || cc.PayableAmount != 1_320_000 ||
		cc.KoroCollectAmount != 120_000 || cc.DirectToPayeeAmount != 1_200_000 {
		t.Fatalf("model C order = %+v", cc)
	}
	if n := len(e.events(domainevent.TopicOrderPlaced)); n != 2 {
		t.Fatalf("order.placed events = %d, want 2", n)
	}
}

// TestCreateOrder_PolicyFrozenOnLine (02 §18.3, ADR-0014): the policy in
// effect at placement is copied onto the line (its id, rate, basis, payer,
// commission VAT, dispute window and payout hold); a policy written later
// prices new orders only.
func TestCreateOrder_PolicyFrozenOnLine(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	off := e.offering(t, func(o *catalog.Offering) { o.QuantityTotal = nil })
	first := e.place(t, line(off, 1))[0].Lines[0]
	e.clk.Advance(time.Hour)
	later := e.addPolicy(t, catalog.ScopeOffering, &off, func(p *catalog.CommercePolicy) {
		p.CommissionRateBP, p.CommissionBasis, p.DisputeWindowDays, p.PayoutHoldDays = 1500, catalog.BasisGrossInclVAT, 14, 3
		p.ValidFrom = e.clk.Now()
	})
	second := e.place(t, line(off, 1))[0].Lines[0]
	stored := e.line(t, first.ID)
	if stored.CommercePolicyID != e.policy.ID || stored.CommissionRateBP != 1000 || stored.CommissionBasis != "net_excl_vat" ||
		stored.CommissionAmount != 100_000 || stored.DisputeWindowDays != 7 || stored.PayoutHoldDays != 0 {
		t.Fatalf("the first line moved with the new policy: %+v", stored)
	}
	// 15 % of the gross 1,200,000 = 180,000 + 36,000 VAT.
	if second.CommercePolicyID != later.ID || second.CommissionRateBP != 1500 || second.CommissionAmount != 180_000 ||
		second.CommissionVATAmount != 36_000 || second.SellerNetAmount != 984_000 || second.DisputeWindowDays != 14 || second.PayoutHoldDays != 3 {
		t.Fatalf("the second line = %+v", second)
	}
}

// TestReferenceCode_FormatAndNormalizedLookup (02 §19.1, 04 §C.3): every
// order's reference is KR + 8 Crockford characters; FindByReference finds it
// however the buyer typed it (case, spaces, hyphens, O/I/L), and a
// malformed or unknown code is "not found", never an error.
// NormalizeReference and ExtractReference are payments' tools (BE-041).
func TestReferenceCode_FormatAndNormalizedLookup(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	o := e.placeOne(t).Order
	if !orderNoRe.MatchString(o.OrderNo) {
		t.Fatalf("order_no %q", o.OrderNo)
	}
	typed := []string{o.OrderNo, "kr" + o.OrderNo[2:], o.OrderNo[:4] + " " + o.OrderNo[4:7] + "-" + o.OrderNo[7:]}
	for _, ref := range typed {
		p, found, err := e.mod.Orders.FindByReference(t.Context(), ref)
		if err != nil || !found || p.ID != o.ID || p.DueAmount != 1_200_000 {
			t.Fatalf("FindByReference(%q) = %+v, %v, %v", ref, p, found, err)
		}
	}
	for _, ref := range []string{"KR00000000", "not a code", ""} {
		if _, found, err := e.mod.Orders.FindByReference(t.Context(), ref); err != nil || found {
			t.Fatalf("FindByReference(%q) = %v, %v; want not found", ref, found, err)
		}
	}
	for in, want := range map[string]string{
		"KR7Q2M9TXA": "KR7Q2M9TXA", "kr7q2m9txa": "KR7Q2M9TXA", "KR 7Q2M-9TXA": "KR7Q2M9TXA", "kr.7q2m_9t xa": "KR7Q2M9TXA",
		"KROOI1LL00": "KR00111100",
	} {
		if got, ok := orders.NormalizeReference(in); !ok || got != want {
			t.Errorf("NormalizeReference(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "KR", "KR7Q2M9TX", "KR7Q2M9TXAB", "XR7Q2M9TXA", "KR7Q2M9TXU", "KR7Q2M9TX!", "KR7Q2Ş9TXA"} {
		if got, ok := orders.NormalizeReference(bad); ok {
			t.Errorf("NormalizeReference(%q) = %q, want refused", bad, got)
		}
	}
	for in, want := range map[string]string{
		"KR7Q2M9TXA STANT": "KR7Q2M9TXA", "Ödeme: kr7q 2m9t-xa stant": "KR7Q2M9TXA", "FATURA 12 / KR-7Q2M-9TXA": "KR7Q2M9TXA",
		"stant " + o.OrderNo: o.OrderNo,
	} {
		if got, ok := orders.ExtractReference(in); !ok || got != want {
			t.Errorf("ExtractReference(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, none := range []string{"STANT ÖDEMESİ", "MARKR7Q2M9TXA", "KR7Q2M9TXAB stant", "KR7Q"} {
		if got, ok := orders.ExtractReference(none); ok {
			t.Errorf("ExtractReference(%q) = %q, want none", none, got)
		}
	}
}

// TestApplyPayment_Partial_Accumulates (T-14): partial transfers add up; the
// order waits until Koro has everything, then it is paid; more than is due
// is 409 orders.amount_exceeds_due (payments refunds the excess), and a
// paid order takes no more money.
func TestApplyPayment_Partial_Accumulates(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	o := e.placeOne(t).Order
	ord := e.mod.Orders
	eff, err := ord.ApplyPayment(t.Context(), o.ID, uuid.NewV7(), 500_000)
	if err != nil || eff.Paid || eff.PaidAmount != 500_000 || eff.DueAmount != 700_000 || len(eff.Lines) != 0 {
		t.Fatalf("first part = %+v, %v", eff, err)
	}
	if got := e.order(t, o.ID); got.Status != orders.StatusAwaitingPayment || got.PaidAt != nil {
		t.Fatalf("after a part the order is %s", got.Status)
	}
	_, err = ord.ApplyPayment(t.Context(), o.ID, uuid.NewV7(), 700_001)
	wantProblem(t, err, 409, "orders.amount_exceeds_due")
	_, err = ord.ApplyPayment(t.Context(), o.ID, uuid.NewV7(), 0)
	wantProblem(t, err, 422, "validation_failed")
	eff, err = ord.ApplyPayment(t.Context(), o.ID, uuid.NewV7(), 700_000)
	if err != nil || !eff.Paid || eff.PaidAmount != 1_200_000 || eff.DueAmount != 0 || len(eff.Lines) != 1 {
		t.Fatalf("second part = %+v, %v", eff, err)
	}
	pl := eff.Lines[0]
	if pl.PayeeID == nil || *pl.PayeeID != e.payeeA || pl.SellerNetAmount != 1_080_000 || pl.CommissionAmount != 100_000 ||
		pl.CommissionVATAmount != 20_000 || pl.PayableAmount != 1_200_000 {
		t.Fatalf("paid line = %+v", pl)
	}
	if got := e.order(t, o.ID); got.Status != orders.StatusPaid || got.PaidAt == nil || got.PaidAmount != got.KoroCollectAmount {
		t.Fatalf("paid order = %+v", got)
	}
	_, err = ord.ApplyPayment(t.Context(), o.ID, uuid.NewV7(), 1)
	wantProblem(t, err, 409, "orders.invalid_state")
}

// TestApplyPayment_Full_PaidActivatesEntitlements (01 §6.3 "Havale
// eşleştirme"): a platform-native line grants its right pending at
// placement; the full payment activates it in the same transaction and
// fulfils the line (auto_platform, order_line.fulfilled); a seller's line
// waits for delivery, so the order is paid, not completed; an order of
// platform lines only is completed at payment.
func TestApplyPayment_Full_PaidActivatesEntitlements(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	quota, booth := e.platformOffering(t), e.offering(t, nil)
	v := e.place(t, line(quota, 2), line(booth, 1))[0]
	var ql, bl orders.Line
	for _, l := range v.Lines {
		if l.OfferingID == quota {
			ql = l
		} else {
			bl = l
		}
	}
	if ql.FulfillmentStatus != orders.FulfillNotApplicable || ql.PayeeID != nil || bl.FulfillmentStatus != orders.FulfillPending {
		t.Fatalf("lines = %+v / %+v", ql, bl)
	}
	rs := e.rights(t, ql.ID)
	if len(rs) != 1 || rs[0].Status != entitlements.StatusPending || rs[0].Feature != entitlements.FeatureTargetedPost ||
		rs[0].QuantityTotal == nil || *rs[0].QuantityTotal != 10 || rs[0].OrgID != org("company_a") {
		t.Fatalf("pending right = %+v", rs)
	}
	if want := v.Order.ExpiresAt.AddDate(0, 0, 90); !rs[0].ValidTo.Equal(want) {
		t.Errorf("right valid to %s, want %s (90 days after the payment window)", rs[0].ValidTo, want)
	}
	if len(e.rights(t, bl.ID)) != 0 {
		t.Fatal("a seller's line granted a right")
	}
	eff := e.pay(t, v.Order.ID)
	if !eff.Paid || len(eff.Lines) != 2 {
		t.Fatalf("effect = %+v", eff)
	}
	if rs := e.rights(t, ql.ID); rs[0].Status != entitlements.StatusActive || rs[0].ActivatedAt == nil {
		t.Fatalf("right after payment = %+v", rs[0])
	}
	got := e.line(t, ql.ID)
	if got.FulfillmentStatus != orders.FulfillFulfilled || got.FulfilledVia == nil || *got.FulfilledVia != orders.ViaAutoPlatform {
		t.Fatalf("platform line after payment = %+v", got)
	}
	if o := e.order(t, v.Order.ID); o.Status != orders.StatusInFulfilment {
		t.Fatalf("order with a seller line to deliver = %s, want in_fulfilment", o.Status)
	}
	if n := len(e.events(domainevent.TopicOrderLineFulfilled)); n != 1 {
		t.Fatalf("order_line.fulfilled events = %d, want 1", n)
	}
	only := e.place(t, line(quota, 1))[0]
	e.pay(t, only.Order.ID)
	if o := e.order(t, only.Order.ID); o.Status != orders.StatusCompleted || o.CompletedAt == nil {
		t.Fatalf("platform-only order after payment = %+v", o)
	}
}

// TestModelC_PaidOnDirectConfirmation (00 T-18, T-227, 06 H-56): a model C
// order (the buyer pays the payee directly, Koro its fee) is paid as in
// model A only once all of it is paid: Koro's fee alone leaves it awaiting
// payment; confirmation then pays the order with every line in the effect.
// A model A order or a paid one is 409 orders.invalid_state. Koro
// holds no seller money of it: its delivered line is never paid out (nor
// keeps the seller's org open) and a refund through Koro covers its fee.
func TestModelC_PaidOnDirectConfirmation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.offering(t, nil)
	e.addPolicy(t, catalog.ScopeOffering, &c, func(p *catalog.CommercePolicy) {
		p.ModelCode, p.CollectionMethod, p.CommissionPayer = catalog.ModelC, catalog.CollectDirectToPayee, catalog.PayerBuyer
	})
	v := e.place(t, line(c, 1))[0]
	o := v.Order
	if o.KoroCollectAmount != 120_000 || o.DirectToPayeeAmount != 1_200_000 {
		t.Fatalf("model C order = %+v", o)
	}
	eff, err := e.mod.Orders.ApplyPayment(t.Context(), o.ID, uuid.NewV7(), 120_000)
	if err != nil || eff.Paid || eff.DueAmount != 0 || len(eff.Lines) != 0 {
		t.Fatalf("fee = %+v, %v", eff, err)
	}
	if got := e.order(t, o.ID); got.Status != orders.StatusAwaitingPayment || got.PaidAmount != 120_000 || got.PaidAt != nil {
		t.Fatalf("order after its fee = %+v", got)
	}
	club := actor(t, "club_admin")
	_, err = e.svc.MarkLineFulfilled(ctxOf(t, club), club, v.Lines[0].ID, orders.MarkInput{})
	wantProblem(t, err, 409, "orders.order_not_paid")
	if eff, err = e.mod.Orders.ConfirmDirectPayment(t.Context(), o.ID, uuid.NewV7()); err != nil || !eff.Paid || len(eff.Lines) != 1 ||
		eff.PaidAmount != 120_000 {
		t.Fatalf("direct confirmation = %+v, %v", eff, err)
	}
	if got := e.order(t, o.ID); got.Status != orders.StatusPaid || got.PaidAt == nil {
		t.Fatalf("order after the direct confirmation = %+v", got)
	}
	_, err = e.mod.Orders.ConfirmDirectPayment(t.Context(), o.ID, uuid.NewV7())
	wantProblem(t, err, 409, "orders.invalid_state")

	// Delivered: no payout of money Koro never held; the seller org is free.
	e.mark(t, v.Lines[0].ID)
	buyer := actor(t, "company_admin")
	if _, err := e.svc.ConfirmLine(ctxOf(t, buyer), buyer, v.Lines[0].ID); err != nil {
		t.Fatal(err)
	}
	if ls, err := e.mod.Orders.PayableLines(t.Context(), e.payeeA, e.clk.Now().Add(time.Hour)); err != nil || len(ls) != 0 {
		t.Fatalf("payable model C lines = %+v, %v", ls, err)
	}
	if open, err := e.mod.OpenCommerce(t.Context(), org("club_a")); err != nil || open {
		t.Fatalf("seller with a delivered model C line open = %v, %v", open, err)
	}
	a := e.placeOne(t).Order
	_, err = e.mod.Orders.ConfirmDirectPayment(t.Context(), a.ID, uuid.NewV7())
	wantProblem(t, err, 409, "orders.invalid_state")

	// Refunded through Koro: the fee only.
	d := e.place(t, line(c, 1))[0]
	if _, err := e.mod.Orders.ApplyPayment(t.Context(), d.Order.ID, uuid.NewV7(), 120_000); err != nil {
		t.Fatal(err)
	}
	if _, err := e.mod.Orders.ConfirmDirectPayment(t.Context(), d.Order.ID, uuid.NewV7()); err != nil {
		t.Fatal(err)
	}
	e.mark(t, d.Lines[0].ID)
	if _, err := e.svc.DisputeLine(ctxOf(t, buyer), buyer, d.Lines[0].ID, "Stant kurulmadı"); err != nil {
		t.Fatal(err)
	}
	fin := actor(t, "finance_admin")
	_, err = e.svc.AdminResolveDispute(ctxOf(t, fin), fin, d.Lines[0].ID, orders.ResolveInput{Outcome: orders.OutcomeRefund, Note: "İade",
		RefundAmount: ptr(int64(1_320_000))})
	wantProblem(t, err, 422, "orders.refund_exceeds_paid")
	if _, err := e.svc.AdminResolveDispute(ctxOf(t, fin), fin, d.Lines[0].ID, orders.ResolveInput{Outcome: orders.OutcomeRefund, Note: "İade"}); err != nil {
		t.Fatal(err)
	}
	due, err := e.mod.Orders.RefundsDue(t.Context(), 10)
	if err != nil || len(due) != 1 || due[0].Amount != 120_000 {
		t.Fatalf("refunds due = %+v, %v", due, err)
	}
	err = e.mod.Orders.ApplyRefund(t.Context(), d.Order.ID, &d.Lines[0].ID, 120_001)
	wantProblem(t, err, 422, "orders.refund_exceeds_paid")
	if err := e.mod.Orders.ApplyRefund(t.Context(), d.Order.ID, &d.Lines[0].ID, 120_000); err != nil {
		t.Fatal(err)
	}
	if due, _ := e.mod.Orders.RefundsDue(t.Context(), 10); len(due) != 0 {
		t.Fatalf("refunds due after the refund = %+v", due)
	}
}

// TestPaymentInstructions_NoCollectionAccount_409 (04 §2.15, §C.3): without
// Koro's active collection account the instructions are 409
// payments.no_active_collection_account; with it, the reference, the
// amount still due and the account; a paid order has none (409); a
// seller and another company never see them.
func TestPaymentInstructions_NoCollectionAccount_409(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	o := e.placeOne(t).Order
	buyer := actor(t, "company_member")
	_, err := e.svc.PaymentInstructions(ctxOf(t, buyer), buyer, o.ID)
	wantErr(t, err, orders.ErrNoCollectionAccount)
	wantProblem(t, err, 409, "payments.no_active_collection_account")
	e.acct = &orders.CollectionAccount{ID: uuid.NewV7(), IBAN: "TR330006100519786457841326", HolderName: "Koro Teknoloji A.Ş.", BankName: "Örnek Bank"}
	if _, err := e.mod.Orders.ApplyPayment(t.Context(), o.ID, uuid.NewV7(), 200_000); err != nil {
		t.Fatal(err)
	}
	in, err := e.svc.PaymentInstructions(ctxOf(t, buyer), buyer, o.ID)
	if err != nil || in.ReferenceCode != o.OrderNo || in.AmountDue != 1_000_000 || in.IBAN != e.acct.IBAN || !in.ExpiresAt.Equal(o.ExpiresAt) {
		t.Fatalf("instructions = %+v, %v", in, err)
	}
	fin := actor(t, "finance_admin")
	if _, err := e.svc.PaymentInstructions(ctxOf(t, fin), fin, o.ID); err != nil {
		t.Fatalf("finance: %v", err)
	}
	club := actor(t, "club_admin")
	_, err = e.svc.PaymentInstructions(ctxOf(t, club), club, o.ID)
	wantErr(t, err, authz.ErrForbidden)
	other := actor(t, "other_company_member")
	_, err = e.svc.PaymentInstructions(ctxOf(t, other), other, o.ID)
	wantErr(t, err, authz.ErrNotFound)
	if _, err := e.mod.Orders.ApplyPayment(t.Context(), o.ID, uuid.NewV7(), 1_000_000); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.PaymentInstructions(ctxOf(t, buyer), buyer, o.ID)
	wantProblem(t, err, 409, "orders.invalid_state")
}

// TestGetOrder_SellerViewK26 (03 k26): a seller's club_admin sees only its
// own lines (with its net share and the commission) and the buyer's title;
// a member of the seller without the role is 403; an outsider 404;
// support reads the whole order.
func TestGetOrder_SellerViewK26(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	mine := e.offering(t, nil)
	theirs := e.offering(t, func(o *catalog.Offering) {
		o.OrgID, o.DefaultPayeeID = org("club_b"), ptr(e.payees.add("club_b", parties.PayeeActive))
	})
	o := e.place(t, line(mine, 1), line(theirs, 1))[0].Order
	club := actor(t, "club_admin")
	v, err := e.svc.GetOrder(ctxOf(t, club), club, o.ID)
	if err != nil || !v.SellerView || len(v.Lines) != 1 || v.Lines[0].OfferingID != mine {
		t.Fatalf("seller view = %+v, %v", v, err)
	}
	for name, want := range map[string]error{"club_member": authz.ErrForbidden, "student": authz.ErrNotFound,
		"university_rep": authz.ErrNotFound, "moderator": authz.ErrForbidden} {
		a := actor(t, name)
		if _, err := e.svc.GetOrder(ctxOf(t, a), a, o.ID); !errors.Is(err, want) {
			t.Errorf("%s: %v, want %v", name, err, want)
		}
	}
	for _, name := range []string{"support", "company_member", "finance_admin"} {
		a := actor(t, name)
		v, err := e.svc.GetOrder(ctxOf(t, a), a, o.ID)
		if err != nil || v.SellerView || len(v.Lines) != 2 {
			t.Errorf("%s: view = %+v, %v", name, v.SellerView, err)
		}
	}
}

// TestCancelOrder_ReleasesUnitsAndRights (04 §2.15): the buyer's admin
// cancels an order awaiting payment: its units go back, its pending rights
// are revoked, its lines are cancelled, order.cancelled (O) is written; a
// cancelled order is 409; a recruiter cannot cancel (403).
func TestCancelOrder_ReleasesUnitsAndRights(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	booth, quota := e.offering(t, nil), e.platformOffering(t)
	v := e.place(t, line(booth, 2), line(quota, 1))[0]
	rec := actor(t, "company_member")
	_, err := e.svc.CancelOrder(ctxOf(t, rec), rec, v.Order.ID, "Vazgeçtik")
	wantErr(t, err, authz.ErrForbidden)
	a := actor(t, "company_admin")
	_, err = e.svc.CancelOrder(ctxOf(t, a), a, v.Order.ID, "  ")
	wantProblem(t, err, 422, "validation_failed")
	got, err := e.svc.CancelOrder(ctxOf(t, a), a, v.Order.ID, "Bütçe değişti")
	if err != nil || got.Order.Status != orders.StatusCancelled || got.Order.CancelledAt == nil || *got.Order.CancelReason != "Bütçe değişti" {
		t.Fatalf("cancelled = %+v, %v", got.Order, err)
	}
	if n := e.sold(t, booth); n != 0 {
		t.Fatalf("units still sold = %d", n)
	}
	for _, l := range got.Lines {
		if l.FulfillmentStatus != orders.FulfillCancelled {
			t.Errorf("line %s = %s, want cancelled", l.ID, l.FulfillmentStatus)
		}
		if l.OfferingID == quota {
			if rs := e.rights(t, l.ID); rs[0].Status != entitlements.StatusRevoked {
				t.Errorf("right = %s, want revoked", rs[0].Status)
			}
		}
	}
	if evs := e.events(domainevent.TopicOrderCancelled); len(evs) != 1 {
		t.Fatalf("order.cancelled events = %d", len(evs))
	}
	_, err = e.svc.CancelOrder(ctxOf(t, a), a, v.Order.ID, "Bütçe değişti")
	wantProblem(t, err, 409, "orders.invalid_state")
}

// TestLists pins the order lists (newest first, cursor pages, filters) and
// the seller's sales.
func TestLists(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	off := e.offering(t, func(o *catalog.Offering) { o.QuantityTotal = nil })
	var ids []uuid.UUID
	for range 3 {
		ids = append(ids, e.place(t, line(off, 1))[0].Order.ID)
		e.clk.Advance(time.Minute)
	}
	e.pay(t, ids[0])
	a := actor(t, "company_member")
	p, err := e.svc.ListOrgOrders(ctxOf(t, a), a, org("company_a"), orders.ListQuery{Limit: 2})
	if err != nil || len(p.Items) != 2 || p.Items[0].ID != ids[2] || p.Next == nil {
		t.Fatalf("first page = %+v, %v", p, err)
	}
	p, err = e.svc.ListOrgOrders(ctxOf(t, a), a, org("company_a"), orders.ListQuery{Limit: 2, After: p.Next})
	if err != nil || len(p.Items) != 1 || p.Items[0].ID != ids[0] || p.Next != nil {
		t.Fatalf("second page = %+v, %v", p, err)
	}
	p, err = e.svc.ListOrgOrders(ctxOf(t, a), a, org("company_a"), orders.ListQuery{Status: orders.StatusPaid})
	if err != nil || len(p.Items) != 1 || p.Items[0].ID != ids[0] {
		t.Fatalf("paid orders = %+v, %v", p, err)
	}
	fin := actor(t, "finance_admin")
	no := e.order(t, ids[1]).OrderNo
	p, err = e.svc.AdminListOrders(ctxOf(t, fin), fin, orders.ListQuery{OrderNo: " " + no[:5] + "-" + no[5:]})
	if err != nil || len(p.Items) != 1 || p.Items[0].ID != ids[1] {
		t.Fatalf("admin list by reference = %+v, %v", p, err)
	}
	_, err = e.svc.AdminListOrders(ctxOf(t, fin), fin, orders.ListQuery{OrderNo: "nope"})
	wantProblem(t, err, 422, "validation_failed")
	club := actor(t, "club_admin")
	_, err = e.svc.AdminListOrders(ctxOf(t, club), club, orders.ListQuery{})
	wantErr(t, err, authz.ErrForbidden)
	s, err := e.svc.ListOrgSales(ctxOf(t, club), club, org("club_a"), orders.SalesQuery{})
	if err != nil || len(s.Items) != 3 || s.Items[0].OrderNo == "" || s.Items[0].BuyerTitle != "Örnek Teknoloji A.Ş." {
		t.Fatalf("sales = %+v, %v", s, err)
	}
	s, err = e.svc.ListOrgSales(ctxOf(t, club), club, org("club_a"), orders.SalesQuery{Fulfillment: orders.FulfillMarked})
	if err != nil || len(s.Items) != 0 {
		t.Fatalf("marked sales = %+v, %v", s, err)
	}
	other := actor(t, "other_club_admin")
	_, err = e.svc.ListOrgSales(ctxOf(t, other), other, org("club_a"), orders.SalesQuery{})
	wantErr(t, err, authz.ErrNotFound)
}
