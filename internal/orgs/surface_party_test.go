package orgs_test

import (
	"maps"
	"testing"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/orgs"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/problem/problemtest"
)

// staffMember gives the platform_admin persona's user an active membership of
// org with role and returns the platform admin holding it.
func (e *env) staffMember(t *testing.T, org uuid.UUID, role authz.MemberRole) authz.Actor {
	t.Helper()
	a := pad(t)
	now := e.clk.Now()
	m, err := e.store.InsertMember(t.Context(), orgs.Membership{
		ID: uuid.NewV7(), OrgID: org, UserID: a.UserID, Role: role, Status: orgs.MemberActive, Source: orgs.SourceInvitation,
		JoinedAt: &now,
	})
	if err != nil {
		t.Fatalf("InsertMember: %v", err)
	}
	return as(a, org, m.OrgKind, role)
}

// activeCompany registers a company and verifies it (a platform admin of no
// company does).
func (e *env) activeCompany(t *testing.T, name string) (orgs.OrgView, authz.Actor) {
	t.Helper()
	v, owner := e.company(t, name)
	got, err := e.svc.AdminVerifyOrg(t.Context(), pad(t), v.Org.ID, "")
	if err != nil {
		t.Fatalf("AdminVerifyOrg: %v", err)
	}
	return got, owner
}

// wantSurfaceDenied checks that act has exactly want denied records of the
// party rule on the resource typ/id of org: reason org_member, the org's
// scope, metadata meta (nil: none).
func wantSurfaceDenied(t *testing.T, e *env, act authz.Action, typ string, id, org uuid.UUID, meta map[string]any, want int) {
	t.Helper()
	var n int
	for _, d := range e.audit.OfOutcome(act, audit.Denied) {
		if d.Reason != "org_member" {
			continue
		}
		if d.ResourceType != typ || d.ResourceID == nil || *d.ResourceID != id || d.OrgID == nil || *d.OrgID != org ||
			len(d.Metadata) != len(meta) || meta != nil && !maps.Equal(d.Metadata, meta) {
			t.Fatalf("%s denied record = %+v", act, d)
		}
		n++
	}
	if n != want {
		t.Fatalf("%d party denied records of %s, want %d", n, act, want)
	}
}

// TestOrgSurface_PartyCannotTakeOver (BE-214, Q-69; T-438): a platform admin
// who is a company's company_admin transfers its ownership neither to
// themselves nor to anyone and does not close it: 403 before any proof,
// recorded as denied (org_member; the transfer names its target). As its
// recruiter they change nothing on its surface (profile, members, roles,
// invitations), each refusal recorded with its resource. As company_admin
// they still do what a company_admin does. A platform admin of no company,
// or one who owns it, transfers and closes; a recruiter without a platform
// role is refused without a party record.
func TestOrgSurface_PartyCannotTakeOver(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	v, owner := e.activeCompany(t, "Ele Geçirilmeyen")
	id := v.Org.ID
	party := e.staffMember(t, id, authz.MemberCompanyAdmin)

	calls := e.reauth.calls
	_, err := e.svc.TransferOwnership(ctx, party, id, party.UserID, proof(goodPassword))
	problemtest.WantErr(t, err, authz.ErrForbidden)
	_, err = e.svc.CloseOrg(ctx, party, id, "Kendi şirketim", proof(goodPassword))
	problemtest.WantErr(t, err, authz.ErrForbidden)
	if e.reauth.calls != calls {
		t.Fatal("a proof was checked for a party")
	}
	wantSurfaceDenied(t, e, authz.OrgsOwnershipTransfer, authz.TypeOrg, id, id,
		map[string]any{"new_owner_user_id": party.UserID.String()}, 1)
	wantSurfaceDenied(t, e, authz.OrgsOrgClose, authz.TypeOrg, id, id, nil, 1)
	if cur, err := e.store.LockOwner(ctx, id); err != nil || cur.UserID != owner.UserID {
		t.Fatalf("owner after the party's attempt = %+v, %v", cur, err)
	}
	if cur, err := e.store.GetOrg(ctx, id); err != nil || cur.Status != orgs.StatusActive {
		t.Fatalf("org after the party's attempt = %+v, %v", cur, err)
	}

	// As company_admin the party still removes a recruiter (its own right).
	gone := e.member(t, id, authz.MemberRecruiter)
	m, err := e.store.LockActiveMember(ctx, id, gone.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.RemoveMember(ctx, party, id, m.ID, ""); err != nil {
		t.Fatalf("removal by the party as company_admin: %v", err)
	}

	// As a recruiter the party changes nothing on the company's surface.
	e2 := newEnv(t)
	v2, owner2 := e2.activeCompany(t, "Taraf Recruiter")
	id2 := v2.Org.ID
	recruiter := e2.staffMember(t, id2, authz.MemberRecruiter)
	target := e2.member(t, id2, authz.MemberRecruiter)
	tm, err := e2.store.LockActiveMember(ctx, id2, target.UserID)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := e2.svc.CreateInvitation(ctx, owner2, id2, "yeni.uye@example.test", authz.MemberRecruiter)
	if err != nil {
		t.Fatal(err)
	}
	name := "Ele Geçirildi"
	_, err = e2.svc.UpdateOrg(ctx, recruiter, id2, v2.ETag, orgs.OrgPatch{Name: &name})
	problemtest.WantErr(t, err, authz.ErrForbidden)
	problemtest.WantErr(t, e2.svc.RemoveMember(ctx, recruiter, id2, tm.ID, ""), authz.ErrForbidden)
	_, err = e2.svc.ChangeMemberRole(ctx, recruiter, id2, tm.ID, authz.MemberCompanyAdmin)
	problemtest.WantErr(t, err, authz.ErrForbidden)
	_, err = e2.svc.CreateInvitation(ctx, recruiter, id2, "baska.uye@example.test", authz.MemberCompanyAdmin)
	problemtest.WantErr(t, err, authz.ErrForbidden)
	problemtest.WantErr(t, e2.svc.RevokeInvitation(ctx, recruiter, id2, inv.ID), authz.ErrForbidden)
	wantSurfaceDenied(t, e2, authz.OrgsOrgUpdate, authz.TypeOrg, id2, id2, nil, 1)
	wantSurfaceDenied(t, e2, authz.OrgsMemberRemove, authz.TypeOrgMember, tm.ID, id2, nil, 1)
	wantSurfaceDenied(t, e2, authz.OrgsMemberChangeRole, authz.TypeOrgMember, tm.ID, id2,
		map[string]any{"new_role": string(authz.MemberCompanyAdmin)}, 1)
	wantSurfaceDenied(t, e2, authz.OrgsInvitationCreate, authz.TypeOrg, id2, id2,
		map[string]any{"role": string(authz.MemberCompanyAdmin)}, 1)
	wantSurfaceDenied(t, e2, authz.OrgsInvitationRevoke, authz.TypeOrgInvitation, inv.ID, id2, nil, 1)
	if cur, err := e2.store.LockActiveMember(ctx, id2, target.UserID); err != nil || cur.Role != authz.MemberRecruiter {
		t.Fatalf("the target after the party's attempts = %+v, %v", cur, err)
	}
	// A recruiter without a platform role is refused too, by no party rule.
	_, err = e2.svc.UpdateOrg(ctx, target, id2, v2.ETag, orgs.OrgPatch{Name: &name})
	problemtest.WantErr(t, err, authz.ErrForbidden)
	wantSurfaceDenied(t, e2, authz.OrgsOrgUpdate, authz.TypeOrg, id2, id2, nil, 1)

	// A platform admin of no company transfers; one who owns it closes it.
	if _, err := e.svc.TransferOwnership(ctx, pad(t), id, party.UserID, proof(goodPassword)); err != nil {
		t.Fatalf("transfer by a platform admin who is no party: %v", err)
	}
	ownerPad := as(pad(t), id, orgs.KindCompany, authz.MemberCompanyOwner)
	if _, err := e.svc.CloseOrg(ctx, ownerPad, id, "Kapanıyoruz", proof(goodPassword)); err != nil {
		t.Fatalf("close by the owner who is a platform admin: %v", err)
	}
	if n := len(e.audit.OfOutcome(authz.OrgsOrgClose, audit.Denied)); n != 1 {
		t.Fatalf("%d denied close records, want the party's 1", n)
	}
}

// TestOrgSurface_PartyGetsNoSuspensionWaiver (BE-214, Q-69): a suspended
// company takes writes only from platform admins who are no member of it;
// its company_admin or owner who is a platform admin is refused like any
// member (403 orgs.org_suspended, no party record: it is the status rule).
func TestOrgSurface_PartyGetsNoSuspensionWaiver(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	v, _ := e.activeCompany(t, "Askıdaki Taraf")
	id := v.Org.ID
	party := e.staffMember(t, id, authz.MemberCompanyAdmin)
	s, err := e.svc.AdminSuspendOrg(ctx, pad(t), id, "Şikâyet incelemesi", proof(goodPassword))
	if err != nil {
		t.Fatal(err)
	}
	name := "Askıda Yazılan"
	_, err = e.svc.UpdateOrg(ctx, party, id, s.ETag, orgs.OrgPatch{Name: &name})
	problemtest.WantErr(t, err, orgs.ErrOrgSuspended)
	gone := e.member(t, id, authz.MemberRecruiter)
	m, err := e.store.LockActiveMember(ctx, id, gone.UserID)
	if err != nil {
		t.Fatal(err)
	}
	problemtest.WantErr(t, e.svc.RemoveMember(ctx, party, id, m.ID, ""), orgs.ErrOrgSuspended)
	_, err = e.svc.CreateInvitation(ctx, party, id, "askida@example.test", authz.MemberRecruiter)
	problemtest.WantErr(t, err, orgs.ErrOrgSuspended)
	_, err = e.svc.CloseOrg(ctx, as(pad(t), id, orgs.KindCompany, authz.MemberCompanyOwner), id, "Kapatalım", proof(goodPassword))
	problemtest.WantErr(t, err, orgs.ErrOrgSuspended)
	if n := len(e.audit.OfOutcome(authz.OrgsOrgUpdate, audit.Denied)); n != 0 {
		t.Fatalf("%d denied update records for the status rule", n)
	}
	// A platform admin of another company writes to the suspended one.
	other, _ := e.company(t, "Başka Şirket")
	elsewhere := as(pad(t), other.Org.ID, orgs.KindCompany, authz.MemberCompanyAdmin)
	got, err := e.svc.UpdateOrg(ctx, elsewhere, id, s.ETag, orgs.OrgPatch{Name: &name})
	if err != nil || got.Org.Name != name {
		t.Fatalf("update by a platform admin who is no party = %+v, %v", got.Org, err)
	}
}

// TestOrgSurface_PartyTaxIDChangeWaitsForReview (BE-214): a new tax
// identifier given by a platform admin who is the company's company_admin
// flags it for review like any member's (a platform admin's correction
// flags nothing only when they are no party), and the editor view masks the
// number for them.
func TestOrgSurface_PartyTaxIDChangeWaitsForReview(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	v, _ := e.activeCompany(t, "Vergi Tarafı")
	id := v.Org.ID
	party := e.staffMember(t, id, authz.MemberCompanyAdmin)
	requests := func() []domainevent.OrgReviewRequested {
		return eventsOf[domainevent.OrgReviewRequested](t, e, domainevent.TopicOrgReviewRequested)
	}
	before := len(requests()) // the registration's
	vkn := otherVKN(t)
	got, err := e.svc.UpdateOrg(ctx, party, id, v.ETag, orgs.OrgPatch{TaxNo: &vkn, Proof: proof(goodPassword)})
	if err != nil {
		t.Fatal(err)
	}
	if got.Company.TaxIDReviewRequestedAt == nil || got.FullTaxNo {
		t.Fatalf("the party's new identifier = %+v (full number %v)", got.Company, got.FullTaxNo)
	}
	if evs := requests(); len(evs) != before+1 || evs[len(evs)-1].OrgID != id {
		t.Fatalf("org.review_requested = %+v, want one more", evs)
	}
	if cur, err := e.svc.GetOrg(ctx, party, id); err != nil || !cur.Manage || cur.FullTaxNo {
		t.Fatalf("the party's editor view = manage %v, full number %v, %v", cur.Manage, cur.FullTaxNo, err)
	}
	if cur, err := e.svc.GetOrg(ctx, pad(t), id); err != nil || !cur.FullTaxNo {
		t.Fatalf("a platform admin of no company reads the number masked: %v", err)
	}
}

// TestOrgSurface_PlatformOrgNotLocked (BE-214): the platform org's members
// are the platform staff, so the rule leaves its surface to them: a platform
// admin who is its staff member edits its profile and removes another staff
// member, while being a party on a company they belong to.
func TestOrgSurface_PlatformOrgNotLocked(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	platform, _, err := e.store.InsertOrg(ctx, orgs.Org{ID: uuid.NewV7(), Kind: orgs.KindPlatform, Name: "Koro", Slug: "koro",
		Status: orgs.StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	staff := e.staffMember(t, platform.ID, authz.MemberStaff)
	cur, err := e.svc.GetOrg(ctx, staff, platform.ID)
	if err != nil {
		t.Fatal(err)
	}
	desc := "Koro ekibi"
	if _, err := e.svc.UpdateOrg(ctx, staff, platform.ID, cur.ETag, orgs.OrgPatch{Description: &desc}); err != nil {
		t.Fatalf("the platform org's staff member updating it: %v", err)
	}
	colleague := e.member(t, platform.ID, authz.MemberStaff)
	m, err := e.store.LockActiveMember(ctx, platform.ID, colleague.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.RemoveMember(ctx, staff, platform.ID, m.ID, ""); err != nil {
		t.Fatalf("the platform org's staff member removing a colleague: %v", err)
	}
	v, _ := e.activeCompany(t, "Personelin Şirketi")
	both := as(staff, v.Org.ID, orgs.KindCompany, authz.MemberRecruiter)
	_, err = e.svc.TransferOwnership(ctx, both, v.Org.ID, both.UserID, proof(goodPassword))
	problemtest.WantErr(t, err, authz.ErrForbidden)
}
