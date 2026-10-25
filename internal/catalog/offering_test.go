package catalog_test

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit/audittest"
	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/catalog"
	"koro/internal/events"
	"koro/internal/orgs"
	"koro/internal/parties"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/money"
	"koro/internal/platform/problem/problemtest"
)

// TestLineCommission_Table (06 H-6, 02 §19.2): the commission of a line
// through platform/money — half away from zero, min/max, gross basis, the
// commission's VAT and the buyer fee when the buyer pays it.
func TestLineCommission_Table(t *testing.T) {
	t.Parallel()
	pol := func(rate int, basis catalog.CommissionBasis, payer catalog.CommissionPayer, minA, maxA *int64) catalog.CommercePolicy {
		return catalog.CommercePolicy{CommissionRateBP: rate, CommissionBasis: basis, CommissionPayer: payer, CommissionVATRateBP: 2000,
			CommissionMinAmount: minA, CommissionMaxAmount: maxA}
	}
	for _, c := range []struct {
		name               string
		p                  catalog.CommercePolicy
		net, vat           int64
		amount, cvat, buyF int64
	}{
		// 02 §19.2 example: net 1,000,000, VAT 200,000, 10 % from the seller → 100,000 + 20,000.
		{"seller 10 % net", pol(1000, catalog.BasisNetExclVAT, catalog.PayerSeller, nil, nil), 1_000_000, 200_000, 100_000, 20_000, 0},
		{"buyer 10 % net", pol(1000, catalog.BasisNetExclVAT, catalog.PayerBuyer, nil, nil), 1_000_000, 200_000, 100_000, 20_000, 120_000},
		{"gross basis", pol(1000, catalog.BasisGrossInclVAT, catalog.PayerSeller, nil, nil), 1_000_000, 200_000, 120_000, 24_000, 0},
		{"half rounds away from zero", pol(1000, catalog.BasisNetExclVAT, catalog.PayerSeller, nil, nil), 5, 1, 1, 0, 0},
		{"just below half", pol(1000, catalog.BasisNetExclVAT, catalog.PayerSeller, nil, nil), 4, 1, 0, 0, 0},
		{"minimum", pol(100, catalog.BasisNetExclVAT, catalog.PayerSeller, ptr(int64(50_000)), nil), 1_000_000, 200_000, 50_000, 10_000, 0},
		{"maximum", pol(5000, catalog.BasisNetExclVAT, catalog.PayerBuyer, nil, ptr(int64(30_000))), 1_000_000, 200_000, 30_000, 6_000, 36_000},
		{"zero rate", pol(0, catalog.BasisNetExclVAT, catalog.PayerSeller, nil, nil), 1_000_000, 200_000, 0, 0, 0},
		{"full rate", pol(10000, catalog.BasisNetExclVAT, catalog.PayerSeller, nil, nil), 333, 67, 333, 67, 0},
	} {
		got, err := c.p.LineCommission(c.net, c.vat)
		if err != nil || got.Amount != c.amount || got.VATAmount != c.cvat || got.BuyerFee != c.buyF {
			t.Errorf("%s: %+v %v, want %d/%d/%d", c.name, got, err, c.amount, c.cvat, c.buyF)
		}
	}
	if _, err := pol(1000, catalog.BasisGrossInclVAT, catalog.PayerSeller, nil, nil).LineCommission(1<<62, 1<<62); !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("overflow: %v", err)
	}
}

// TestFlowOf_ModelTable pins the 02 §18.3 model table.
func TestFlowOf_ModelTable(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		m       catalog.ModelCode
		payer   catalog.CommissionPayer
		collect catalog.Party
		issuer  catalog.Party
		comm    [2]catalog.Party
		payee   bool
		payout  bool
	}{
		{catalog.ModelA, catalog.PayerSeller, catalog.PartyKoro, catalog.PartyPayee, [2]catalog.Party{catalog.PartyKoro, catalog.PartyPayee}, false, true},
		{catalog.ModelA, catalog.PayerBuyer, catalog.PartyKoro, catalog.PartyPayee, [2]catalog.Party{catalog.PartyKoro, catalog.PartyBuyer}, false, true},
		{catalog.ModelA2, catalog.PayerSeller, catalog.PartyKoro, catalog.PartyKoroOnBehal, [2]catalog.Party{catalog.PartyKoro, catalog.PartyPayee}, false, true},
		{catalog.ModelB, catalog.PayerSeller, catalog.PartyKoro, catalog.PartyKoro, [2]catalog.Party{}, true, true},
		{catalog.ModelC, catalog.PayerBuyer, catalog.PartyPayee, catalog.PartyPayee, [2]catalog.Party{catalog.PartyKoro, catalog.PartyBuyer}, false, false},
	} {
		f, err := catalog.FlowOf(c.m, c.payer)
		if err != nil || f.CollectedBy != c.collect || f.ServiceInvoiceIssuer != c.issuer || f.CommissionInvoice.From != c.comm[0] ||
			f.CommissionInvoice.To != c.comm[1] || f.PayeeInvoicesKoro != c.payee || f.Payout != c.payout {
			t.Errorf("%s/%s: %+v %v", c.m, c.payer, f, err)
		}
	}
	if _, err := catalog.FlowOf("Z", catalog.PayerSeller); err == nil {
		t.Fatal("unknown model accepted")
	}
}

// TestServices_AccountingReviewIsFinance (03 k23, 04 §2.14): platform admins
// manage services but only finance reviews their VAT profile; a VAT
// change withdraws the review (no unreviewed profile is sold);
// organizations see active reviewed services only.
func TestServices_AccountingReviewIsFinance(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	pad, fin := actor(t, "platform_admin"), actor(t, "finance_admin")
	_, err := e.svc.AdminCreateService(ctxOf(t, pad), pad, catalog.ServiceInput{Code: "logo_banner", NameTR: "Afişte logo",
		Category: catalog.CategoryLogoPlacement, FulfillmentType: catalog.FulfillOrgBound, AccountingReviewed: true})
	problemtest.WantErr(t, err, authz.ErrForbidden)
	sv, err := e.svc.AdminCreateService(ctxOf(t, pad), pad, catalog.ServiceInput{Code: "logo_banner", NameTR: "Afişte logo",
		Category: catalog.CategoryLogoPlacement, FulfillmentType: catalog.FulfillOrgBound, VATClass: catalog.VATWithheld,
		VATWithholdingNum: ptr(5), VATWithholdingDen: ptr(10), VATWithholdingCode: "616"})
	if err != nil || sv.AccountingReviewedAt != nil || sv.Sellable() {
		t.Fatalf("create by platform admin: %+v %v", sv, err)
	}
	_, err = e.svc.AdminCreateService(ctxOf(t, pad), pad, catalog.ServiceInput{Code: "logo_banner", NameTR: "Kopya",
		Category: catalog.CategoryOther, FulfillmentType: catalog.FulfillOrgBound})
	problemtest.WantProblem(t, err, http.StatusConflict, "catalog.service_code_taken")
	_, err = e.svc.AdminUpdateService(ctxOf(t, pad), pad, sv.ID, catalog.ServicePatch{AccountingReviewed: ptr(true)})
	problemtest.WantErr(t, err, authz.ErrForbidden)
	got, err := e.svc.AdminUpdateService(ctxOf(t, fin), fin, sv.ID, catalog.ServicePatch{AccountingReviewed: ptr(true),
		AccountingReviewedRef: ptr("MM-2026-02")})
	if err != nil || got.AccountingReviewedAt == nil || !got.Sellable() {
		t.Fatalf("review by finance: %+v %v", got, err)
	}
	// A VAT profile change by the platform admin withdraws the review.
	got, err = e.svc.AdminUpdateService(ctxOf(t, pad), pad, sv.ID, catalog.ServicePatch{DefaultVATRateBP: ptr(1000)})
	if err != nil || got.AccountingReviewedAt != nil || got.DefaultVATRateBP != 1000 {
		t.Fatalf("VAT change: %+v %v", got, err)
	}
	// A name change keeps it.
	got, _ = e.svc.AdminUpdateService(ctxOf(t, fin), fin, sv.ID, catalog.ServicePatch{AccountingReviewed: ptr(true)})
	got, err = e.svc.AdminUpdateService(ctxOf(t, pad), pad, sv.ID, catalog.ServicePatch{NameTR: ptr("Afişte logo (A3)")})
	if err != nil || got.AccountingReviewedAt == nil {
		t.Fatalf("name change: %+v %v", got, err)
	}
	_, err = e.svc.AdminUpdateService(ctxOf(t, pad), pad, sv.ID, catalog.ServicePatch{})
	problemtest.WantProblem(t, err, http.StatusUnprocessableEntity, "catalog.empty_patch")
	_, err = e.svc.AdminUpdateService(ctxOf(t, pad), pad, sv.ID, catalog.ServicePatch{VATClass: ptr(catalog.VATStandard)})
	problemtest.WantProblem(t, err, http.StatusUnprocessableEntity, "validation_failed") // withheld fraction left on a standard class
	unreviewed := e.service(t, "workshop_x", catalog.CategoryWorkshop, catalog.FulfillOrgBound, false)
	ca := actor(t, "club_admin")
	orgView, err := e.svc.ListServices(ctxOf(t, ca), ca, "")
	if err != nil || slices.ContainsFunc(orgView, func(s catalog.ServiceItem) bool { return s.ID == unreviewed }) ||
		!slices.ContainsFunc(orgView, func(s catalog.ServiceItem) bool { return s.ID == e.booth }) {
		t.Fatalf("organization's services = %d %v", len(orgView), err)
	}
	staffView, _ := e.svc.ListServices(ctxOf(t, fin), fin, "")
	if !slices.ContainsFunc(staffView, func(s catalog.ServiceItem) bool { return s.ID == unreviewed }) {
		t.Fatal("finance does not see the unreviewed service")
	}
	if booths, _ := e.svc.ListServices(ctxOf(t, ca), ca, catalog.CategoryBooth); len(booths) != 1 || booths[0].ID != e.booth {
		t.Fatalf("booth services = %+v", booths)
	}
	if _, err := e.svc.ListServices(ctxOf(t, authz.Anonymous()), authz.Anonymous(), ""); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("anonymous: %v", err)
	}
}

// TestCreateOffering_ParentsAndFields: the parents come from the seller org
// (06 H-38, H-55: another org's event or payee is 422
// catalog.parent_mismatch); a platform-native service is the platform's;
// the fields are checked; another org's surface is 404; a suspended seller
// cannot write.
func TestCreateOffering_ParentsAndFields(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ca := actor(t, "club_admin")
	ctx := ctxOf(t, ca)
	evB := e.events.add("club_b", events.StatusPublished)
	payeeB := e.payees.add("club_b", parties.PayeeActive, true)
	base := func(mut func(*catalog.OfferingInput)) catalog.OfferingInput {
		in := catalog.OfferingInput{ServiceID: e.booth, Title: "Stant paketi", UnitPriceAmount: 500_000}
		if mut != nil {
			mut(&in)
		}
		return in
	}
	_, err := e.svc.CreateOffering(ctx, ca, org("club_a"), base(func(in *catalog.OfferingInput) { in.EventID = &evB }))
	problemtest.WantProblem(t, err, http.StatusUnprocessableEntity, "catalog.parent_mismatch")
	_, err = e.svc.CreateOffering(ctx, ca, org("club_a"), base(func(in *catalog.OfferingInput) { in.DefaultPayeeID = &payeeB }))
	problemtest.WantProblem(t, err, http.StatusUnprocessableEntity, "catalog.parent_mismatch")
	cancelled := e.events.add("club_a", events.StatusCancelled)
	for name, in := range map[string]catalog.OfferingInput{
		"a cancelled event":   base(func(in *catalog.OfferingInput) { in.EventID = &cancelled }),
		"an unknown event":    base(func(in *catalog.OfferingInput) { in.EventID = ptr(uuid.NewV7()) }),
		"an unknown payee":    base(func(in *catalog.OfferingInput) { in.DefaultPayeeID = ptr(uuid.NewV7()) }),
		"an unknown service":  base(func(in *catalog.OfferingInput) { in.ServiceID = uuid.NewV7() }),
		"a platform service":  base(func(in *catalog.OfferingInput) { in.ServiceID = e.quota }),
		"a short title":       base(func(in *catalog.OfferingInput) { in.Title = "ab" }),
		"a zero price":        base(func(in *catalog.OfferingInput) { in.UnitPriceAmount = 0 }),
		"a limit above stock": base(func(in *catalog.OfferingInput) { in.QuantityTotal, in.PerBuyerLimit = ptr(2), ptr(3) }),
		"a past sales end":    base(func(in *catalog.OfferingInput) { in.SalesEndAt = ptr(t0.Add(-time.Hour)) }),
		"an inverted window": base(func(in *catalog.OfferingInput) {
			in.SalesStartAt, in.SalesEndAt = ptr(t0.Add(48*time.Hour)), ptr(t0.Add(24*time.Hour))
		}),
		"duplicate deliverable": base(func(in *catalog.OfferingInput) {
			in.Deliverables = []catalog.Deliverable{{Key: "a1", Label: "x"}, {Key: "a1", Label: "y"}}
		}),
		"an unknown flag": base(func(in *catalog.OfferingInput) { in.BlockedSponsorFlags = []catalog.SponsorFlag{"weapons"} }),
		"raw HTML description": base(func(in *catalog.OfferingInput) {
			in.DescriptionJSON = map[string]any{"type": "doc", "html": "<b>x</b>"}
		}),
	} {
		_, err := e.svc.CreateOffering(ctx, ca, org("club_a"), in)
		if s, _ := problemtest.StatusCode(err); s != http.StatusUnprocessableEntity {
			t.Errorf("%s: %v, want 422", name, err)
		}
	}
	// IDOR: another club's surface is 404, a member without the role 403.
	_, err = e.svc.CreateOffering(ctxOf(t, actor(t, "other_club_admin")), actor(t, "other_club_admin"), org("club_a"), base(nil))
	problemtest.WantErr(t, err, authz.ErrNotFound)
	_, err = e.svc.CreateOffering(ctxOf(t, actor(t, "club_member")), actor(t, "club_member"), org("club_a"), base(nil))
	problemtest.WantErr(t, err, authz.ErrForbidden)
	_, err = e.svc.CreateOffering(ctx, ca, uuid.NewV7(), base(nil))
	problemtest.WantErr(t, err, catalog.ErrNotFound)
	// A description is sanitised to the allow-list.
	v, err := e.svc.CreateOffering(ctx, ca, org("club_a"), base(func(in *catalog.OfferingInput) {
		in.DescriptionJSON = map[string]any{"type": "doc", "content": []any{map[string]any{"type": "paragraph",
			"content": []any{map[string]any{"type": "text", "text": "Stant 3x3"}}}}}
		in.BlockedSponsorFlags = []catalog.SponsorFlag{catalog.SponsorTobacco, catalog.SponsorAlcohol}
	}))
	if err != nil || v.Offering.DescriptionJSON == nil || !slices.Equal([]string(v.Offering.BlockedSponsorFlags), []string{"alcohol", "tobacco"}) ||
		v.Offering.VATRateBP != 2000 || v.Offering.DefaultPayeeID == nil || *v.Offering.DefaultPayeeID != e.payeeA ||
		v.Offering.InvoiceTrigger != catalog.InvoiceOnFulfillment || v.Offering.CreatedBy != ca.UserID {
		t.Fatalf("created = %+v %v", v.Offering, err)
	}
	if evs := e.audit.Of(authz.CatalogOfferingCreate); len(evs) != 1 || *evs[0].OrgID != org("club_a") {
		t.Fatalf("create audit = %+v", evs)
	}
	// A suspended seller writes nothing (03 §2.3).
	sus := e.orgs.m[org("club_a")]
	sus.Status = orgs.StatusSuspended
	e.orgs.m[org("club_a")] = sus
	_, err = e.svc.CreateOffering(ctx, ca, org("club_a"), base(nil))
	problemtest.WantErr(t, err, orgs.ErrOrgSuspended)
}

// TestUpdateOffering_DraftOnlyWithIfMatch (04 §2.14, §1.13): drafts only,
// If-Match required; a new service re-copies its VAT rate; clear empties
// optional fields; another club is 404.
func TestUpdateOffering_DraftOnlyWithIfMatch(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.defaultPolicy(t)
	ev := e.events.add("club_a", events.StatusDraft)
	v := e.offering(t, "club_admin", "club_a", func(in *catalog.OfferingInput) { in.EventID = &ev; in.QuantityTotal = ptr(5) })
	ca := actor(t, "club_admin")
	ctx := ctxOf(t, ca)
	_, err := e.svc.UpdateOffering(ctx, ca, v.Offering.ID, "", catalog.OfferingPatch{Title: ptr("Yeni başlık")})
	problemtest.WantErr(t, err, catalog.ErrPreconditionRequired)
	_, err = e.svc.UpdateOffering(ctx, ca, v.Offering.ID, `"stale"`, catalog.OfferingPatch{Title: ptr("Yeni başlık")})
	problemtest.WantErr(t, err, catalog.ErrPreconditionFailed)
	_, err = e.svc.UpdateOffering(ctx, ca, v.Offering.ID, v.ETag, catalog.OfferingPatch{})
	problemtest.WantProblem(t, err, http.StatusUnprocessableEntity, "catalog.empty_patch")
	_, err = e.svc.UpdateOffering(ctx, ca, v.Offering.ID, v.ETag, catalog.OfferingPatch{Clear: []string{"title"}})
	problemtest.WantProblem(t, err, http.StatusUnprocessableEntity, "validation_failed")
	other := actor(t, "other_club_admin")
	_, err = e.svc.UpdateOffering(ctxOf(t, other), other, v.Offering.ID, v.ETag, catalog.OfferingPatch{Title: ptr("x y z")})
	problemtest.WantErr(t, err, authz.ErrNotFound)

	lowVAT := e.service(t, "workshop_low", catalog.CategoryWorkshop, catalog.FulfillOrgBound, true)
	fin := actor(t, "finance_admin")
	if _, err := e.svc.AdminUpdateService(ctxOf(t, fin), fin, lowVAT, catalog.ServicePatch{DefaultVATRateBP: ptr(1000),
		AccountingReviewed: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	got, err := e.svc.UpdateOffering(ctx, ca, v.Offering.ID, v.ETag, catalog.OfferingPatch{ServiceID: &lowVAT, Title: ptr("Atölye paketi"),
		Clear: []string{"event_id", "quantity_total"}})
	if err != nil || got.Offering.VATRateBP != 1000 || got.Offering.EventID != nil || got.Offering.QuantityTotal != nil ||
		got.Offering.Title != "Atölye paketi" || got.ETag == v.ETag {
		t.Fatalf("update: %+v %v", got.Offering, err)
	}
	if evs := e.audit.Of(authz.CatalogOfferingUpdate); len(evs) != 1 {
		t.Fatalf("update audit = %+v", evs)
	}
	e.active(t, "club_admin", v.Offering.ID)
	cur, _ := e.svc.GetOffering(ctx, ca, v.Offering.ID)
	_, err = e.svc.UpdateOffering(ctx, ca, v.Offering.ID, cur.ETag, catalog.OfferingPatch{Title: ptr("Yeni başlık")})
	problemtest.WantProblem(t, err, http.StatusConflict, "catalog.invalid_state")
}

// TestSubmitOffering_Checks: an event-bound service needs its event (422
// catalog.offering_incomplete); an unreviewed service is not sold (409
// catalog.service_not_sellable); the VAT rate is refreshed from the
// service; offering.submitted (A) notifies every platform admin.
func TestSubmitOffering_Checks(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ca := actor(t, "club_admin")
	ctx := ctxOf(t, ca)
	stand := e.service(t, "stand_event", catalog.CategoryBooth, catalog.FulfillEventBound, true)
	v := e.offering(t, "club_admin", "club_a", func(in *catalog.OfferingInput) { in.ServiceID = stand })
	_, err := e.svc.SubmitOffering(ctx, ca, v.Offering.ID)
	problemtest.WantProblem(t, err, http.StatusUnprocessableEntity, "catalog.offering_incomplete")
	unreviewed := e.service(t, "unreviewed_x", catalog.CategoryWorkshop, catalog.FulfillOrgBound, false)
	w := e.offering(t, "club_admin", "club_a", func(in *catalog.OfferingInput) { in.ServiceID = unreviewed })
	_, err = e.svc.SubmitOffering(ctx, ca, w.Offering.ID)
	problemtest.WantProblem(t, err, http.StatusConflict, "catalog.service_not_sellable")

	x := e.offering(t, "club_admin", "club_a", nil)
	fin := actor(t, "finance_admin")
	if _, err := e.svc.AdminUpdateService(ctxOf(t, fin), fin, e.booth, catalog.ServicePatch{DefaultVATRateBP: ptr(1000),
		AccountingReviewed: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	got, err := e.svc.SubmitOffering(ctx, ca, x.Offering.ID)
	if err != nil || got.Offering.VATRateBP != 1000 || got.Offering.Status != catalog.OfferingPendingApproval {
		t.Fatalf("submit: %+v %v", got.Offering, err)
	}
	// The approval needs a commerce policy (T-10).
	pad := actor(t, "platform_admin")
	_, err = e.svc.AdminDecideOffering(ctxOf(t, pad), pad, x.Offering.ID, catalog.Decision{Approve: true})
	problemtest.WantProblem(t, err, http.StatusConflict, "catalog.no_commerce_policy")
	// A rejection needs a reason and sends it back to draft.
	_, err = e.svc.AdminDecideOffering(ctxOf(t, pad), pad, x.Offering.ID, catalog.Decision{})
	problemtest.WantProblem(t, err, http.StatusUnprocessableEntity, "validation_failed")
	got, err = e.svc.AdminDecideOffering(ctxOf(t, pad), pad, x.Offering.ID, catalog.Decision{Reason: "Fiyat belirsiz"})
	if err != nil || got.Offering.Status != catalog.OfferingDraft || *got.Offering.RejectionReason != "Fiyat belirsiz" {
		t.Fatalf("reject: %+v %v", got.Offering, err)
	}
	// The notices: offering.submitted → every platform admin; offering.decided (rejected) → the seller's admins.
	for _, ev := range e.outbox.Events() {
		for _, s := range subscriptions(t, e) {
			if s.topic == ev.Topic {
				if err := s.h(t.Context(), ev); err != nil {
					t.Fatalf("%s: %v", ev.Topic, err)
				}
			}
		}
	}
	for _, name := range []string{"platform_admin", "platform_admin_2"} {
		if n := countTemplate(e.sender.all(), "catalog.offering_review_pending", authztest.ID(name)); n != 1 {
			t.Errorf("%s review notices = %d", name, n)
		}
	}
	if n := countTemplate(e.sender.all(), "catalog.offering_rejected", authztest.ID("club_admin")); n != 1 {
		t.Errorf("club_admin rejection notices = %d", n)
	}
}

// TestCancelOffering_Codes: the seller cancels with seller_cancelled, the
// platform with platform_cancelled; offering.cancelled is an outbox event
// (O); a cancelled offering is 409.
func TestCancelOffering_Codes(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.defaultPolicy(t)
	a := e.offering(t, "club_admin", "club_a", nil)
	b := e.offering(t, "club_admin", "club_a", nil)
	e.active(t, "club_admin", b.Offering.ID)
	ca, pad := actor(t, "club_admin"), actor(t, "platform_admin")
	_, err := e.svc.CancelOffering(ctxOf(t, ca), ca, a.Offering.ID, " ")
	problemtest.WantProblem(t, err, http.StatusUnprocessableEntity, "validation_failed")
	if _, err := e.svc.CancelOffering(ctxOf(t, ca), ca, a.Offering.ID, "Vazgeçtik"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CancelOffering(ctxOf(t, pad), pad, b.Offering.ID, "Kural dışı içerik"); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.CancelOffering(ctxOf(t, ca), ca, a.Offering.ID, "Vazgeçtik")
	problemtest.WantProblem(t, err, http.StatusConflict, "catalog.invalid_state")
	var reasons []string
	for _, ev := range e.outbox.Events() {
		if ev.Topic == domainevent.TopicOfferingCancelled {
			p, _ := domainevent.Decode[domainevent.OfferingCancelled](ev)
			reasons = append(reasons, p.Reason)
		}
	}
	if !slices.Equal(reasons, []string{catalog.CancelBySeller, catalog.CancelByPlatform}) {
		t.Fatalf("reasons = %v", reasons)
	}
	if evs := e.audit.Of(authz.CatalogOfferingCancel); len(evs) != 2 || evs[0].Reason != "Vazgeçtik" {
		t.Fatalf("cancel audit = %+v", evs)
	}
	// A cancelled offering that was on sale stays readable; one that never was is the seller's.
	co := actor(t, "company_admin")
	if _, err := e.svc.GetOffering(ctxOf(t, co), co, b.Offering.ID); err != nil {
		t.Fatalf("company reads the cancelled published offering: %v", err)
	}
	if _, err := e.svc.GetOffering(ctxOf(t, co), co, a.Offering.ID); !errors.Is(err, authz.ErrNotFound) {
		t.Fatalf("company reads the cancelled draft: %v", err)
	}
}

// TestQueues_Pagination: the platform queue keeps the offerings waiting for
// it, oldest first; the university queue keeps one university's clubs'
// offerings across pages.
func TestQueues_Pagination(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ca, ob := actor(t, "club_admin"), actor(t, "other_club_admin")
	e.payees.add("club_b", parties.PayeeActive, true)
	var uniA []uuid.UUID
	for i := range 5 {
		e.clk.Advance(time.Minute)
		v := e.offering(t, "club_admin", "club_a", func(in *catalog.OfferingInput) { in.RequiresUniversityApproval = true })
		if _, err := e.svc.SubmitOffering(ctxOf(t, ca), ca, v.Offering.ID); err != nil {
			t.Fatal(err)
		}
		uniA = append(uniA, v.Offering.ID)
		w := e.offering(t, "other_club_admin", "club_b", func(in *catalog.OfferingInput) { in.RequiresUniversityApproval = true })
		if _, err := e.svc.SubmitOffering(ctxOf(t, ob), ob, w.Offering.ID); err != nil {
			t.Fatalf("club_b offering %d: %v", i, err)
		}
	}
	rep := actor(t, "university_rep")
	var seen []uuid.UUID
	var after *catalog.TimeKey
	for range 5 {
		p, err := e.svc.ListUniversityApprovals(ctxOf(t, rep), rep, org("uni_a"), catalog.QueueQuery{Limit: 2, After: after})
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range p.Items {
			seen = append(seen, it.Offering.ID)
		}
		if p.NextTime == nil {
			break
		}
		after = p.NextTime
	}
	if !slices.Equal(seen, uniA) {
		t.Fatalf("uni_a queue pages = %v, want %v", seen, uniA)
	}
	pad := actor(t, "platform_admin")
	q, err := e.svc.AdminReviewQueue(ctxOf(t, pad), pad, catalog.QueueQuery{Limit: 100})
	if err != nil || len(q.Items) != 10 || q.Items[0].Offering.ID != uniA[0] {
		t.Fatalf("platform queue = %d %v", len(q.Items), err)
	}
	if _, err := e.svc.ListUniversityApprovals(ctxOf(t, rep), rep, uuid.NewV7(), catalog.QueueQuery{}); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("unknown org: %v", err)
	}
}

// TestBrowseOfferings_FiltersAndPages: text, seller, event, university,
// category, price and the three sorts with their cursors (HTTP).
func TestBrowseOfferings_FiltersAndPages(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.defaultPolicy(t)
	e.payees.add("club_b", parties.PayeeActive, true)
	ev := e.events.add("club_a", events.StatusPublished)
	var ids []string
	for i, price := range []int64{300_000, 100_000, 200_000} {
		e.clk.Advance(time.Minute)
		v := e.offering(t, "club_admin", "club_a", func(in *catalog.OfferingInput) {
			in.UnitPriceAmount, in.Title = price, "Stant paketi "+strconv.Itoa(i)
			if i == 0 {
				in.EventID = &ev
			}
		})
		e.active(t, "club_admin", v.Offering.ID)
		ids = append(ids, v.Offering.ID.String())
	}
	talk := e.service(t, "talk_slot_x", catalog.CategoryTalkSlot, catalog.FulfillOrgBound, true)
	b := e.offering(t, "other_club_admin", "club_b", func(in *catalog.OfferingInput) { in.ServiceID, in.Title = talk, "Kariyer söyleşisi" })
	e.active(t, "other_club_admin", b.Offering.ID)
	app := newHTTP(t, e)
	list := func(q string) []string {
		t.Helper()
		var out []string
		cursor := ""
		for range 10 {
			path := "/api/v1/offerings?limit=2&" + q
			if cursor != "" {
				path += "&cursor=" + cursor
			}
			rec, body := call(t, app, "company_admin", http.MethodGet, path, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
			}
			for _, it := range body["items"].([]any) {
				out = append(out, it.(map[string]any)["offering_id"].(string))
			}
			c, _ := body["next_cursor"].(string)
			if c == "" {
				break
			}
			cursor = c
		}
		return out
	}
	if got := list("sort=unit_price_amount&org_id=" + org("club_a").String()); !slices.Equal(got, []string{ids[1], ids[2], ids[0]}) {
		t.Fatalf("by price = %v", got)
	}
	if got := list("sort=-created_at&university_id=" + authztest.UniversityID("uni_a").String()); !slices.Equal(got, []string{ids[2], ids[1], ids[0]}) {
		t.Fatalf("uni_a newest first = %v", got)
	}
	if got := list("event_id=" + ev.String()); !slices.Equal(got, []string{ids[0]}) {
		t.Fatalf("event = %v", got)
	}
	if got := list("service_category=talk_slot"); !slices.Equal(got, []string{b.Offering.ID.String()}) {
		t.Fatalf("category = %v", got)
	}
	if got := list("price_max_amount=150000&org_id=" + org("club_a").String()); !slices.Equal(got, []string{ids[1]}) {
		t.Fatalf("price max = %v", got)
	}
	if got := list("q=kariyer"); !slices.Equal(got, []string{b.Offering.ID.String()}) {
		t.Fatalf("text = %v", got)
	}
	// A cursor of another filter is refused.
	rec, body := call(t, app, "company_admin", http.MethodGet, "/api/v1/offerings?limit=1&sort=unit_price_amount", nil)
	c, _ := body["next_cursor"].(string)
	if rec.Code != 200 || c == "" {
		t.Fatalf("first page: %d %v", rec.Code, body)
	}
	if rec, _ := call(t, app, "company_admin", http.MethodGet, "/api/v1/offerings?limit=1&sort=sales_end_at&cursor="+c, nil); rec.Code != 422 {
		t.Fatalf("cursor of another sort: %d", rec.Code)
	}
}

// TestHTTP_OfferingFlow drives the HTTP surface: create (201 + ETag),
// update with If-Match, submit, decide, the buyer's view.
func TestHTTP_OfferingFlow(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.defaultPolicy(t)
	app := newHTTP(t, e)
	rec, body := call(t, app, "club_admin", http.MethodPost, "/api/v1/orgs/"+org("club_a").String()+"/offerings", map[string]any{
		"service_id": e.booth.String(), "title": "Bahar Şenliği Stant Paketi", "unit_price_amount": 1000000,
		"deliverables": []map[string]string{{"key": "booth_3x3", "label": "3x3 stant alanı"}}, "quantity_total": 5, "per_buyer_limit": 1,
		"sales_end_at": t0.Add(30 * 24 * time.Hour).Format(time.RFC3339), "blocked_sponsor_flags": []string{"alcohol"},
	})
	if rec.Code != http.StatusCreated || rec.Header().Get("ETag") == "" {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	id := body["offering_id"].(string)
	appr := body["approval"].(map[string]any)
	if appr["default_payee_id"] != e.payeeA.String() || body["status"] != "draft" || body["vat_rate_bp"].(float64) != 2000 {
		t.Fatalf("created body = %v", body)
	}
	etag := rec.Header().Get("ETag")
	req := func(persona, method, path string, b any, hdr map[string]string) (int, map[string]any, string) {
		t.Helper()
		r, out := callH(t, app, persona, method, path, b, hdr)
		return r.Code, out, r.Header().Get("ETag")
	}
	code, _, _ := req("club_admin", http.MethodPatch, "/api/v1/offerings/"+id, map[string]any{"title": "Stant paketi (güncel)"}, nil)
	if code != http.StatusPreconditionRequired {
		t.Fatalf("update without If-Match: %d", code)
	}
	code, body, _ = req("club_admin", http.MethodPatch, "/api/v1/offerings/"+id, map[string]any{"title": "Stant paketi (güncel)"},
		map[string]string{"If-Match": etag})
	if code != http.StatusOK || body["title"] != "Stant paketi (güncel)" {
		t.Fatalf("update: %d %v", code, body)
	}
	if code, _, _ := req("club_admin", http.MethodPost, "/api/v1/offerings/"+id+"/submit", nil, nil); code != http.StatusOK {
		t.Fatalf("submit: %d", code)
	}
	if code, _, _ := req("platform_admin", http.MethodPost, "/api/v1/admin/offerings/"+id+"/decision",
		map[string]any{"decision": "approved"}, nil); code != http.StatusOK {
		t.Fatalf("decide: %d", code)
	}
	code, body, etag = req("company_admin", http.MethodGet, "/api/v1/offerings/"+id, nil, nil)
	if code != http.StatusOK || body["status"] != "active" || body["approval"] != nil || etag != "" || body["quantity_remaining"].(float64) != 5 ||
		body["seller"].(map[string]any)["kind"] != "club" {
		t.Fatalf("buyer view: %d %v", code, body)
	}
	if code, _, _ := req("finance_admin", http.MethodGet, "/api/v1/admin/commerce-policies?scope=default&at="+t0.Format(time.RFC3339), nil, nil); code != 200 {
		t.Fatalf("policy list: %d", code)
	}
}

// TestSeeder_DefaultPolicyIdempotent (bootstrap-commerce): the default
// policy (model A, net, from the seller) is written once; a default in
// effect is left alone.
func TestSeeder_DefaultPolicyIdempotent(t *testing.T) {
	t.Parallel()
	store := catalog.NewMemStore(func() time.Time { return t0 })
	au := &audittest.Recorder{}
	sd, err := catalog.NewSeeder(catalog.SeederDeps{Store: store, Tx: db.NopTx{}, Audit: au, Clock: clock.NewFake(t0), Log: discardLog()})
	if err != nil {
		t.Fatal(err)
	}
	by := authztest.ID("platform_admin")
	p, created, err := sd.EnsureDefaultPolicy(t.Context(), catalog.DevCommissionRateBP, by)
	if err != nil || !created || p.CommissionRateBP != 1000 || p.ModelCode != catalog.ModelA || p.CommissionPayer != catalog.PayerSeller ||
		p.CommissionBasis != catalog.BasisNetExclVAT || p.CollectionMethod != catalog.CollectBankTransfer || p.Scope != catalog.ScopeDefault {
		t.Fatalf("seed = %+v %v %v", p, created, err)
	}
	again, created, err := sd.EnsureDefaultPolicy(t.Context(), 500, by)
	if err != nil || created || again.ID != p.ID {
		t.Fatalf("second run = %+v %v %v", again.ID, created, err)
	}
	if evs := au.Of(authz.CatalogPolicyManage); len(evs) != 1 || evs[0].SystemJob != catalog.SeedJob {
		t.Fatalf("seed audit = %+v", evs)
	}
	if _, err := catalog.NewSeeder(catalog.SeederDeps{}); err == nil {
		t.Fatal("a seeder without dependencies")
	}
}
