package authz_test

import (
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
	// BE-019's actions stay denied until written.
	wantErr(t, authz.Can(ctx, persona(t, "club_admin"), authz.OrgsMemberList, orgRes("club_a", authz.OrgClub, "active")), authz.ErrForbidden, false)
}
