package catalog_test

import (
	"errors"
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/catalog"
)

// world creates the parent rows a store needs: seller orgs, users, payees
// of an org and events of an org (FKs in PostgreSQL; plain IDs in memory).
type world struct {
	org   func(t *testing.T) uuid.UUID
	user  func(t *testing.T) uuid.UUID
	payee func(t *testing.T, org uuid.UUID) uuid.UUID
	event func(t *testing.T, org uuid.UUID) uuid.UUID
}

func memWorld() world {
	id := func(*testing.T) uuid.UUID { return uuid.NewV7() }
	of := func(*testing.T, uuid.UUID) uuid.UUID { return uuid.NewV7() }
	return world{org: id, user: id, payee: of, event: of}
}

func TestStore_Contract_Mem(t *testing.T) {
	t.Parallel()
	storeContract(t, func(t *testing.T) (catalog.Store, world) {
		return catalog.NewMemStore(func() time.Time { return t0 }), memWorld()
	})
}

// storeContract is the contract both stores pass (05 §13.3).
func storeContract(t *testing.T, newStore func(t *testing.T) (catalog.Store, world)) {
	t.Helper()
	ctx := t.Context()
	service := func(t *testing.T, s catalog.Store, code string, reviewed bool) catalog.ServiceItem {
		t.Helper()
		v := catalog.ServiceItem{ID: uuid.NewV7(), Code: code, NameTR: "Stant " + code, Category: catalog.CategoryBooth,
			FulfillmentType: catalog.FulfillOrgBound, DefaultVATRateBP: 2000, VATClass: catalog.VATStandard, Active: true}
		if reviewed {
			v.AccountingReviewedAt = &t0
		}
		got, err := s.InsertService(ctx, v)
		if err != nil {
			t.Fatalf("InsertService: %v", err)
		}
		return got
	}
	type seller struct{ org, payee, creator uuid.UUID }
	newSeller := func(t *testing.T, w world) seller {
		org := w.org(t)
		return seller{org: org, payee: w.payee(t, org), creator: w.user(t)}
	}
	offering := func(t *testing.T, s catalog.Store, sv catalog.ServiceItem, sl seller, mut func(*catalog.Offering)) catalog.Offering {
		t.Helper()
		o := catalog.Offering{ID: uuid.NewV7(), ServiceID: sv.ID, SellerKind: catalog.SellerOrg, OrgID: sl.org, DefaultPayeeID: &sl.payee,
			Title: "Bahar Şenliği Stant", Deliverables: catalog.Deliverables{{Key: "booth", Label: "Stant"}}, UnitPriceAmount: 1_000_000,
			Currency: "TRY", VATRateBP: 2000, CompensationType: "cash", InvoiceTrigger: catalog.InvoiceOnFulfillment,
			UniversityApprovalStatus: catalog.UniNotRequired, Status: catalog.OfferingDraft, CreatedBy: sl.creator}
		if mut != nil {
			mut(&o)
		}
		got, err := s.InsertOffering(ctx, o)
		if err != nil {
			t.Fatalf("InsertOffering: %v", err)
		}
		return got
	}
	// activate moves a draft to active through pending_approval.
	activate := func(t *testing.T, s catalog.Store, o catalog.Offering, approver uuid.UUID) catalog.Offering {
		t.Helper()
		now := t0
		next := o
		next.Status, next.SubmittedAt = catalog.OfferingPendingApproval, &now
		got, err := s.UpdateOffering(ctx, next, catalog.OfferingDraft)
		if err != nil {
			t.Fatalf("submit: %v", err)
		}
		next = got
		next.Status, next.ApprovedBy, next.ApprovedAt = catalog.OfferingActive, &approver, &now
		if got, err = s.UpdateOffering(ctx, next, catalog.OfferingPendingApproval); err != nil {
			t.Fatalf("approve: %v", err)
		}
		return got
	}

	t.Run("services", func(t *testing.T) {
		t.Parallel()
		s, _ := newStore(t)
		a := service(t, s, "booth_a_"+short(), true)
		b := service(t, s, "booth_b_"+short(), false)
		if _, err := s.InsertService(ctx, catalog.ServiceItem{ID: uuid.NewV7(), Code: a.Code, NameTR: "Kopya",
			Category: catalog.CategoryOther, FulfillmentType: catalog.FulfillOrgBound, DefaultVATRateBP: 2000,
			VATClass: catalog.VATStandard, Active: true}); !errors.Is(err, catalog.ErrServiceCodeTaken) {
			t.Fatalf("duplicate code: %v", err)
		}
		bad := a
		bad.ID, bad.Code, bad.VATClass = uuid.NewV7(), "withheld_"+short(), catalog.VATWithheld // no fraction
		if _, err := s.InsertService(ctx, bad); err == nil {
			t.Fatal("a withheld service without a fraction was stored")
		}
		b.Active = false
		got, err := s.UpdateService(ctx, b)
		if err != nil || got.Active || got.Code != b.Code {
			t.Fatalf("UpdateService: %+v %v", got, err)
		}
		all, err := s.ListServices(ctx, catalog.ServiceFilter{})
		if err != nil || !hasService(all, a.ID) || !hasService(all, b.ID) {
			t.Fatalf("ListServices = %d %v", len(all), err)
		}
		sellable, err := s.ListServices(ctx, catalog.ServiceFilter{SellableOnly: true, Category: catalog.CategoryBooth})
		if err != nil || !hasService(sellable, a.ID) || hasService(sellable, b.ID) {
			t.Fatalf("sellable services = %+v %v", sellable, err)
		}
		m, err := s.ServicesByID(ctx, []uuid.UUID{a.ID, uuid.NewV7()})
		if err != nil || len(m) != 1 || m[a.ID].Code != a.Code {
			t.Fatalf("ServicesByID = %v %v", m, err)
		}
		if _, err := s.GetService(ctx, uuid.NewV7(), false); !errors.Is(err, catalog.ErrNotFound) {
			t.Fatalf("unknown service: %v", err)
		}
	})

	t.Run("offering lifecycle and four eyes", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		sv := service(t, s, "booth_"+short(), true)
		sl := newSeller(t, w)
		o := offering(t, s, sv, sl, nil)
		if o.Status != catalog.OfferingDraft || o.QuantitySold != 0 || o.Currency != "TRY" || len(o.Deliverables) != 1 ||
			o.CreatedAt.IsZero() || o.BlockedSponsorFlags == nil {
			t.Fatalf("inserted offering = %+v", o)
		}
		now := t0
		next := o
		next.Status, next.SubmittedAt = catalog.OfferingPendingApproval, &now
		pending, err := s.UpdateOffering(ctx, next, catalog.OfferingDraft)
		if err != nil {
			t.Fatalf("submit: %v", err)
		}
		if _, err := s.UpdateOffering(ctx, next, catalog.OfferingDraft); !errors.Is(err, catalog.ErrInvalidState) {
			t.Fatalf("stale status: %v", err)
		}
		forged := pending
		forged.Status, forged.ApprovedBy, forged.ApprovedAt = catalog.OfferingActive, &sl.creator, &now
		if _, err := s.UpdateOffering(ctx, forged, catalog.OfferingPendingApproval); !errors.Is(err, authz.ErrFourEyes) {
			t.Fatalf("approved by its creator: %v, want authz.four_eyes_violation", err)
		}
		noApproval := pending
		noApproval.Status = catalog.OfferingActive
		if _, err := s.UpdateOffering(ctx, noApproval, catalog.OfferingPendingApproval); err == nil {
			t.Fatal("active without an approval was stored")
		}
		approved := pending
		approver := w.user(t)
		approved.Status, approved.ApprovedBy, approved.ApprovedAt = catalog.OfferingActive, &approver, &now
		active, err := s.UpdateOffering(ctx, approved, catalog.OfferingPendingApproval)
		if err != nil {
			t.Fatalf("approve: %v", err)
		}
		if active.Status != catalog.OfferingActive || !active.Published() || active.UpdatedAt.Before(o.UpdatedAt) {
			t.Fatalf("activated = %+v", active)
		}
		cancel := active
		reason := "Etkinlik ertelendi"
		cancel.Status, cancel.CancelledAt, cancel.CancelReason = catalog.OfferingCancelled, &now, &reason
		got, err := s.UpdateOffering(ctx, cancel, catalog.OfferingActive)
		if err != nil || got.Status != catalog.OfferingCancelled || *got.CancelReason != reason {
			t.Fatalf("cancel: %+v %v", got, err)
		}
		if _, err := s.GetOffering(ctx, uuid.NewV7(), true); !errors.Is(err, catalog.ErrNotFound) {
			t.Fatalf("unknown offering: %v", err)
		}
	})

	t.Run("reserve and release", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		sv := service(t, s, "booth_"+short(), true)
		sl := newSeller(t, w)
		end := t0.Add(24 * time.Hour)
		o := activate(t, s, offering(t, s, sv, sl, func(o *catalog.Offering) { o.QuantityTotal, o.SalesEndAt = ptr(3), &end }), w.user(t))
		if _, ok, err := s.Reserve(ctx, o.ID, 4, t0); ok || err != nil {
			t.Fatalf("more than the stock: %v %v", ok, err)
		}
		got, ok, err := s.Reserve(ctx, o.ID, 2, t0)
		if !ok || err != nil || got.QuantitySold != 2 || got.Status != catalog.OfferingActive {
			t.Fatalf("reserve 2: %+v %v %v", got, ok, err)
		}
		if _, ok, _ := s.Reserve(ctx, o.ID, 1, end); ok {
			t.Fatal("reserved at the sales end")
		}
		got, ok, err = s.Reserve(ctx, o.ID, 1, t0)
		if !ok || err != nil || got.Status != catalog.OfferingSoldOut || got.QuantitySold != 3 {
			t.Fatalf("last unit: %+v %v %v", got.Status, ok, err)
		}
		if _, ok, _ := s.Reserve(ctx, o.ID, 1, t0); ok {
			t.Fatal("reserved a sold-out offering")
		}
		got, ok, err = s.Release(ctx, o.ID, 2)
		if !ok || err != nil || got.Status != catalog.OfferingActive || got.QuantitySold != 1 {
			t.Fatalf("release: %+v %v %v", got.Status, ok, err)
		}
		if _, ok, _ := s.Release(ctx, o.ID, 2); ok {
			t.Fatal("released more than sold")
		}
		if _, _, err := s.Reserve(ctx, uuid.NewV7(), 1, t0); !errors.Is(err, catalog.ErrNotFound) {
			t.Fatalf("reserve unknown: %v", err)
		}
		draft := offering(t, s, sv, sl, nil)
		if _, ok, _ := s.Reserve(ctx, draft.ID, 1, t0); ok {
			t.Fatal("reserved a draft")
		}
		later := t0.Add(time.Hour)
		notYet := activate(t, s, offering(t, s, sv, sl, func(o *catalog.Offering) { o.SalesStartAt = &later }), w.user(t))
		if _, ok, _ := s.Reserve(ctx, notYet.ID, 1, t0); ok {
			t.Fatal("reserved before the sales start")
		}
	})

	t.Run("lists and queues", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		sv := service(t, s, "booth_"+short(), true)
		other := service(t, s, "talk_"+short(), true)
		other.Category = catalog.CategoryTalkSlot
		if _, err := s.UpdateService(ctx, other); err != nil {
			t.Fatal(err)
		}
		a, b := newSeller(t, w), newSeller(t, w)
		approver := w.user(t)
		ev := w.event(t, a.org)
		a1 := activate(t, s, offering(t, s, sv, a, func(o *catalog.Offering) { o.UnitPriceAmount = 300; o.EventID = &ev }), approver)
		a2 := offering(t, s, sv, a, nil)
		b1 := activate(t, s, offering(t, s, other, b, func(o *catalog.Offering) { o.UnitPriceAmount = 100; o.Title = "Kariyer söyleşisi" }), approver)
		page, err := s.ListOrgOfferings(ctx, a.org, catalog.OrgOfferingFilter{}, nil, 10)
		if err != nil || ids(page)[0] != a2.ID || len(page) != 2 {
			t.Fatalf("org list = %v %v", ids(page), err)
		}
		next, err := s.ListOrgOfferings(ctx, a.org, catalog.OrgOfferingFilter{}, &catalog.TimeKey{At: page[0].CreatedAt, ID: page[0].ID}, 10)
		if err != nil || len(next) != 1 || next[0].ID != a1.ID {
			t.Fatalf("org list after the first = %v %v", ids(next), err)
		}
		drafts, _ := s.ListOrgOfferings(ctx, a.org, catalog.OrgOfferingFilter{Status: catalog.OfferingDraft}, nil, 10)
		if len(drafts) != 1 || drafts[0].ID != a2.ID {
			t.Fatalf("org drafts = %v", ids(drafts))
		}
		browse := func(f catalog.BrowseFilter, after *catalog.BrowseKey) []uuid.UUID {
			t.Helper()
			f.Now = t0
			rows, err := s.BrowseOfferings(ctx, f, after, 50)
			if err != nil {
				t.Fatalf("browse: %v", err)
			}
			return ids(rows)
		}
		mine := func(xs []uuid.UUID) []uuid.UUID { // other tests' rows share the PostgreSQL world
			return slices.DeleteFunc(xs, func(id uuid.UUID) bool { return id != a1.ID && id != b1.ID })
		}
		if got := mine(browse(catalog.BrowseFilter{Sort: catalog.SortPrice}, nil)); !slices.Equal(got, []uuid.UUID{b1.ID, a1.ID}) {
			t.Fatalf("by price = %v", got)
		}
		if got := browse(catalog.BrowseFilter{OrgID: &a.org}, nil); !slices.Equal(got, []uuid.UUID{a1.ID}) {
			t.Fatalf("seller a = %v", got)
		}
		if got := browse(catalog.BrowseFilter{OrgIDs: []uuid.UUID{b.org}}, nil); !slices.Equal(got, []uuid.UUID{b1.ID}) {
			t.Fatalf("university sellers = %v", got)
		}
		if got := browse(catalog.BrowseFilter{OrgIDs: []uuid.UUID{}}, nil); len(got) != 0 {
			t.Fatalf("no seller = %v", got)
		}
		if got := browse(catalog.BrowseFilter{EventID: &ev}, nil); !slices.Equal(got, []uuid.UUID{a1.ID}) {
			t.Fatalf("event = %v", got)
		}
		if got := mine(browse(catalog.BrowseFilter{Category: catalog.CategoryBooth, PriceMax: ptr(int64(500))}, nil)); !slices.Equal(got, []uuid.UUID{a1.ID}) {
			t.Fatalf("category and price = %v", got)
		}
		if got := mine(browse(catalog.BrowseFilter{Q: "kariyer"}, nil)); !slices.Equal(got, []uuid.UUID{b1.ID}) {
			t.Fatalf("text = %v", got)
		}
		if got := mine(browse(catalog.BrowseFilter{Sort: catalog.SortPrice}, &catalog.BrowseKey{Price: b1.UnitPriceAmount, ID: b1.ID})); !slices.Equal(got, []uuid.UUID{a1.ID}) {
			t.Fatalf("by price after the first = %v", got)
		}
		if got := mine(browse(catalog.BrowseFilter{}, nil)); !slices.Equal(got, []uuid.UUID{b1.ID, a1.ID}) {
			t.Fatalf("newest first = %v", got)
		}
		// Queues: a submitted offering waits for the platform; one that needs a
		// university waits for it too.
		now := t0
		sub := func(o catalog.Offering, uni bool) catalog.Offering {
			t.Helper()
			next := o
			next.Status, next.SubmittedAt = catalog.OfferingPendingApproval, &now
			if uni {
				next.RequiresUniversityApproval, next.UniversityApprovalStatus = true, catalog.UniPending
			}
			got, err := s.UpdateOffering(ctx, next, catalog.OfferingDraft)
			if err != nil {
				t.Fatalf("submit: %v", err)
			}
			return got
		}
		p1 := sub(a2, true)
		p2 := sub(offering(t, s, sv, b, nil), false)
		queue, err := s.ReviewQueue(ctx, nil, 100)
		if err != nil || !slices.Contains(ids(queue), p1.ID) || !slices.Contains(ids(queue), p2.ID) {
			t.Fatalf("review queue = %v %v", ids(queue), err)
		}
		uq, err := s.UniversityQueue(ctx, nil, 100)
		if err != nil || !slices.Contains(ids(uq), p1.ID) || slices.Contains(ids(uq), p2.ID) {
			t.Fatalf("university queue = %v %v", ids(uq), err)
		}
		approved := p1
		approved.ApprovedBy, approved.ApprovedAt = &approver, &now
		if _, err := s.UpdateOffering(ctx, approved, catalog.OfferingPendingApproval); err != nil {
			t.Fatal(err)
		}
		if queue, _ := s.ReviewQueue(ctx, nil, 100); slices.Contains(ids(queue), p1.ID) {
			t.Fatal("an approved offering is still in the platform queue")
		}
		live, err := s.LiveEventOfferings(ctx, a.org, ev)
		if err != nil || !slices.Equal(ids(live), []uuid.UUID{a1.ID}) {
			t.Fatalf("event offerings = %v %v", ids(live), err)
		}
		if live, _ := s.LiveEventOfferings(ctx, b.org, ev); len(live) != 0 {
			t.Fatalf("another org's event offerings = %v", ids(live))
		}
	})

	t.Run("policies", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		sv := service(t, s, "booth_"+short(), true)
		sl := newSeller(t, w)
		o := offering(t, s, sv, sl, nil)
		by := w.user(t)
		pol := func(scope catalog.PolicyScope, id *uuid.UUID, rate int, from time.Time, to *time.Time) (catalog.CommercePolicy, error) {
			return s.InsertPolicy(ctx, catalog.CommercePolicy{ID: uuid.NewV7(), Scope: scope, ScopeID: id, ModelCode: catalog.ModelA,
				CollectionMethod: catalog.CollectBankTransfer, CommissionRateBP: rate, CommissionBasis: catalog.BasisNetExclVAT,
				CommissionPayer: catalog.PayerSeller, CommissionVATRateBP: 2000, InvoiceAutomation: catalog.InvoiceManual,
				DisputeWindowDays: 7, ValidFrom: from, ValidTo: to, CreatedBy: by})
		}
		jan, jun := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
		org := sl.org
		first, err := pol(catalog.ScopeSellerOrg, &org, 800, jan, &jun)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pol(catalog.ScopeSellerOrg, &org, 900, jan.Add(24*time.Hour), nil); !errors.Is(err, catalog.ErrPolicyOverlap) {
			t.Fatalf("overlap: %v", err)
		}
		second, err := pol(catalog.ScopeSellerOrg, &org, 900, jun, nil)
		if err != nil {
			t.Fatalf("adjacent: %v", err)
		}
		svc, err := pol(catalog.ScopeService, &sv.ID, 600, jan, nil)
		if err != nil {
			t.Fatal(err)
		}
		at := func(tm time.Time) uuid.UUID {
			t.Helper()
			p, ok, err := s.PolicyAt(ctx, o.ID, uuid.NewV7(), org, tm)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				return uuid.Nil()
			}
			return p.ID
		}
		if at(jan) != first.ID || at(jun) != second.ID || at(jan.Add(-time.Second)) != uuid.Nil() {
			t.Fatalf("seller org versions: %s %s %s", at(jan), at(jun), at(jan.Add(-time.Second)))
		}
		if p, ok, _ := s.PolicyAt(ctx, o.ID, sv.ID, org, jun); !ok || p.ID != svc.ID {
			t.Fatalf("service over seller org: %v", p.ID)
		}
		own, err := pol(catalog.ScopeOffering, &o.ID, 300, jan, nil)
		if err != nil {
			t.Fatal(err)
		}
		if p, _, _ := s.PolicyAt(ctx, o.ID, sv.ID, org, jun); p.ID != own.ID {
			t.Fatalf("offering over service: %v", p.ID)
		}
		closed, err := s.ClosePolicy(ctx, second.ID, jun.Add(30*24*time.Hour))
		if err != nil || closed.ValidTo == nil || closed.CommissionRateBP != 900 {
			t.Fatalf("close: %+v %v", closed, err)
		}
		if _, err := s.ClosePolicy(ctx, second.ID, jun.Add(60*24*time.Hour)); !errors.Is(err, catalog.ErrInvalidState) {
			t.Fatalf("close again: %v", err)
		}
		if _, err := s.ClosePolicy(ctx, uuid.NewV7(), jun); !errors.Is(err, catalog.ErrNotFound) {
			t.Fatalf("close unknown: %v", err)
		}
		list, err := s.ListPolicies(ctx, catalog.PolicyFilter{Scope: catalog.ScopeSellerOrg})
		if err != nil || !slices.Contains(policyIDs(list), first.ID) || slices.Contains(policyIDs(list), svc.ID) {
			t.Fatalf("seller org policies = %v %v", policyIDs(list), err)
		}
		feb := jan.Add(40 * 24 * time.Hour)
		list, _ = s.ListPolicies(ctx, catalog.PolicyFilter{Scope: catalog.ScopeSellerOrg, At: &feb})
		if !slices.Contains(policyIDs(list), first.ID) || slices.Contains(policyIDs(list), second.ID) {
			t.Fatalf("seller org policies in February = %v", policyIDs(list))
		}
	})
}

func short() string { return uuid.NewV7().String()[24:] }

func ids(os []catalog.Offering) []uuid.UUID {
	out := make([]uuid.UUID, len(os))
	for i, o := range os {
		out[i] = o.ID
	}
	return out
}

func policyIDs(ps []catalog.CommercePolicy) []uuid.UUID {
	out := make([]uuid.UUID, len(ps))
	for i, p := range ps {
		out[i] = p.ID
	}
	return out
}

func hasService(vs []catalog.ServiceItem, id uuid.UUID) bool {
	return slices.ContainsFunc(vs, func(v catalog.ServiceItem) bool { return v.ID == id })
}
