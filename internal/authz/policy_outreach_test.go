package authz_test

import (
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
)

// segmentsOf is the Resource the outreach service builds for every
// operation (03 §3.5 segment): OrgID = the path's org, no Attrs.
func segmentsOf(org string) authz.Resource {
	id := authztest.ID(org)
	return authz.Resource{Type: authz.TypeSegment, ID: id, OrgID: &id}
}

// dispatchesOf is the Resource of the dispatch operations (03 §3.5
// dispatch): OrgID = the path's org (the post's org for createDispatch).
func dispatchesOf(org string) authz.Resource {
	id := authztest.ID(org)
	return authz.Resource{Type: authz.TypeDispatch, ID: id, OrgID: &id}
}

// TestOutreachPolicy_Matrix pins 03 §5.8 for the 18 personas (the HTTP
// matrix, testdata/authz/outreach.yaml, checks the same through the API).
func TestOutreachPolicy_Matrix(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	forbidden, notFound := authz.ErrForbidden, authz.ErrNotFound
	staff := func(pad error) map[string]error {
		return map[string]error{
			"anonymous": forbidden, "moderator": forbidden, "support": forbidden, "dpo": forbidden,
			"platform_admin": pad, "finance_admin": forbidden, "talent_auditor": forbidden,
		}
	}
	with := func(m map[string]error, kv ...any) map[string]error {
		for i := 0; i+1 < len(kv); i += 2 {
			if kv[i+1] == nil {
				m[kv[i].(string)] = nil
				continue
			}
			m[kv[i].(string)] = kv[i+1].(error)
		}
		return m
	}
	// list, read, manage, dispatch.create: S cells; platform_admin k-po (not a member of these orgs → 403).
	for _, act := range []authz.Action{authz.OutreachSegmentList, authz.OutreachSegmentRead, authz.OutreachSegmentManage,
		authz.OutreachDispatchCreate} {
		res := segmentsOf
		if act == authz.OutreachDispatchCreate {
			res = dispatchesOf
		}
		for _, tc := range []struct {
			org  string
			want map[string]error
		}{
			{"company_a", with(staff(forbidden), "company_member", nil, "company_admin", nil)},
			{"company_b", with(staff(forbidden), "other_company_member", nil)},
			{"club_a", with(staff(forbidden), "club_admin", nil, "club_member", forbidden)},
			{"uni_a", with(staff(forbidden), "university_rep", nil)},
			{"club_sensitive", with(staff(forbidden), "club_member", forbidden)},
			{"platform_org", staff(forbidden)},
		} {
			for _, p := range authztest.Personas() {
				want, ok := tc.want[p.Name]
				if !ok {
					want = notFound
				}
				wantErr(t, authz.Can(ctx, p.Actor, act, res(tc.org)), want, false)
			}
		}
	}
	// count, dispatch list/read/cancel: the same S cells plus platform_admin on every org (✓).
	for _, act := range []authz.Action{authz.OutreachAudienceCount, authz.OutreachDispatchList, authz.OutreachDispatchRead,
		authz.OutreachDispatchCancel} {
		res := dispatchesOf
		if act == authz.OutreachAudienceCount {
			res = segmentsOf
		}
		for _, tc := range []struct {
			org  string
			want map[string]error
		}{
			{"company_a", with(staff(nil), "company_member", nil, "company_admin", nil)},
			{"club_a", with(staff(nil), "club_admin", nil, "club_member", forbidden)},
			{"uni_a", with(staff(nil), "university_rep", nil)},
			{"club_sensitive", with(staff(nil), "club_member", forbidden)},
			{"platform_org", staff(nil)},
		} {
			for _, p := range authztest.Personas() {
				want, ok := tc.want[p.Name]
				if !ok {
					want = notFound
				}
				wantErr(t, authz.Can(ctx, p.Actor, act, res(tc.org)), want, false)
			}
		}
	}
}

func TestOutreachPolicy_EdgeCases(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// company_owner counts as company_admin (S); the sensitive club's own
	// club_admin holds the S cells of its club (k16 is the service's).
	wantErr(t, authz.Can(ctx, persona(t, "company_owner_a"), authz.OutreachSegmentManage, segmentsOf("company_a")), nil, false)
	wantErr(t, authz.Can(ctx, persona(t, "club_admin_sensitive"), authz.OutreachAudienceCount, segmentsOf("club_sensitive")), nil, false)
	wantErr(t, authz.Can(ctx, persona(t, "club_admin_sensitive"), authz.OutreachSegmentManage, segmentsOf("club_sensitive")), nil, false)

	// career_staff takes the university_rep cells (03 §1.1 row 8) without MFA.
	uni := authztest.ID("org_university")
	cs := member(authz.OrgUniversity, authz.MemberCareerStaff)
	res := authz.Resource{Type: authz.TypeSegment, ID: uni, OrgID: &uni}
	wantErr(t, authz.Can(ctx, cs, authz.OutreachSegmentManage, res), nil, false)
	wantErr(t, authz.Can(ctx, cs, authz.OutreachAudienceCount, res), nil, false)

	// k-mfa: university_rep, company_admin and platform_admin need an MFA
	// session; company_member (recruiter) and club_admin do not.
	for _, name := range []string{"university_rep", "company_admin", "platform_admin"} {
		a := persona(t, name)
		a.MFAVerified = false
		org := map[string]string{"university_rep": "uni_a", "company_admin": "company_a", "platform_admin": "company_a"}[name]
		wantErr(t, authz.Can(ctx, a, authz.OutreachAudienceCount, segmentsOf(org)), authz.ErrMFARequired, false)
	}
	for _, tc := range []struct{ name, org string }{{"company_member", "company_a"}, {"club_admin", "club_a"}} {
		a := persona(t, tc.name)
		a.MFAVerified = false
		wantErr(t, authz.Can(ctx, a, authz.OutreachSegmentManage, segmentsOf(tc.org)), nil, false)
	}

	// k-po: a platform_admin who belongs to the platform org manages its
	// segments; without that membership it counts but does not manage.
	pad := persona(t, "platform_admin")
	po := authztest.ID("platform_org")
	pad.Memberships = append(pad.Memberships, authz.Membership{OrgID: po, OrgKind: authz.OrgPlatform, Role: authz.MemberStaff})
	for _, act := range []authz.Action{authz.OutreachSegmentList, authz.OutreachSegmentRead, authz.OutreachSegmentManage, authz.OutreachAudienceCount} {
		wantErr(t, authz.Can(ctx, pad, act, segmentsOf("platform_org")), nil, false)
	}

	// Y4: a minor never acts as club_admin.
	minorAdmin := persona(t, "club_admin")
	minorAdmin.IsMinor = true
	wantErr(t, authz.Can(ctx, minorAdmin, authz.OutreachAudienceCount, segmentsOf("club_a")), authz.ErrForbidden, false)

	// A resource without OrgID is nobody's surface but the staff's; another
	// type is not visible.
	bare := authz.Resource{Type: authz.TypeSegment, ID: uuid.NewV7()}
	wantErr(t, authz.Can(ctx, persona(t, "company_admin"), authz.OutreachSegmentList, bare), authz.ErrNotFound, false)
	wantErr(t, authz.Can(ctx, persona(t, "platform_admin"), authz.OutreachAudienceCount, bare), nil, false)
	wantErr(t, authz.Can(ctx, persona(t, "company_admin"), authz.OutreachSegmentList, adminSurface), authz.ErrNotFound, false)

	// BE-035 dispatches: the sensitive club's own club_admin dispatches to
	// its members (T-258; k16 is the service's); career_staff without MFA;
	// k-mfa for company_admin; k-po for create, ✓ for the rest; a minor
	// never acts as club_admin (Y4).
	for _, act := range []authz.Action{authz.OutreachDispatchCreate, authz.OutreachDispatchList, authz.OutreachDispatchRead,
		authz.OutreachDispatchCancel} {
		wantErr(t, authz.Can(ctx, persona(t, "club_admin_sensitive"), act, dispatchesOf("club_sensitive")), nil, false)
		wantErr(t, authz.Can(ctx, persona(t, "company_owner_a"), act, dispatchesOf("company_a")), nil, false)
		wantErr(t, authz.Can(ctx, cs, act, authz.Resource{Type: authz.TypeDispatch, ID: uni, OrgID: &uni}), nil, false)
		noMFA := persona(t, "company_admin")
		noMFA.MFAVerified = false
		wantErr(t, authz.Can(ctx, noMFA, act, dispatchesOf("company_a")), authz.ErrMFARequired, false)
		wantErr(t, authz.Can(ctx, pad, act, dispatchesOf("platform_org")), nil, false)
		wantErr(t, authz.Can(ctx, minorAdmin, act, dispatchesOf("club_a")), authz.ErrForbidden, false)
		// The system actor never reaches the HTTP actions; only outreach.dispatch.run.
		wantErr(t, authz.Can(ctx, authz.SystemActor("outreach.run_dispatch"), act, dispatchesOf("company_a")), authz.ErrForbidden, false)
	}
	wantErr(t, authz.Can(ctx, authz.SystemActor("outreach.run_dispatch"), authz.OutreachDispatchRun, authz.Resource{}), nil, false)
	// k-po: a platform_admin outside the platform org dispatches nothing, but lists, reads and cancels everywhere.
	plain := persona(t, "platform_admin")
	wantErr(t, authz.Can(ctx, plain, authz.OutreachDispatchCreate, dispatchesOf("company_a")), authz.ErrForbidden, false)
	wantErr(t, authz.Can(ctx, plain, authz.OutreachDispatchCancel, dispatchesOf("company_a")), nil, false)
}
