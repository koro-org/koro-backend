package catalog_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/catalog"
	"koro/internal/events"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/parties"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
)

// TestSubmitOffering_NoApprovedIBAN_409 (BE-037 note, 04 §2.14): an
// offering whose payee has no approved bank account is not submitted (409
// parties.no_approved_account) and stays a draft; an inactive payee is 409
// catalog.payee_not_active; the same offering goes once the account is
// approved. The approval checks it again: an account revoked meanwhile
// keeps the offering off the marketplace.
func TestSubmitOffering_NoApprovedIBAN_409(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.defaultPolicy(t)
	noAccount := e.payees.add("club_a", parties.PayeeActive, false)
	v := e.offering(t, "club_admin", "club_a", func(in *catalog.OfferingInput) { in.DefaultPayeeID = &noAccount })
	a := actor(t, "club_admin")
	_, err := e.svc.SubmitOffering(ctxOf(t, a), a, v.Offering.ID)
	wantProblem(t, err, http.StatusConflict, "parties.no_approved_account")
	wantErr(t, err, parties.ErrNoApprovedAccount)
	got, _ := e.store.GetOffering(t.Context(), v.Offering.ID, false)
	if got.Status != catalog.OfferingDraft || got.SubmittedAt != nil {
		t.Fatalf("offering after a refused submission: %s", got.Status)
	}
	if n := len(e.outbox.Events()); n != 0 {
		t.Fatalf("a refused submission wrote %d events", n)
	}

	inactive := e.payees.add("club_a", parties.PayeeSuspended, true)
	v2 := e.offering(t, "club_admin", "club_a", func(in *catalog.OfferingInput) { in.DefaultPayeeID = &inactive })
	_, err = e.svc.SubmitOffering(ctxOf(t, a), a, v2.Offering.ID)
	wantProblem(t, err, http.StatusConflict, "catalog.payee_not_active")

	e.payees.setApproved(noAccount, true)
	sub, err := e.svc.SubmitOffering(ctxOf(t, a), a, v.Offering.ID)
	if err != nil || sub.Offering.Status != catalog.OfferingPendingApproval {
		t.Fatalf("submit after approval: %v %s", err, sub.Offering.Status)
	}
	// The account is revoked before the platform decides: no sale.
	e.payees.setApproved(noAccount, false)
	pad := actor(t, "platform_admin")
	_, err = e.svc.AdminDecideOffering(ctxOf(t, pad), pad, v.Offering.ID, catalog.Decision{Approve: true})
	wantErr(t, err, parties.ErrNoApprovedAccount)
	got, _ = e.store.GetOffering(t.Context(), v.Offering.ID, false)
	if got.Status != catalog.OfferingPendingApproval || got.ApprovedBy != nil {
		t.Fatalf("approval without an approved account changed the offering: %s", got.Status)
	}
}

// TestDecideOffering_CreatorCannotApprove_403 (03 §6.1 "Teklif onayı",
// k24): the platform admin who created a platform offering may not
// approve it (403 authz.four_eyes_violation, also in the database:
// offerings_approver_chk), another platform admin does; a club admin is
// never an approver; a decided offering is 409.
func TestDecideOffering_CreatorCannotApprove_403(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.defaultPolicy(t)
	// platform_admin acts for the platform org (k-po: a platform-kind membership).
	creator := actor(t, "platform_admin")
	creator.Memberships = append(creator.Memberships, authz.Membership{OrgID: org("platform_org"), OrgKind: authz.OrgPlatform,
		Role: authz.MemberStaff})
	ctx := ctxOf(t, creator)
	v, err := e.svc.CreateOffering(ctx, creator, org("platform_org"), catalog.OfferingInput{ServiceID: e.quota,
		Title: "Hedefli gönderi paketi (5)", UnitPriceAmount: 250_000, InvoiceTrigger: catalog.InvoiceOnPayment})
	if err != nil {
		t.Fatalf("CreateOffering (platform): %v", err)
	}
	if v.Offering.SellerKind != catalog.SellerPlatform || v.Offering.DefaultPayeeID != nil {
		t.Fatalf("platform offering = %s payee %v", v.Offering.SellerKind, v.Offering.DefaultPayeeID)
	}
	if _, err := e.svc.SubmitOffering(ctx, creator, v.Offering.ID); err != nil {
		t.Fatalf("SubmitOffering: %v", err)
	}
	_, err = e.svc.AdminDecideOffering(ctx, creator, v.Offering.ID, catalog.Decision{Approve: true})
	wantErr(t, err, authz.ErrFourEyes)
	wantProblem(t, err, http.StatusForbidden, "authz.four_eyes_violation")
	ca := actor(t, "club_admin")
	_, err = e.svc.AdminDecideOffering(ctxOf(t, ca), ca, v.Offering.ID, catalog.Decision{Approve: true})
	wantErr(t, err, authz.ErrForbidden)

	// The database refuses it too (the in-memory store mirrors offerings_approver_chk).
	cur, _ := e.store.GetOffering(t.Context(), v.Offering.ID, false)
	now := t0
	forged := cur
	forged.Status, forged.ApprovedBy, forged.ApprovedAt = catalog.OfferingActive, &creator.UserID, &now
	if _, err := e.store.UpdateOffering(t.Context(), forged, catalog.OfferingPendingApproval); !errors.Is(err, authz.ErrFourEyes) {
		t.Fatalf("store approval by the creator: %v, want authz.four_eyes_violation", err)
	}

	pad2 := actor(t, "platform_admin_2")
	got, err := e.svc.AdminDecideOffering(ctxOf(t, pad2), pad2, v.Offering.ID, catalog.Decision{Approve: true})
	if err != nil || got.Offering.Status != catalog.OfferingActive || *got.Offering.ApprovedBy != pad2.UserID {
		t.Fatalf("approval by another admin: %v %+v", err, got.Offering.Status)
	}
	_, err = e.svc.AdminDecideOffering(ctxOf(t, pad2), pad2, v.Offering.ID, catalog.Decision{Approve: true})
	wantProblem(t, err, http.StatusConflict, "catalog.invalid_state")
	if evs := e.audit.of(authz.CatalogOfferingDecide); len(evs) != 1 || evs[0].Metadata["outcome"] != catalog.OutcomeApproved {
		t.Fatalf("decide audit = %+v", evs)
	}
	if topics := e.outbox.Topics(); !slices.Contains(topics, domainevent.TopicOfferingDecided) {
		t.Fatalf("topics = %v, want offering.decided", topics)
	}
}

// TestReserve_ConcurrentLastUnit_OneSucceeds (02 §18.2): 20 buyers race for
// the last unit; exactly one reservation succeeds, the others are 409
// catalog.sold_out, and the offering ends sold_out with its stock exact.
// The PostgreSQL store passes the same race (store_integration_test.go).
func TestReserve_ConcurrentLastUnit_OneSucceeds(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.defaultPolicy(t)
	v := e.offering(t, "club_admin", "club_a", func(in *catalog.OfferingInput) { in.QuantityTotal = ptr(1) })
	e.active(t, "club_admin", v.Offering.ID)
	var wg sync.WaitGroup
	errs := make([]error, 20)
	for i := range errs {
		wg.Go(func() { errs[i] = e.mod.Offerings.Reserve(t.Context(), v.Offering.ID, 1, uuid.NewV7()) })
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, catalog.ErrSoldOut):
		default:
			t.Errorf("reserve: %v", err)
		}
	}
	got, _ := e.store.GetOffering(t.Context(), v.Offering.ID, false)
	if ok != 1 || got.Status != catalog.OfferingSoldOut || got.QuantitySold != 1 {
		t.Fatalf("reservations = %d, offering %s sold %d; want 1, sold_out, 1", ok, got.Status, got.QuantitySold)
	}
	_, err := e.mod.Offerings.ForCheckout(t.Context(), v.Offering.ID)
	wantErr(t, err, catalog.ErrSoldOut)
	// An expired order gives the unit back: on sale again.
	if err := e.mod.Offerings.Release(t.Context(), v.Offering.ID, 1); err != nil {
		t.Fatalf("Release: %v", err)
	}
	got, _ = e.store.GetOffering(t.Context(), v.Offering.ID, false)
	if got.Status != catalog.OfferingActive || got.QuantitySold != 0 {
		t.Fatalf("after release: %s sold %d", got.Status, got.QuantitySold)
	}
	if err := e.mod.Offerings.Release(t.Context(), v.Offering.ID, 1); !errors.Is(err, catalog.ErrInvalidState) {
		t.Fatalf("releasing more than sold: %v", err)
	}
}

// TestReserve_PerBuyerLimit: per_buyer_limit counts the units the buyer
// holds in its open orders (BuyerHeld, BE-040) plus the new ones; other
// buyers are not limited by it; a draft or an offering whose sales ended is
// not on sale.
func TestReserve_PerBuyerLimit(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.defaultPolicy(t)
	v := e.offering(t, "club_admin", "club_a", func(in *catalog.OfferingInput) {
		in.QuantityTotal, in.PerBuyerLimit, in.SalesEndAt = ptr(10), ptr(2), ptr(t0.Add(48*time.Hour))
	})
	_, err := e.mod.Offerings.ForCheckout(t.Context(), v.Offering.ID)
	wantErr(t, err, catalog.ErrNotOnSale) // a draft
	e.active(t, "club_admin", v.Offering.ID)
	off := e.mod.Offerings
	buyer, other := org("company_a"), org("company_b")
	if err := off.Reserve(t.Context(), v.Offering.ID, 3, buyer); !errors.Is(err, catalog.ErrPerBuyerLimit) {
		t.Fatalf("3 units at once (limit 2): %v", err)
	}
	if err := off.Reserve(t.Context(), v.Offering.ID, 1, buyer); err != nil {
		t.Fatalf("first unit: %v", err)
	}
	e.held.set(v.Offering.ID, buyer, 1) // orders now holds one unit for the buyer
	if err := off.Reserve(t.Context(), v.Offering.ID, 2, buyer); !errors.Is(err, catalog.ErrPerBuyerLimit) {
		t.Fatalf("1 held + 2 (limit 2): %v", err)
	}
	wantProblem(t, off.Reserve(t.Context(), v.Offering.ID, 2, buyer), http.StatusConflict, "catalog.per_buyer_limit")
	if err := off.Reserve(t.Context(), v.Offering.ID, 1, buyer); err != nil {
		t.Fatalf("1 held + 1 (limit 2): %v", err)
	}
	if err := off.Reserve(t.Context(), v.Offering.ID, 2, other); err != nil {
		t.Fatalf("another buyer: %v", err)
	}
	got, _ := e.store.GetOffering(t.Context(), v.Offering.ID, false)
	if got.QuantitySold != 4 {
		t.Fatalf("sold = %d, want 4", got.QuantitySold)
	}
	if err := off.Reserve(t.Context(), v.Offering.ID, 0, other); err == nil {
		t.Fatal("a zero quantity was reserved")
	}
	co, err := off.ForCheckout(t.Context(), v.Offering.ID)
	if err != nil || co.SellerOrgID != org("club_a") || co.DefaultPayeeID == nil || *co.DefaultPayeeID != e.payeeA ||
		co.VATRateBP != 2000 || co.VATClass != catalog.VATStandard || *co.QuantityRemaining != 6 || len(co.Deliverables) != 2 {
		t.Fatalf("ForCheckout = %+v, %v", co, err)
	}
	// The sales window ends: nothing more is reserved.
	e.clk.Advance(49 * time.Hour)
	if err := off.Reserve(t.Context(), v.Offering.ID, 1, other); !errors.Is(err, catalog.ErrNotOnSale) {
		t.Fatalf("after the sales window: %v", err)
	}
}

// policy writes a policy straight into the store (any period: the service
// refuses past starts).
func (e *env) policy(t *testing.T, scope catalog.PolicyScope, scopeID *uuid.UUID, rate int, from time.Time, to *time.Time) catalog.CommercePolicy {
	t.Helper()
	p, err := e.store.InsertPolicy(t.Context(), catalog.CommercePolicy{
		ID: uuid.NewV7(), Scope: scope, ScopeID: scopeID, ModelCode: catalog.ModelA, CollectionMethod: catalog.CollectBankTransfer,
		CommissionRateBP: rate, CommissionBasis: catalog.BasisNetExclVAT, CommissionPayer: catalog.PayerSeller,
		CommissionVATRateBP: 2000, InvoiceAutomation: catalog.InvoiceManual, DisputeWindowDays: 7, ValidFrom: from, ValidTo: to,
		CreatedBy: authztest.ID("finance_admin"),
	})
	if err != nil {
		t.Fatalf("InsertPolicy: %v", err)
	}
	return p
}

// TestResolvePolicy_Precedence_OfferingServiceSellerDefault (02 §18.3):
// offering > service > seller_org > default; without any policy the
// offering cannot be sold (409 catalog.no_commerce_policy, T-10).
func TestResolvePolicy_Precedence_OfferingServiceSellerDefault(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	from := t0.Add(-time.Hour)
	v := e.offering(t, "club_admin", "club_a", nil)
	other := e.offering(t, "other_club_admin", "club_b", nil)
	res := func(id uuid.UUID) (int, error) {
		p, err := e.mod.Offerings.ResolvePolicy(t.Context(), id, t0)
		return p.CommissionRateBP, err
	}
	if _, err := res(v.Offering.ID); !errors.Is(err, catalog.ErrNoPolicy) {
		t.Fatalf("no policy: %v", err)
	}
	wantProblem(t, func() error { _, err := res(v.Offering.ID); return err }(), http.StatusConflict, "catalog.no_commerce_policy")
	steps := []struct {
		scope catalog.PolicyScope
		id    *uuid.UUID
		rate  int
	}{
		{catalog.ScopeDefault, nil, 1000},
		{catalog.ScopeSellerOrg, ptr(org("club_a")), 800},
		{catalog.ScopeService, ptr(e.booth), 600},
		{catalog.ScopeOffering, &v.Offering.ID, 400},
	}
	for _, s := range steps {
		e.policy(t, s.scope, s.id, s.rate, from, nil)
		if got, err := res(v.Offering.ID); err != nil || got != s.rate {
			t.Fatalf("after the %s policy: rate %d (%v), want %d", s.scope, got, err, s.rate)
		}
	}
	// club_b's offering shares the service: the service policy wins over the default.
	if got, _ := res(other.Offering.ID); got != 600 {
		t.Fatalf("other offering of the service: %d, want 600", got)
	}
	if _, err := e.mod.Offerings.ResolvePolicy(t.Context(), uuid.NewV7(), t0); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("unknown offering: %v", err)
	}
}

// TestResolvePolicy_AtTimeUsesVersion: a change is "close + new row"; the
// resolution at a time uses the row in effect then (orders freeze it), the
// closed row stays as it was.
func TestResolvePolicy_AtTimeUsesVersion(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	v := e.offering(t, "club_admin", "club_a", nil)
	jan := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	change := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	old := e.policy(t, catalog.ScopeDefault, nil, 1000, jan, &change)
	cur := e.policy(t, catalog.ScopeDefault, nil, 1200, change, nil)
	for _, c := range []struct {
		at   time.Time
		want uuid.UUID
	}{
		{jan, old.ID}, {change.Add(-time.Microsecond), old.ID}, {change, cur.ID}, {t0, cur.ID},
	} {
		p, err := e.mod.Offerings.ResolvePolicy(t.Context(), v.Offering.ID, c.at)
		if err != nil || p.ID != c.want {
			t.Fatalf("at %s: %s (%v), want %s", c.at, p.ID, err, c.want)
		}
	}
	if _, err := e.mod.Offerings.ResolvePolicy(t.Context(), v.Offering.ID, jan.Add(-time.Second)); !errors.Is(err, catalog.ErrNoPolicy) {
		t.Fatalf("before the first policy: %v", err)
	}
	// The admin list "at" keeps the version in effect.
	pad := actor(t, "platform_admin")
	at := change.Add(-time.Hour)
	ps, err := e.svc.AdminListPolicies(ctxOf(t, pad), pad, catalog.PolicyFilter{At: &at})
	if err != nil || len(ps) != 1 || ps[0].ID != old.ID {
		t.Fatalf("list at %s = %+v %v", at, ps, err)
	}
}

// TestCreatePolicy_OverlappingRange_409 (T-43): periods of one scope never
// overlap (409 catalog.policy_overlap); adjacent periods and other scopes
// are fine; no policy starts in the past; the rate has no default.
func TestCreatePolicy_OverlappingRange_409(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	fin := actor(t, "finance_admin")
	ctx := ctxOf(t, fin)
	may, jun := t0.Add(24*time.Hour), t0.Add(30*24*time.Hour)
	in := catalog.DefaultPolicy(1000)
	in.ValidFrom, in.ValidTo = &may, &jun
	if _, err := e.svc.AdminCreatePolicy(ctx, fin, in, proof()); err != nil {
		t.Fatalf("first policy: %v", err)
	}
	overlap := in
	mid := may.Add(48 * time.Hour)
	overlap.ValidFrom, overlap.ValidTo = &mid, nil
	_, err := e.svc.AdminCreatePolicy(ctx, fin, overlap, proof())
	wantProblem(t, err, http.StatusConflict, "catalog.policy_overlap")
	adjacent := in
	adjacent.ValidFrom, adjacent.ValidTo = &jun, nil
	if _, err := e.svc.AdminCreatePolicy(ctx, fin, adjacent, proof()); err != nil {
		t.Fatalf("adjacent period: %v", err)
	}
	club := catalog.DefaultPolicy(500)
	club.Scope, club.ScopeID, club.ValidFrom = catalog.ScopeSellerOrg, ptr(org("club_a")), &may
	if _, err := e.svc.AdminCreatePolicy(ctx, fin, club, proof()); err != nil {
		t.Fatalf("the same period on another scope: %v", err)
	}
	past := catalog.DefaultPolicy(1000)
	past.Scope, past.ScopeID, past.ValidFrom = catalog.ScopeService, &e.booth, ptr(t0.Add(-time.Minute))
	_, err = e.svc.AdminCreatePolicy(ctx, fin, past, proof())
	wantProblem(t, err, http.StatusUnprocessableEntity, "validation_failed")
	noRate := catalog.DefaultPolicy(1000)
	noRate.CommissionRateBP = nil
	_, err = e.svc.AdminCreatePolicy(ctx, fin, noRate, proof())
	wantProblem(t, err, http.StatusUnprocessableEntity, "validation_failed")
	unknown := catalog.DefaultPolicy(1000)
	unknown.Scope, unknown.ScopeID = catalog.ScopeService, ptr(uuid.NewV7())
	_, err = e.svc.AdminCreatePolicy(ctx, fin, unknown, proof())
	wantProblem(t, err, http.StatusUnprocessableEntity, "validation_failed")
	company := catalog.DefaultPolicy(1000)
	company.Scope, company.ScopeID = catalog.ScopeSellerOrg, ptr(org("company_a"))
	_, err = e.svc.AdminCreatePolicy(ctx, fin, company, proof())
	wantProblem(t, err, http.StatusUnprocessableEntity, "validation_failed")
	c := catalog.DefaultPolicy(1000)
	c.ModelCode, c.CollectionMethod, c.CommissionPayer = catalog.ModelC, "", catalog.PayerSeller
	_, err = e.svc.AdminCreatePolicy(ctx, fin, c, proof())
	wantProblem(t, err, http.StatusUnprocessableEntity, "validation_failed") // model C charges the buyer
	c.CommissionPayer, c.Scope, c.ScopeID = "", catalog.ScopeOffering, nil
	if _, err := e.svc.AdminCreatePolicy(ctx, fin, c, proof()); err == nil {
		t.Fatal("an offering scope without its id was stored")
	}
}

// TestClosePolicy_RowImmutable (02 §18.3, koro_policy_close_only): a
// policy is only closed — once, never in the past — and a closed row
// takes no other change; closing a policy that has not started makes it
// never apply.
func TestClosePolicy_RowImmutable(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	p := e.defaultPolicy(t)
	pad := actor(t, "platform_admin")
	ctx := ctxOf(t, pad)
	_, err := e.svc.AdminClosePolicy(ctx, pad, p.ID, ptr(t0.Add(-time.Hour)), proof())
	wantProblem(t, err, http.StatusUnprocessableEntity, "validation_failed") // not in the past
	end := t0.Add(24 * time.Hour)
	closed, err := e.svc.AdminClosePolicy(ctx, pad, p.ID, &end, proof())
	if err != nil || closed.ValidTo == nil || !closed.ValidTo.Equal(end) {
		t.Fatalf("close: %+v %v", closed.ValidTo, err)
	}
	_, err = e.svc.AdminClosePolicy(ctx, pad, p.ID, nil, proof())
	wantProblem(t, err, http.StatusConflict, "catalog.invalid_state")
	if _, err := e.store.ClosePolicy(t.Context(), p.ID, end.Add(time.Hour)); !errors.Is(err, catalog.ErrInvalidState) {
		t.Fatalf("store re-close: %v", err)
	}
	// Every other column is as created.
	got, _ := e.store.GetPolicy(t.Context(), p.ID, false)
	if got.CommissionRateBP != p.CommissionRateBP || got.ModelCode != p.ModelCode || !got.ValidFrom.Equal(p.ValidFrom) ||
		got.CreatedBy != p.CreatedBy || got.Scope != p.Scope {
		t.Fatalf("closed row changed: %+v, created %+v", got, p)
	}
	// The next version starts where the closed one ends.
	next := catalog.DefaultPolicy(1100)
	next.ValidFrom = &end
	if _, err := e.svc.AdminCreatePolicy(ctx, pad, next, proof()); err != nil {
		t.Fatalf("the next version: %v", err)
	}
	// A policy that has not started is closed at its start + 1 µs.
	later := t0.Add(90 * 24 * time.Hour)
	svc := catalog.DefaultPolicy(700)
	svc.Scope, svc.ScopeID, svc.ValidFrom = catalog.ScopeService, &e.booth, &later
	future, err := e.svc.AdminCreatePolicy(ctx, pad, svc, proof())
	if err != nil {
		t.Fatal(err)
	}
	gone, err := e.svc.AdminClosePolicy(ctx, pad, future.ID, nil, proof())
	if err != nil || !gone.ValidTo.Equal(later.Add(time.Microsecond)) {
		t.Fatalf("closing a future policy: %v %v", gone.ValidTo, err)
	}
	_, err = e.svc.AdminClosePolicy(ctx, pad, uuid.NewV7(), nil, proof())
	wantErr(t, err, catalog.ErrNotFound)
}

// TestPolicyChange_RequiresReauth_NotifiesAllAdmins (06 H-17): one person
// changes a policy — with re-authentication (403 reauth_required /
// reauth_failed, nothing written but the denied records, BE-074), audited
// with the reauth kind — and every
// platform and finance admin is notified in-app and by e-mail (A task).
func TestPolicyChange_RequiresReauth_NotifiesAllAdmins(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	fin := actor(t, "finance_admin")
	ctx := ctxOf(t, fin)
	_, err := e.svc.AdminCreatePolicy(ctx, fin, catalog.DefaultPolicy(1000), identity.ReauthProof{})
	wantErr(t, err, identity.ErrReauthRequired)
	_, err = e.svc.AdminCreatePolicy(ctx, fin, catalog.DefaultPolicy(1000), identity.ReauthProof{Password: "wrong"})
	wantErr(t, err, identity.ErrReauthFailed)
	if ps, _ := e.store.ListPolicies(t.Context(), catalog.PolicyFilter{}); len(ps) != 0 || len(e.queue.Tasks()) != 0 {
		t.Fatalf("a refused change wrote %d policies, %d tasks", len(ps), len(e.queue.Tasks()))
	}
	// Roles without the cell are refused before the proof is asked.
	for _, name := range []string{"club_admin", "company_admin", "moderator", "support", "dpo"} {
		a := actor(t, name)
		_, err := e.svc.AdminCreatePolicy(ctxOf(t, a), a, catalog.DefaultPolicy(1000), proof())
		wantErr(t, err, authz.ErrForbidden)
	}
	p, err := e.svc.AdminCreatePolicy(ctx, fin, catalog.DefaultPolicy(1000), proof())
	if err != nil {
		t.Fatal(err)
	}
	if n := len(e.audit.ofOutcome(authz.CatalogPolicyManage, audit.Denied)); n != 2 {
		t.Fatalf("%d denied records for the two refused proofs, want 2", n)
	}
	evs := e.audit.ofOutcome(authz.CatalogPolicyManage, audit.Success)
	if len(evs) != 1 || evs[0].Metadata["reauth"] != "password" || evs[0].Metadata["commission_rate_bp"] != 1000 ||
		*evs[0].ActorUserID != fin.UserID {
		t.Fatalf("policy audit = %+v", evs)
	}
	pad := actor(t, "platform_admin")
	if _, err := e.svc.AdminClosePolicy(ctxOf(t, pad), pad, p.ID, nil, identity.ReauthProof{}); !errors.Is(err, identity.ErrReauthRequired) {
		t.Fatalf("close without proof: %v", err)
	}
	if _, err := e.svc.AdminClosePolicy(ctxOf(t, pad), pad, p.ID, ptr(t0.Add(time.Hour)), proof()); err != nil {
		t.Fatal(err)
	}
	tasks := e.queue.Tasks()
	if len(tasks) != 2 || tasks[0].Type != catalog.NotifyPolicyChangedTaskName || tasks[0].Queue != "critical" {
		t.Fatalf("tasks = %+v", tasks)
	}
	for _, task := range tasks {
		if err := e.mod.Tasks()[0].Handle(t.Context(), task.Payload); err != nil {
			t.Fatalf("notice task: %v", err)
		}
	}
	got := map[string][]notify.Channel{}
	for _, m := range e.sender.all() {
		if m.Template != "catalog.commerce_policy_changed" {
			t.Fatalf("template %s", m.Template)
		}
		got[m.UserID.String()] = append(got[m.UserID.String()], m.Channel)
		if m.Channel == notify.ChannelEmail && (m.Data["commission_rate"] != "10" || m.Data["model_code"] != "A") {
			t.Fatalf("mail data = %+v", m.Data)
		}
	}
	for _, name := range []string{"platform_admin", "platform_admin_2", "finance_admin", "finance_admin_2"} {
		if chs := got[authztest.ID(name).String()]; len(chs) != 4 { // in-app + e-mail, twice (created, closed)
			t.Errorf("%s got %v", name, chs)
		}
	}
	if len(got) != 4 {
		t.Fatalf("notified %d users, want the 4 admins", len(got))
	}
}

// TestEventCancelled_CancelsLinkedOfferings (01 §6.2): event.cancelled
// cancels the offerings bound to the event (any status but cancelled), each
// with offering.cancelled (O, reason event_cancelled); offerings of other
// events stay; a second delivery changes nothing; a payload for an event
// that is not cancelled is ignored.
func TestEventCancelled_CancelsLinkedOfferings(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.defaultPolicy(t)
	ev, other := e.events.add("club_a", events.StatusPublished), e.events.add("club_a", events.StatusPublished)
	draft := e.offering(t, "club_admin", "club_a", func(in *catalog.OfferingInput) { in.EventID = &ev })
	live := e.offering(t, "club_admin", "club_a", func(in *catalog.OfferingInput) { in.EventID = &ev })
	e.active(t, "club_admin", live.Offering.ID)
	keep := e.offering(t, "club_admin", "club_a", func(in *catalog.OfferingInput) { in.EventID = &other })
	env, err := domainevent.New(domainevent.EventCancelled{EventID: ev, OrgID: org("club_a")}, "event", ev)
	if err != nil {
		t.Fatal(err)
	}
	var handle func() error
	for _, s := range subscriptions(t, e) {
		if s.topic == domainevent.TopicEventCancelled {
			handle = func() error { return s.h(t.Context(), env) }
		}
	}
	if handle == nil {
		t.Fatal("no event.cancelled subscription")
	}
	// Not cancelled yet: a stale or forged payload changes nothing.
	if err := handle(); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.store.GetOffering(t.Context(), live.Offering.ID, false); got.Status != catalog.OfferingActive {
		t.Fatalf("offering of a live event: %s", got.Status)
	}
	e.events.set(ev, events.StatusCancelled)
	for range 2 {
		if err := handle(); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []uuid.UUID{draft.Offering.ID, live.Offering.ID} {
		got, _ := e.store.GetOffering(t.Context(), id, false)
		if got.Status != catalog.OfferingCancelled || got.CancelReason == nil || *got.CancelReason != "event_cancelled" {
			t.Fatalf("offering %s: %s", id, got.Status)
		}
	}
	if got, _ := e.store.GetOffering(t.Context(), keep.Offering.ID, false); got.Status != catalog.OfferingDraft {
		t.Fatalf("offering of another event: %s", got.Status)
	}
	var cancelled []domainevent.OfferingCancelled
	for _, ev := range e.outbox.Events() {
		if ev.Topic == domainevent.TopicOfferingCancelled {
			p, _ := domainevent.Decode[domainevent.OfferingCancelled](ev)
			cancelled = append(cancelled, p)
		}
	}
	if len(cancelled) != 2 || cancelled[0].Reason != catalog.CancelEventCancelled || cancelled[1].OrgID != org("club_a") {
		t.Fatalf("offering.cancelled = %+v", cancelled)
	}
	evs := e.audit.of(authz.CatalogOfferingCancel)
	if len(evs) != 2 || evs[0].SystemJob != catalog.CancelEventOfferingsTaskName || evs[0].Metadata["cause"] != "event_cancelled" {
		t.Fatalf("cancel audit = %+v", evs)
	}
	// The offering.cancelled notice reaches the club's admins.
	for _, s := range subscriptions(t, e) {
		if s.topic != domainevent.TopicOfferingCancelled {
			continue
		}
		for _, ev := range e.outbox.Events() {
			if ev.Topic == domainevent.TopicOfferingCancelled {
				if err := s.h(t.Context(), ev); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if n := countTemplate(e.sender.all(), "catalog.offering_cancelled", authztest.ID("club_admin")); n != 2 {
		t.Fatalf("cancel notices to club_admin = %d, want 2", n)
	}
}

// TestUniversityApprovalRequired_Flow (T-21): an offering of a club that
// requires its university's approval waits for both decisions in any
// order; only the club's university's rep decides it (other universities'
// reps: 404; the platform: 403); the platform's approval alone does not
// put it on sale; a university rejection sends it back to draft.
func TestUniversityApprovalRequired_Flow(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.defaultPolicy(t)
	v := e.offering(t, "club_admin", "club_a", func(in *catalog.OfferingInput) { in.RequiresUniversityApproval = true })
	if v.Offering.UniversityApprovalStatus != catalog.UniPending {
		t.Fatalf("a draft that needs its university: %s", v.Offering.UniversityApprovalStatus)
	}
	ca, rep, pad := actor(t, "club_admin"), actor(t, "university_rep"), actor(t, "platform_admin")
	// The rep does not see a draft (not under review yet).
	if _, err := e.svc.GetOffering(ctxOf(t, rep), rep, v.Offering.ID); !errors.Is(err, authz.ErrNotFound) {
		t.Fatalf("rep reads the draft: %v", err)
	}
	if _, err := e.svc.SubmitOffering(ctxOf(t, ca), ca, v.Offering.ID); err != nil {
		t.Fatal(err)
	}
	q, err := e.svc.ListUniversityApprovals(ctxOf(t, rep), rep, org("uni_a"), catalog.QueueQuery{})
	if err != nil || len(q.Items) != 1 || q.Items[0].Offering.ID != v.Offering.ID {
		t.Fatalf("uni_a queue = %+v %v", q.Items, err)
	}
	// The rep of another university: no grant on uni_b's queue for uni_a's offering.
	otherRep := rep
	otherRep.Memberships = []authz.Membership{{OrgID: org("uni_b"), OrgKind: authz.OrgUniversity, Role: authz.MemberUniversityRep}}
	q, err = e.svc.ListUniversityApprovals(ctxOf(t, otherRep), otherRep, org("uni_b"), catalog.QueueQuery{})
	if err != nil || len(q.Items) != 0 {
		t.Fatalf("uni_b queue = %+v %v", q.Items, err)
	}
	_, err = e.svc.DecideUniversityApproval(ctxOf(t, otherRep), otherRep, org("uni_b"), v.Offering.ID, catalog.Decision{Approve: true})
	wantErr(t, err, catalog.ErrNotFound)
	_, err = e.svc.DecideUniversityApproval(ctxOf(t, pad), pad, org("uni_a"), v.Offering.ID, catalog.Decision{Approve: true})
	wantErr(t, err, authz.ErrForbidden)
	_, err = e.svc.ListUniversityApprovals(ctxOf(t, ca), ca, org("club_a"), catalog.QueueQuery{})
	wantErr(t, err, authz.ErrForbidden)

	// The platform approves first: it waits for the university.
	got, err := e.svc.AdminDecideOffering(ctxOf(t, pad), pad, v.Offering.ID, catalog.Decision{Approve: true})
	if err != nil || got.Offering.Status != catalog.OfferingPendingApproval || got.Offering.ApprovedBy == nil {
		t.Fatalf("platform approval: %v %s", err, got.Offering.Status)
	}
	if _, err := e.mod.Offerings.ForCheckout(t.Context(), v.Offering.ID); !errors.Is(err, catalog.ErrNotOnSale) {
		t.Fatalf("on sale before the university: %v", err)
	}
	if _, err := e.svc.GetOffering(ctxOf(t, rep), rep, v.Offering.ID); err != nil {
		t.Fatalf("rep reads the offering under review: %v", err)
	}
	got, err = e.svc.DecideUniversityApproval(ctxOf(t, rep), rep, org("uni_a"), v.Offering.ID, catalog.Decision{Approve: true})
	if err != nil || got.Offering.Status != catalog.OfferingActive || got.Offering.UniversityApprovalStatus != catalog.UniApproved {
		t.Fatalf("university approval: %v %s/%s", err, got.Offering.Status, got.Offering.UniversityApprovalStatus)
	}
	_, err = e.svc.DecideUniversityApproval(ctxOf(t, rep), rep, org("uni_a"), v.Offering.ID, catalog.Decision{Approve: true})
	wantErr(t, err, authz.ErrForbidden) // no longer under the university's review

	// Another offering: the university rejects it first → back to draft.
	w := e.offering(t, "club_admin", "club_a", func(in *catalog.OfferingInput) { in.RequiresUniversityApproval = true })
	if _, err := e.svc.SubmitOffering(ctxOf(t, ca), ca, w.Offering.ID); err != nil {
		t.Fatal(err)
	}
	got, err = e.svc.DecideUniversityApproval(ctxOf(t, rep), rep, org("uni_a"), w.Offering.ID,
		catalog.Decision{Reason: "Yönerge gereği alkol sponsoru kabul edilmiyor"})
	if err != nil || got.Offering.Status != catalog.OfferingDraft || got.Offering.UniversityApprovalStatus != catalog.UniRejected ||
		got.Offering.RejectionReason == nil {
		t.Fatalf("university rejection: %v %+v", err, got.Offering)
	}
	_, err = e.svc.AdminDecideOffering(ctxOf(t, pad), pad, w.Offering.ID, catalog.Decision{Approve: true})
	wantProblem(t, err, http.StatusConflict, "catalog.invalid_state")
	// Resubmitted, it asks the university again.
	got, err = e.svc.SubmitOffering(ctxOf(t, ca), ca, w.Offering.ID)
	if err != nil || got.Offering.UniversityApprovalStatus != catalog.UniPending || got.Offering.UniversityApprovedBy != nil {
		t.Fatalf("resubmission: %v %+v", err, got.Offering)
	}
	// A university or the platform cannot ask for a university's approval.
	_, err = e.svc.CreateOffering(ctxOf(t, rep), rep, org("uni_a"), catalog.OfferingInput{ServiceID: e.booth, Title: "Kariyer günü standı",
		UnitPriceAmount: 500_000, RequiresUniversityApproval: true})
	wantProblem(t, err, http.StatusUnprocessableEntity, "validation_failed")
}

// TestBrowseOfferings_OnlyActive_RequiresSession (T-57, 04 §2.14): the
// marketplace lists only active offerings whose sales have not ended, for
// signed-in users only (anonymous 401); the buyer sees neither the payee nor
// the approval block; a draft of another org is 404.
func TestBrowseOfferings_OnlyActive_RequiresSession(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.defaultPolicy(t)
	live := e.offering(t, "club_admin", "club_a", nil)
	e.active(t, "club_admin", live.Offering.ID)
	ending := e.offering(t, "club_admin", "club_a", func(in *catalog.OfferingInput) { in.SalesEndAt = ptr(t0.Add(time.Hour)) })
	e.active(t, "club_admin", ending.Offering.ID)
	draft := e.offering(t, "club_admin", "club_a", nil)
	pending := e.offering(t, "club_admin", "club_a", nil)
	ca := actor(t, "club_admin")
	if _, err := e.svc.SubmitOffering(ctxOf(t, ca), ca, pending.Offering.ID); err != nil {
		t.Fatal(err)
	}
	cancelled := e.offering(t, "club_admin", "club_a", nil)
	e.active(t, "club_admin", cancelled.Offering.ID)
	if _, err := e.svc.CancelOffering(ctxOf(t, ca), ca, cancelled.Offering.ID, "Etkinlik ertelendi"); err != nil {
		t.Fatal(err)
	}
	app := newHTTP(t, e)
	rec, _ := call(t, app, "anonymous", http.MethodGet, "/api/v1/offerings", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous browse: %d", rec.Code)
	}
	rec, _ = call(t, app, "anonymous", http.MethodGet, "/api/v1/offerings/"+live.Offering.ID.String(), nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous read: %d", rec.Code)
	}
	ids := func(persona string) []string {
		rec, body := call(t, app, persona, http.MethodGet, "/api/v1/offerings?sort=sales_end_at", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s browse: %d %s", persona, rec.Code, rec.Body.String())
		}
		var out []string
		for _, it := range body["items"].([]any) {
			m := it.(map[string]any)
			if _, ok := m["approval"]; ok {
				t.Fatalf("a card carries the approval block: %v", m)
			}
			out = append(out, m["offering_id"].(string))
		}
		return out
	}
	want := []string{ending.Offering.ID.String(), live.Offering.ID.String()} // sales end first, open-ended last
	for _, p := range []string{"company_admin", "student", "user_unverified", "student_minor", "platform_admin"} {
		if got := ids(p); !slices.Equal(got, want) {
			t.Fatalf("%s browses %v, want %v", p, got, want)
		}
	}
	e.clk.Advance(2 * time.Hour) // the first one's sales end
	if got := ids("company_admin"); !slices.Equal(got, want[1:]) {
		t.Fatalf("after the sales end: %v", got)
	}
	// The buyer reads the live offering without the payee; the seller with it (and the ETag).
	rec, body := call(t, app, "company_admin", http.MethodGet, "/api/v1/offerings/"+live.Offering.ID.String(), nil)
	if rec.Code != http.StatusOK || body["approval"] != nil || rec.Header().Get("ETag") != "" {
		t.Fatalf("buyer read: %d %v etag %q", rec.Code, body, rec.Header().Get("ETag"))
	}
	rec, body = call(t, app, "club_admin", http.MethodGet, "/api/v1/offerings/"+live.Offering.ID.String(), nil)
	appr, _ := body["approval"].(map[string]any)
	if rec.Code != http.StatusOK || appr["default_payee_id"] != e.payeeA.String() || rec.Header().Get("ETag") == "" {
		t.Fatalf("seller read: %d %v", rec.Code, body)
	}
	// Drafts and offerings under review: 404 outside the seller (IDOR), 403 for a member without a role.
	for _, id := range []uuid.UUID{draft.Offering.ID, pending.Offering.ID} {
		for persona, code := range map[string]int{"company_admin": 404, "other_club_admin": 404, "student": 404,
			"club_member": 403, "finance_admin": 403, "club_admin": 200, "platform_admin": 200} {
			if rec, _ := call(t, app, persona, http.MethodGet, "/api/v1/offerings/"+id.String(), nil); rec.Code != code {
				t.Errorf("%s reads %s: %d, want %d", persona, id, rec.Code, code)
			}
		}
	}
	// A cancelled offering that was on sale stays readable (orders reference it).
	if rec, _ := call(t, app, "company_admin", http.MethodGet, "/api/v1/offerings/"+cancelled.Offering.ID.String(), nil); rec.Code != 200 {
		t.Fatalf("cancelled offering: %d", rec.Code)
	}
}

type sub struct {
	topic string
	h     func(ctx context.Context, e domainevent.Envelope) error
}

// subscriptions returns the outbox subscriptions catalog registers.
func subscriptions(t *testing.T, e *env) []sub {
	t.Helper()
	r := outbox.NewRegistry()
	e.mod.Subscribe(r)
	if err := r.Err(); err != nil {
		t.Fatal(err)
	}
	var out []sub
	for _, topic := range []string{domainevent.TopicEventCancelled, domainevent.TopicOfferingSubmitted,
		domainevent.TopicOfferingDecided, domainevent.TopicOfferingCancelled} {
		for _, s := range r.For(topic) {
			out = append(out, sub{topic: topic, h: s.Handle})
		}
	}
	return out
}

func countTemplate(ms []notify.Message, tmpl string, user uuid.UUID) int {
	n := 0
	for _, m := range ms {
		if m.Template == tmpl && m.UserID != nil && *m.UserID == user {
			n++
		}
	}
	return n
}

// TestInvoiceTerms_OfferingServiceAndFrozenPolicy: billing reads the
// offering's invoice trigger, the service's invoice codes and the frozen
// policy's automation, whatever their status (BE-042).
func TestInvoiceTerms_OfferingServiceAndFrozenPolicy(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	v := e.offering(t, "club_admin", "club_a", nil)
	p := e.policy(t, catalog.ScopeDefault, nil, 1000, t0.Add(-time.Hour), nil)
	got, err := e.mod.Offerings.InvoiceTerms(t.Context(), v.Offering.ID, p.ID)
	if err != nil || got.OfferingID != v.Offering.ID || got.Trigger != v.Offering.InvoiceTrigger || !got.Trigger.Valid() ||
		got.Automation != p.InvoiceAutomation {
		t.Fatalf("InvoiceTerms = %+v, %v", got, err)
	}
	if _, err := e.mod.Offerings.InvoiceTerms(t.Context(), uuid.NewV7(), p.ID); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("unknown offering: %v", err)
	}
	if _, err := e.mod.Offerings.InvoiceTerms(t.Context(), v.Offering.ID, uuid.NewV7()); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("unknown policy: %v", err)
	}
}
