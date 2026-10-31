package authz_test

import (
	"maps"
	"testing"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
)

// statsOrg is the Resource the stats service builds for an org panel
// (03 §3.5; H-27): the path org's TypeOrg resource, Attrs sensitive for a
// club.
func statsOrg(name string, kind authz.OrgKind, sensitive bool) authz.Resource {
	r := authz.OrgResource(authztest.ID(name), string(kind), "active")
	if sensitive {
		r.Attrs[authz.AttrSensitive] = "true"
	}
	return r
}

// TestStatsPolicy_Matrix pins 03 §5.23 (BE-163) for the 18 personas: the
// platform panel to platform_admin only (403 for everyone else, the admin
// surface); each org panel to its role on the org's surface plus
// platform_admin (other platform roles 403), members without the role 403,
// everyone else 404; a sensitive club's panel to its club_admin only
// (staff 404, Y2).
func TestStatsPolicy_Matrix(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	forbidden, notFound := authz.ErrForbidden, authz.ErrNotFound
	staff := map[string]error{
		"anonymous": forbidden, "moderator": forbidden, "support": forbidden, "dpo": forbidden,
		"platform_admin": nil, "finance_admin": forbidden, "talent_auditor": forbidden,
	}
	with := func(base, extra map[string]error) map[string]error {
		out := maps.Clone(base)
		maps.Copy(out, extra)
		return out
	}
	for _, tc := range []struct {
		name string
		act  authz.Action
		res  authz.Resource
		def  error // everyone not listed
		want map[string]error
	}{
		{"platform", authz.StatsPlatformRead, authz.Resource{Type: authz.TypeAdminSurface}, forbidden,
			map[string]error{"platform_admin": nil}},
		{"club_a", authz.StatsClubRead, statsOrg("club_a", authz.OrgClub, false), notFound,
			with(staff, map[string]error{"club_admin": nil, "club_member": forbidden})},
		{"club_b", authz.StatsClubRead, statsOrg("club_b", authz.OrgClub, false), notFound,
			with(staff, map[string]error{"other_club_admin": nil})},
		{"club_sensitive", authz.StatsClubRead, statsOrg("club_sensitive", authz.OrgClub, true), notFound,
			map[string]error{"anonymous": forbidden, "club_member": forbidden}},
		{"company_a", authz.StatsCompanyRead, statsOrg("company_a", authz.OrgCompany, false), notFound,
			with(staff, map[string]error{"company_member": nil, "company_admin": nil})},
		{"company_b", authz.StatsCompanyRead, statsOrg("company_b", authz.OrgCompany, false), notFound,
			with(staff, map[string]error{"other_company_member": nil})},
		{"uni_a", authz.StatsUniversityRead, statsOrg("uni_a", authz.OrgUniversity, false), notFound,
			with(staff, map[string]error{"university_rep": nil})},
		{"uni_b", authz.StatsUniversityRead, statsOrg("uni_b", authz.OrgUniversity, false), notFound, staff},
		// The club panel's action on a company the caller administers: a member without the role.
		{"club action on company_a", authz.StatsClubRead, statsOrg("company_a", authz.OrgCompany, false), notFound,
			with(staff, map[string]error{"company_member": forbidden, "company_admin": forbidden})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, p := range authztest.Personas() {
				want, ok := tc.want[p.Name]
				if !ok {
					want = tc.def
				}
				wantErr(t, authz.Can(ctx, p.Actor, tc.act, tc.res), want, false)
			}
		})
	}
	// The sensitive club's own club_admin (a helper user) reads its panel.
	p, _ := authztest.Get("club_admin_sensitive")
	wantErr(t, authz.Can(ctx, p.Actor, authz.StatsClubRead, statsOrg("club_sensitive", authz.OrgClub, true)), nil, false)
}

// TestStatsPolicy_CareerStaffReadsUniversityPanel: career_staff holds the
// university_rep cells except k-cs (members, invitations, offering
// approval); the panel is not one of them, and needs no MFA.
func TestStatsPolicy_CareerStaffReadsUniversityPanel(t *testing.T) {
	t.Parallel()
	uni := authztest.ID("uni_a")
	a := authz.Actor{Kind: authz.KindUser, UserID: authztest.ID("career_staff_a"),
		Memberships: []authz.Membership{{OrgID: uni, OrgKind: authz.OrgUniversity, Role: authz.MemberCareerStaff}}}
	wantErr(t, authz.Can(t.Context(), a, authz.StatsUniversityRead, statsOrg("uni_a", authz.OrgUniversity, false)), nil, false)
	wantErr(t, authz.Can(t.Context(), a, authz.StatsCompanyRead, statsOrg("uni_a", authz.OrgUniversity, false)), authz.ErrForbidden, false)
}
