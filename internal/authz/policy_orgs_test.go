package authz_test

import (
	"strconv"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
)

// orgRes is the org resource the orgs service builds (03 §3.5; 06 H-27).
func orgRes(name string, kind authz.OrgKind, status string) authz.Resource {
	id := authztest.ID(name)
	return authz.Resource{Type: authz.TypeOrg, ID: id, OrgID: &id, Attrs: map[string]string{
		"org_kind": string(kind), "org_status": status,
	}}
}

var newCompany = authz.Resource{Type: authz.TypeOrg, Attrs: map[string]string{"org_kind": "company", "org_status": "pending_review"}}

// TestOrgsPolicy_Matrix pins 03 §5.2 (BE-017's 9 actions) for the 18
// personas; testdata/authz/orgs.yaml checks the same through the API.
func TestOrgsPolicy_Matrix(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fb, nf := authz.ErrForbidden, authz.ErrNotFound
	staff := []string{"moderator", "support", "dpo", "platform_admin", "finance_admin", "talent_auditor"}
	// company_a's management surface: members 403 unless they hold the
	// role, platform staff 403 unless platform_admin, everyone else 404.
	surface := func(allowed ...string) map[string]error {
		m := map[string]error{"anonymous": fb, "company_member": fb, "company_admin": fb}
		for _, s := range staff {
			m[s] = fb
		}
		for _, a := range allowed {
			m[a] = nil
		}
		return m
	}
	for _, tc := range []struct {
		name    string
		act     authz.Action
		res     authz.Resource
		want    map[string]error
		missing error
	}{
		{"read active", authz.OrgsOrgRead, orgRes("company_a", authz.OrgCompany, "active"), nil, nil},
		{"read suspended", authz.OrgsOrgRead, orgRes("company_a", authz.OrgCompany, "suspended"), nil, nil},
		{"read pending (no member persona)", authz.OrgsOrgRead, orgRes("company_x", authz.OrgCompany, "pending_review"),
			map[string]error{"moderator": nil, "support": nil, "platform_admin": nil}, nf},
		{"read closed, company_a members", authz.OrgsOrgRead, orgRes("company_a", authz.OrgCompany, "closed"),
			map[string]error{"company_member": nil, "company_admin": nil, "moderator": nil, "support": nil, "platform_admin": nil}, nf},
		{"search", authz.OrgsOrgSearch, authz.Resource{Type: authz.TypeOrgCatalog}, nil, nil},
		{"company.create", authz.OrgsCompanyCreate, newCompany,
			map[string]error{"anonymous": fb, "student_minor": fb, "moderator": fb, "support": fb, "dpo": fb,
				"platform_admin": fb, "finance_admin": fb, "talent_auditor": fb}, nil},
		{"update company_a", authz.OrgsOrgUpdate, orgRes("company_a", authz.OrgCompany, "active"),
			surface("company_admin", "platform_admin"), nf},
		{"update club_a", authz.OrgsOrgUpdate, orgRes("club_a", authz.OrgClub, "active"),
			func() map[string]error {
				m := surface("club_admin", "platform_admin")
				delete(m, "company_member")
				delete(m, "company_admin")
				m["club_member"] = fb
				return m
			}(), nf},
		{"update uni_a", authz.OrgsOrgUpdate, orgRes("uni_a", authz.OrgUniversity, "active"),
			func() map[string]error {
				m := surface("university_rep", "platform_admin")
				delete(m, "company_member")
				delete(m, "company_admin")
				return m
			}(), nf},
		{"close company_a", authz.OrgsOrgClose, orgRes("company_a", authz.OrgCompany, "active"), surface("platform_admin"), nf},
		{"transfer company_a", authz.OrgsOwnershipTransfer, orgRes("company_a", authz.OrgCompany, "active"), surface("platform_admin"), nf},
		{"list_admin", authz.OrgsOrgListAdmin, adminSurface,
			map[string]error{"moderator": nil, "support": nil, "platform_admin": nil}, fb},
		{"verify", authz.OrgsOrgVerify, adminSurface, map[string]error{"platform_admin": nil}, fb},
		{"suspend", authz.OrgsOrgSuspend, adminSurface, map[string]error{"platform_admin": nil}, fb},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, p := range authztest.Personas() {
				want, ok := tc.want[p.Name]
				if !ok {
					want = tc.missing
				}
				t.Run(p.Name, func(t *testing.T) {
					wantErr(t, authz.Can(ctx, p.Actor, tc.act, tc.res), want, false)
				})
			}
		})
	}
}

func TestOrgsPolicy_EdgeCases(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	co := orgRes("company_a", authz.OrgCompany, "active")

	// k-own: the owner closes and transfers; MFA comes with the role path.
	owner := persona(t, "company_owner_a")
	for _, act := range []authz.Action{authz.OrgsOrgClose, authz.OrgsOwnershipTransfer, authz.OrgsOrgUpdate} {
		wantErr(t, authz.Can(ctx, owner, act, co), nil, false)
	}
	noMFA := owner
	noMFA.MFAVerified = false
	wantErr(t, authz.Can(ctx, noMFA, authz.OrgsOrgClose, co), authz.ErrMFARequired, false)
	// career_staff is not university_rep for the profile (k-cs).
	uni := orgRes("uni_a", authz.OrgUniversity, "active")
	cs := authz.Actor{Kind: authz.KindUser, UserID: uuid.NewV7(), MFAVerified: true,
		Memberships: []authz.Membership{{OrgID: authztest.ID("uni_a"), OrgKind: authz.OrgUniversity, Role: authz.MemberCareerStaff}}}
	wantErr(t, authz.Can(ctx, cs, authz.OrgsOrgUpdate, uni), authz.ErrForbidden, false)
	// A club_admin who is a minor is no club_admin (Y4).
	minorAdmin := persona(t, "club_admin")
	minorAdmin.IsMinor = true
	wantErr(t, authz.Can(ctx, minorAdmin, authz.OrgsOrgUpdate, orgRes("club_a", authz.OrgClub, "active")), authz.ErrForbidden, false)
	// A minor never registers a company, whatever membership they hold (k-cmp).
	wantErr(t, authz.Can(ctx, minorAdmin, authz.OrgsCompanyCreate, newCompany), authz.ErrForbidden, false)
	// Missing Attrs are a programmer error (deny).
	wantErr(t, authz.Can(ctx, owner, authz.OrgsOrgUpdate, authz.Resource{Type: authz.TypeOrg, OrgID: co.OrgID}), authz.ErrForbidden, true)
	// An unknown status hides the profile (safe default).
	wantErr(t, authz.Can(ctx, authz.Anonymous(), authz.OrgsOrgRead, orgRes("company_a", authz.OrgCompany, "")), authz.ErrNotFound, false)
	// A member list without the sensitive key is a programmer error (deny).
	wantErr(t, authz.Can(ctx, persona(t, "club_admin"), authz.OrgsMemberRead,
		authz.Resource{Type: authz.TypeOrgMember, OrgID: ptr(authztest.ID("club_a")), Attrs: map[string]string{
			"org_kind": "club", "role": "member", "status": "active"}}), authz.ErrForbidden, true)
}

// surfaceRes is the org collection resource of the member list and the
// invitations (an org resource with the club's sensitivity, 03 k4).
func surfaceRes(name string, kind authz.OrgKind, sensitive bool, reason string) authz.Resource {
	r := orgRes(name, kind, "active")
	r.Attrs["sensitive"] = strconv.FormatBool(sensitive)
	if reason != "" {
		r.Attrs["access_reason"] = reason
	}
	return r
}

// memberRes is the org_member resource of the persona's membership in org.
func memberRes(owner, org string, kind authz.OrgKind, role authz.MemberRole, sensitive bool, reason string) authz.Resource {
	o := authztest.ID(org)
	r := authz.Resource{Type: authz.TypeOrgMember, ID: authztest.ID(owner + "_in_" + org), OrgID: &o,
		OwnerUserID: ptr(authztest.ID(owner)), Attrs: map[string]string{
			"org_kind": string(kind), "role": string(role), "status": "active", "sensitive": strconv.FormatBool(sensitive),
		}}
	if reason != "" {
		r.Attrs["access_reason"] = reason
	}
	return r
}

func inviteRes(org string, kind authz.OrgKind, role authz.MemberRole) authz.Resource {
	o := authztest.ID(org)
	return authz.Resource{Type: authz.TypeOrgInvitation, OrgID: &o, Attrs: map[string]string{"org_kind": string(kind), "role": string(role)}}
}

// TestOrgsPolicy_Memberships pins 03 §5.2 (BE-019's 12 actions, k4, k-cs,
// Y2) for the 18 personas; testdata/authz/orgs.yaml checks the same
// through the API.
func TestOrgsPolicy_Memberships(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fb, nf, bg := authz.ErrForbidden, authz.ErrNotFound, authz.ErrBreakGlassReason
	const why = "Destek talebi 4711 inceleniyor"
	staff := []string{"moderator", "support", "dpo", "platform_admin", "finance_admin", "talent_auditor"}
	students := []string{"student", "student_minor", "club_member", "club_admin", "other_club_admin", "event_staff"}
	// club_a's management surface: non-members 404; members and staff 403
	// unless allowed.
	clubA := func(allowed map[string]error) map[string]error {
		m := map[string]error{"anonymous": fb, "club_member": fb, "club_admin": fb}
		for _, s := range staff {
			m[s] = fb
		}
		for k, v := range allowed {
			m[k] = v
		}
		return m
	}
	companyA := func(allowed map[string]error) map[string]error {
		m := map[string]error{"anonymous": fb, "company_member": fb, "company_admin": fb}
		for _, s := range staff {
			m[s] = fb
		}
		for k, v := range allowed {
			m[k] = v
		}
		return m
	}
	clubMember := memberRes("club_member", "club_a", authz.OrgClub, authz.MemberMember, false, "")
	for _, tc := range []struct {
		name    string
		act     authz.Action
		res     authz.Resource
		want    map[string]error
		missing error
	}{
		{"list club_a", authz.OrgsMemberList, surfaceRes("club_a", authz.OrgClub, false, ""),
			clubA(map[string]error{"club_admin": nil, "support": bg, "platform_admin": bg}), nf},
		{"list club_a break-glass", authz.OrgsMemberList, surfaceRes("club_a", authz.OrgClub, false, why),
			clubA(map[string]error{"club_admin": nil, "support": nil, "platform_admin": nil}), nf},
		{"list sensitive club (Y2)", authz.OrgsMemberList, surfaceRes("club_sensitive", authz.OrgClub, true, why),
			map[string]error{"anonymous": fb, "club_member": fb}, nf},
		{"list company_a", authz.OrgsMemberList, surfaceRes("company_a", authz.OrgCompany, false, ""),
			companyA(map[string]error{"company_member": nil, "company_admin": nil, "support": bg, "platform_admin": bg}), nf},
		{"list uni_a", authz.OrgsMemberList, surfaceRes("uni_a", authz.OrgUniversity, false, ""),
			func() map[string]error {
				m := clubA(map[string]error{"university_rep": nil, "support": bg, "platform_admin": bg})
				delete(m, "club_member")
				delete(m, "club_admin")
				return m
			}(), nf},
		{"read club_a member", authz.OrgsMemberRead, clubMember,
			clubA(map[string]error{"club_admin": nil, "support": bg, "platform_admin": bg}), nf},
		{"read sensitive membership (Y2)", authz.OrgsMemberRead,
			memberRes("club_member", "club_sensitive", authz.OrgClub, authz.MemberMember, true, why),
			map[string]error{"anonymous": fb, "club_member": fb}, nf},
		{"decide club_a", authz.OrgsMemberDecide, clubMember, clubA(map[string]error{"club_admin": nil}), nf},
		{"remove club_a", authz.OrgsMemberRemove, clubMember, clubA(map[string]error{"club_admin": nil, "platform_admin": nil}), nf},
		{"remove company_a", authz.OrgsMemberRemove,
			memberRes("company_member", "company_a", authz.OrgCompany, authz.MemberRecruiter, false, ""),
			companyA(map[string]error{"company_admin": nil, "platform_admin": nil}), nf},
		{"change_role club_a", authz.OrgsMemberChangeRole, clubMember,
			clubA(map[string]error{"club_admin": nil, "platform_admin": nil}), nf},
		{"change_role company_a", authz.OrgsMemberChangeRole,
			memberRes("company_member", "company_a", authz.OrgCompany, authz.MemberRecruiter, false, ""),
			companyA(map[string]error{"company_admin": nil, "platform_admin": nil}), nf},
		{"invite club_a", authz.OrgsInvitationCreate, inviteRes("club_a", authz.OrgClub, authz.MemberMember),
			clubA(map[string]error{"club_admin": nil, "platform_admin": nil}), nf},
		{"invite uni_a (platform_admin only)", authz.OrgsInvitationCreate,
			inviteRes("uni_a", authz.OrgUniversity, authz.MemberCareerStaff),
			func() map[string]error {
				m := clubA(map[string]error{"university_rep": fb, "platform_admin": nil})
				delete(m, "club_member")
				delete(m, "club_admin")
				return m
			}(), nf},
		{"list invitations company_a", authz.OrgsInvitationList, orgRes("company_a", authz.OrgCompany, "active"),
			companyA(map[string]error{"company_admin": nil, "platform_admin": nil}), nf},
		{"revoke invitation club_a", authz.OrgsInvitationRevoke, inviteRes("club_a", authz.OrgClub, authz.MemberMember),
			clubA(map[string]error{"club_admin": nil, "platform_admin": nil}), nf},
		{"leave club_member's membership", authz.OrgsMembershipLeave, clubMember,
			map[string]error{"anonymous": fb, "club_member": nil}, nf},
		{"join a club", authz.OrgsMemberJoin, authz.Resource{Type: authz.TypeClub, ID: authztest.ID("club_unclaimed"),
			OrgID: ptr(authztest.ID("club_unclaimed")), Attrs: map[string]string{"sensitive": "false",
				"university_id": authztest.UniversityID("uni_a").String()}},
			func() map[string]error {
				m := map[string]error{}
				for _, s := range students {
					m[s] = nil
				}
				return m
			}(), fb},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, p := range authztest.Personas() {
				want, ok := tc.want[p.Name]
				if !ok {
					want = tc.missing
				}
				t.Run(p.Name, func(t *testing.T) {
					wantErr(t, authz.Can(ctx, p.Actor, tc.act, tc.res), want, false)
				})
			}
		})
	}

	// O: list_own and accept are the caller's own resources.
	for _, p := range authztest.Personas() {
		if !p.Actor.IsUser() {
			continue
		}
		me := p.User()
		own := authz.Resource{Type: authz.TypeUser, ID: me, OwnerUserID: &me}
		wantErr(t, authz.Can(ctx, p.Actor, authz.OrgsMembershipListOwn, own), nil, false)
		inv := inviteRes("club_a", authz.OrgClub, authz.MemberMember)
		wantErr(t, authz.Can(ctx, p.Actor, authz.OrgsInvitationAccept, inv), nf, false) // not addressed to the caller
		inv.OwnerUserID = &me
		wantErr(t, authz.Can(ctx, p.Actor, authz.OrgsInvitationAccept, inv), nil, false)
	}
	// The sensitive club's own club_admin reads the list and a membership;
	// a minor club_admin is no club_admin (Y4).
	sensAdmin := persona(t, "club_admin_sensitive")
	wantErr(t, authz.Can(ctx, sensAdmin, authz.OrgsMemberList, surfaceRes("club_sensitive", authz.OrgClub, true, "")), nil, false)
	sensMember := memberRes("club_member", "club_sensitive", authz.OrgClub, authz.MemberMember, true, "")
	wantErr(t, authz.Can(ctx, sensAdmin, authz.OrgsMemberRead, sensMember), nil, false)
	minor := sensAdmin
	minor.IsMinor = true
	wantErr(t, authz.Can(ctx, minor, authz.OrgsMemberRead, sensMember), nf, false)
	// career_staff is not university_rep for removals (k-cs).
	cs := authz.Actor{Kind: authz.KindUser, UserID: uuid.NewV7(),
		Memberships: []authz.Membership{{OrgID: authztest.ID("uni_a"), OrgKind: authz.OrgUniversity, Role: authz.MemberCareerStaff}}}
	wantErr(t, authz.Can(ctx, cs, authz.OrgsMemberRemove,
		memberRes("university_rep", "uni_a", authz.OrgUniversity, authz.MemberUniversityRep, false, "")), fb, false)
	// The system action is the workers' only.
	wantErr(t, authz.Can(ctx, authz.SystemActor("orgs.admin_sweep"), authz.OrgsMemberEndRole, authz.Resource{Type: authz.TypeOrgMember}), nil, false)
	wantErr(t, authz.Can(ctx, persona(t, "platform_admin"), authz.OrgsMemberEndRole, clubMember), fb, false)
}
