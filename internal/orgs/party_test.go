package orgs_test

import (
	"testing"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/notify"
	"koro/internal/orgs"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/problem/problemtest"
)

// partyRoles are the company roles; any of them makes a platform admin a
// party to the company's decisions.
var partyRoles = []authz.MemberRole{authz.MemberRecruiter, authz.MemberCompanyAdmin, authz.MemberCompanyOwner}

// wantPartyDenied checks that the denied records of act on the org id with
// metadata key = value are want party refusals: reason org_member, scoped to
// the org (it was read, 06 H-38), the decision named and nothing else.
func wantPartyDenied(t *testing.T, e *env, act authz.Action, id uuid.UUID, key, value string, want int) {
	t.Helper()
	var n int
	for _, d := range e.audit.OfOutcome(act, audit.Denied) {
		if d.ResourceID == nil || *d.ResourceID != id || d.Metadata[key] != value {
			continue
		}
		if d.Reason != "org_member" || d.OrgID == nil || *d.OrgID != id || d.ResourceType != authz.TypeOrg || len(d.Metadata) != 1 {
			t.Fatalf("%s denied record = %+v", act, d)
		}
		n++
	}
	if n != want {
		t.Fatalf("%d denied %s records with %s=%s, want %d", n, act, key, value, want)
	}
}

// TestOrgReview_PartyCannotDecide (BE-212, Q-65): a platform admin who is a
// member of a pending_review company (any role) neither verifies its
// registration (adminVerifyOrg) nor rejects it (closeOrg as staff): 403
// before the proof, recorded as denied (orgs.org.verify, org_member, the
// decision named), and the company still awaits review. Since BE-214 the
// org surface refuses the close first (orgs.org.close: a member closes only
// as the owner), recorded as that action's party refusal. A platform admin
// of another company, or of none, decides; a platform admin who owns the
// pending company withdraws it (the owner's close is no rejection).
func TestOrgReview_PartyCannotDecide(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	v, _ := e.company(t, "Taraf Başvurusu")
	id := v.Org.ID
	other, _ := e.company(t, "Başka Başvuru")

	for _, role := range partyRoles {
		party := as(pad(t), id, orgs.KindCompany, role)
		calls := e.reauth.calls
		_, err := e.svc.AdminVerifyOrg(ctx, party, id, "Kendi şirketim")
		problemtest.WantErr(t, err, authz.ErrForbidden)
		if role != authz.MemberCompanyOwner { // the owner closing it withdraws it (below)
			_, err = e.svc.CloseOrg(ctx, party, id, "Kendi başvurum", proof(goodPassword))
			problemtest.WantErr(t, err, authz.ErrForbidden)
		}
		if e.reauth.calls != calls {
			t.Fatalf("%s: a proof was checked for a party", role)
		}
	}
	wantPartyDenied(t, e, authz.OrgsOrgVerify, id, "transition", "verify", 3)
	wantSurfaceDenied(t, e, authz.OrgsOrgClose, authz.TypeOrg, id, id, nil, 2) // BE-214
	cur, err := e.store.GetOrg(ctx, id)
	if err != nil || cur.Status != orgs.StatusPendingReview || cur.VerifiedAt != nil || cur.RejectionReason != nil {
		t.Fatalf("the party decided: %+v, %v", cur, err)
	}
	if n := len(e.audit.OfOutcome(authz.OrgsOrgVerify, audit.Success)) + len(e.audit.OfOutcome(authz.OrgsOrgClose, audit.Success)); n != 0 {
		t.Fatalf("%d decision records after the party's attempts", n)
	}
	if evs := eventsOf[domainevent.OrgReviewDecided](t, e, domainevent.TopicOrgReviewDecided); len(evs) != 0 {
		t.Fatalf("org.review_decided after the party's attempts: %+v", evs)
	}

	// A platform admin of another company verifies; one of none rejects.
	elsewhere := as(pad(t), other.Org.ID, orgs.KindCompany, authz.MemberCompanyAdmin)
	got, err := e.svc.AdminVerifyOrg(ctx, elsewhere, id, "")
	if err != nil || got.Org.Status != orgs.StatusActive {
		t.Fatalf("verification by a platform admin of another company = %+v, %v", got.Org, err)
	}
	v2, _ := e.company(t, "Reddedilen Başvuru")
	rejected, err := e.svc.CloseOrg(ctx, pad(t), v2.Org.ID, "Belgeler eksik", proof(goodPassword))
	if err != nil || rejected.Org.Status != orgs.StatusClosed {
		t.Fatalf("rejection by a platform admin who is no party = %+v, %v", rejected.Org, err)
	}
	// A platform admin who owns a pending company withdraws it: no rejection.
	v3, _ := e.company(t, "Geri Çekilen Başvuru")
	owner := as(pad(t), v3.Org.ID, orgs.KindCompany, authz.MemberCompanyOwner)
	if _, err := e.svc.CloseOrg(ctx, owner, v3.Org.ID, "Vazgeçtik", proof(goodPassword)); err != nil {
		t.Fatalf("withdrawal by the owner who is a platform admin: %v", err)
	}
	evs := eventsOf[domainevent.OrgReviewDecided](t, e, domainevent.TopicOrgReviewDecided)
	if len(evs) != 2 || evs[0].OrgID != id || evs[0].Outcome != domainevent.OrgReviewVerified ||
		evs[1].OrgID != v2.Org.ID || evs[1].Outcome != domainevent.OrgReviewRejected {
		t.Fatalf("org.review_decided = %+v", evs)
	}
}

// TestOrgSuspension_PartyCannotDecide (BE-212, Q-65 and its suspension
// reading): a platform admin who is a member of a company (any role) neither
// suspends nor reactivates it: 403 before the proof and the status (a
// missing proof, a reactivation of the active company are 403 too),
// recorded as denied (orgs.org.suspend, org_member, the transition named).
// Another platform admin decides both; a missing org is 404 before the
// proof.
func TestOrgSuspension_PartyCannotDecide(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	v, _ := e.company(t, "Taraf Şirketi")
	id := v.Org.ID
	if _, err := e.svc.AdminVerifyOrg(ctx, pad(t), id, ""); err != nil {
		t.Fatal(err)
	}
	other, _ := e.company(t, "Başka Şirket")

	for _, role := range partyRoles {
		party := as(pad(t), id, orgs.KindCompany, role)
		calls := e.reauth.calls
		_, err := e.svc.AdminSuspendOrg(ctx, party, id, "Kendi şirketim", proof(goodPassword))
		problemtest.WantErr(t, err, authz.ErrForbidden)
		_, err = e.svc.AdminSuspendOrg(ctx, party, id, "Kendi şirketim", proof(""))
		problemtest.WantErr(t, err, authz.ErrForbidden)
		_, err = e.svc.AdminReactivateOrg(ctx, party, id, "Kendi şirketim", proof(goodPassword))
		problemtest.WantErr(t, err, authz.ErrForbidden)
		if e.reauth.calls != calls {
			t.Fatalf("%s: a proof was checked for a party", role)
		}
	}
	cur, err := e.store.GetOrg(ctx, id)
	if err != nil || cur.Status != orgs.StatusActive {
		t.Fatalf("the party suspended it: %+v, %v", cur, err)
	}
	if n := len(e.audit.OfOutcome(authz.OrgsOrgSuspend, audit.Success)); n != 0 {
		t.Fatalf("%d suspension records after the party's attempts", n)
	}

	// Another platform admin suspends; the party still cannot reactivate.
	if _, err := e.svc.AdminSuspendOrg(ctx, pad(t), id, "Şikâyet incelemesi", proof(goodPassword)); err != nil {
		t.Fatalf("suspension by a platform admin who is no party: %v", err)
	}
	party := as(pad(t), id, orgs.KindCompany, authz.MemberCompanyAdmin)
	_, err = e.svc.AdminReactivateOrg(ctx, party, id, "Kendi şirketim", proof(goodPassword))
	problemtest.WantErr(t, err, authz.ErrForbidden)
	wantPartyDenied(t, e, authz.OrgsOrgSuspend, id, "transition", "suspend", 6)
	wantPartyDenied(t, e, authz.OrgsOrgSuspend, id, "transition", "reactivate", 4)
	elsewhere := as(pad(t), other.Org.ID, orgs.KindCompany, authz.MemberCompanyOwner)
	got, err := e.svc.AdminReactivateOrg(ctx, elsewhere, id, "İnceleme bitti", proof(goodPassword))
	if err != nil || got.Org.Status != orgs.StatusActive {
		t.Fatalf("reactivation by a platform admin of another company = %+v, %v", got.Org, err)
	}

	// The org is read before the proof: a missing one is 404 without one.
	calls := e.reauth.calls
	_, err = e.svc.AdminSuspendOrg(ctx, pad(t), uuid.NewV7(), "Şikâyet incelemesi", proof(""))
	problemtest.WantErr(t, err, orgs.ErrNotFound)
	if e.reauth.calls != calls {
		t.Fatal("a proof was checked for a missing org")
	}
}

// TestOrgReview_PartyStillSeesTheQueue (BE-212; BE-208's choice for the tax
// identifier review): the party keeps seeing the pending company in the
// review queue (adminListOrgs?status=pending_review) and on its detail, and
// is told of it like every platform admin; only the decision is refused.
func TestOrgReview_PartyStillSeesTheQueue(t *testing.T) {
	t.Parallel()
	partyID := persona(t, "platform_admin").UserID
	e := reviewEnv(t, partyID)
	ctx := t.Context()
	v, _ := e.company(t, "Görünen Başvuru")
	party := as(pad(t), v.Org.ID, orgs.KindCompany, authz.MemberCompanyAdmin)
	page, err := e.svc.AdminListOrgs(ctx, party, orgs.AdminQuery{Status: orgs.StatusPendingReview})
	if err != nil || len(page.Items) != 1 || page.Items[0].Org.ID != v.Org.ID {
		t.Fatalf("the party's queue = %+v, %v", page.Items, err)
	}
	if _, err := e.svc.GetOrg(ctx, party, v.Org.ID); err != nil {
		t.Fatalf("the party's detail: %v", err)
	}
	e.deliver(t, domainevent.TopicOrgReviewRequested)
	got := e.sentOf("orgs.review_requested", notify.ChannelInApp)
	if len(got) != 1 || got[0].UserID == nil || *got[0].UserID != partyID {
		t.Fatalf("review notices = %+v", got)
	}
	_, err = e.svc.AdminVerifyOrg(ctx, party, v.Org.ID, "")
	problemtest.WantErr(t, err, authz.ErrForbidden)
}
