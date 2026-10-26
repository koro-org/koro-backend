package authz_test

import (
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
)

// auditOrgLog is the audit_log resource of queryOrgAuditEvents for org.
func auditOrgLog(org string) authz.Resource {
	id := authztest.ID(org)
	return authz.Resource{Type: authz.TypeAuditLog, ID: id, OrgID: &id}
}

// TestAuditPolicy_Matrix pins 03 §5.21 for the 18 personas (the HTTP
// matrix, testdata/authz/audit.yaml, checks the same through the API).
func TestAuditPolicy_Matrix(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	forbidden, notFound := authz.ErrForbidden, authz.ErrNotFound
	staffOnly := map[string]error{"dpo": nil, "platform_admin": nil}
	orgClubA := map[string]error{
		"anonymous": forbidden, "club_member": forbidden, "club_admin": nil,
		"moderator": forbidden, "support": forbidden, "dpo": nil, "platform_admin": nil,
		"finance_admin": forbidden, "talent_auditor": forbidden,
	}
	orgCompanyA := map[string]error{
		"anonymous": forbidden, "company_member": forbidden, "company_admin": nil,
		"moderator": forbidden, "support": forbidden, "dpo": nil, "platform_admin": nil,
		"finance_admin": forbidden, "talent_auditor": forbidden,
	}
	orgUniA := map[string]error{
		"anonymous": forbidden, "university_rep": nil,
		"moderator": forbidden, "support": forbidden, "dpo": nil, "platform_admin": nil,
		"finance_admin": forbidden, "talent_auditor": forbidden,
		// club_a belongs to uni_a, but a club membership is no university membership.
	}
	for _, tc := range []struct {
		name    string
		act     authz.Action
		res     func(p authztest.Persona) authz.Resource
		want    map[string]error
		missing error // personas not in want
	}{
		{"list_own", authz.AuditEventListOwn, func(p authztest.Persona) authz.Resource {
			u := p.User()
			return authz.Resource{Type: authz.TypeAuditLog, ID: u, OwnerUserID: &u}
		}, map[string]error{"anonymous": forbidden}, nil},
		{"list_org club_a", authz.AuditEventListOrg, func(authztest.Persona) authz.Resource { return auditOrgLog("club_a") }, orgClubA, notFound},
		{"list_org company_a", authz.AuditEventListOrg, func(authztest.Persona) authz.Resource { return auditOrgLog("company_a") }, orgCompanyA, notFound},
		{"list_org uni_a", authz.AuditEventListOrg, func(authztest.Persona) authz.Resource { return auditOrgLog("uni_a") }, orgUniA, notFound},
		{"list", authz.AuditEventList, func(authztest.Persona) authz.Resource { return adminSurface }, withAnon(staffOnly), forbidden},
		{"verify", authz.AuditChainVerify, func(authztest.Persona) authz.Resource { return adminSurface }, withAnon(staffOnly), forbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, p := range authztest.Personas() {
				want, ok := tc.want[p.Name]
				if !ok {
					want = tc.missing
				}
				t.Run(p.Name, func(t *testing.T) {
					wantErr(t, authz.Can(ctx, p.Actor, tc.act, tc.res(p)), want, false)
				})
			}
		})
	}
}

func withAnon(m map[string]error) map[string]error {
	out := map[string]error{"anonymous": authz.ErrForbidden}
	for k, v := range m {
		out[k] = v
	}
	return out
}

func TestAuditPolicy_EdgeCases(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// O: another user's log is invisible.
	other := uuid.NewV7()
	wantErr(t, authz.Can(ctx, persona(t, "student"), authz.AuditEventListOwn,
		authz.Resource{Type: authz.TypeAuditLog, ID: other, OwnerUserID: &other}), authz.ErrNotFound, false)

	// k-cs: career_staff is a university member without the uni cell.
	uni := authztest.ID("uni_a")
	career := authz.Actor{Kind: authz.KindUser, UserID: uuid.NewV7(), MFAVerified: true,
		Memberships: []authz.Membership{{OrgID: uni, OrgKind: authz.OrgUniversity, Role: authz.MemberCareerStaff}}}
	wantErr(t, authz.Can(ctx, career, authz.AuditEventListOrg, auditOrgLog("uni_a")), authz.ErrForbidden, false)

	// Company owner counts as company_admin (S).
	owner := persona(t, "company_owner_a")
	wantErr(t, authz.Can(ctx, owner, authz.AuditEventListOrg, auditOrgLog("company_a")), nil, false)

	// S: a club_admin of club_b sees nothing of club_a (not a member → 404).
	wantErr(t, authz.Can(ctx, persona(t, "other_club_admin"), authz.AuditEventListOrg, auditOrgLog("club_a")), authz.ErrNotFound, false)

	// Y4: a minor club_admin is no club_admin.
	minor := persona(t, "club_admin")
	minor.IsMinor = true
	wantErr(t, authz.Can(ctx, minor, authz.AuditEventListOrg, auditOrgLog("club_a")), authz.ErrForbidden, false)

	// k-mfa: dpo, platform_admin and company_admin need an MFA session.
	for _, name := range []string{"dpo", "platform_admin"} {
		a := persona(t, name)
		a.MFAVerified = false
		wantErr(t, authz.Can(ctx, a, authz.AuditEventList, adminSurface), authz.ErrMFARequired, false)
		wantErr(t, authz.Can(ctx, a, authz.AuditChainVerify, adminSurface), authz.ErrMFARequired, false)
	}
	cma := persona(t, "company_admin")
	cma.MFAVerified = false
	wantErr(t, authz.Can(ctx, cma, authz.AuditEventListOrg, auditOrgLog("company_a")), authz.ErrMFARequired, false)

	// The admin actions are bound to the admin surface, list_org to an org log.
	wantErr(t, authz.Can(ctx, persona(t, "dpo"), authz.AuditEventList, auditOrgLog("club_a")), authz.ErrNotFound, false)
	wantErr(t, authz.Can(ctx, persona(t, "dpo"), authz.AuditEventListOrg, adminSurface), authz.ErrNotFound, false)

	// The chain job is a system action (03 §3.7).
	wantErr(t, authz.Can(ctx, authz.SystemActor("audit.chain"), authz.AuditChainAppend, authz.Resource{Type: authz.TypeAuditLog}), nil, false)
}
