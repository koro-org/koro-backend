package authz_test

import (
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
)

// entitlementsOf is the Resource the entitlements service builds for the
// org endpoints (03 §3.5; H-27): Type entitlement, OrgID = the path's org.
func entitlementsOf(org string) authz.Resource {
	id := authztest.ID(org)
	return authz.Resource{Type: authz.TypeEntitlement, ID: id, OrgID: &id}
}

// TestEntitlementsPolicy_Matrix pins 03 §5.11 for the 18 personas (the HTTP
// matrix, testdata/authz/entitlements.yaml, checks the same through the API).
func TestEntitlementsPolicy_Matrix(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	forbidden, notFound := authz.ErrForbidden, authz.ErrNotFound
	companyA := map[string]error{
		"anonymous": forbidden, "company_member": nil, "company_admin": nil,
		"moderator": forbidden, "support": forbidden, "dpo": forbidden, "platform_admin": nil,
		"finance_admin": nil, "talent_auditor": forbidden,
	}
	companyB := map[string]error{
		"anonymous": forbidden, "other_company_member": nil,
		"moderator": forbidden, "support": forbidden, "dpo": forbidden, "platform_admin": nil,
		"finance_admin": nil, "talent_auditor": forbidden,
	}
	// club_a members see the surface (403); nobody holds an entitlement role there.
	clubA := map[string]error{
		"anonymous": forbidden, "club_member": forbidden, "club_admin": forbidden,
		"moderator": forbidden, "support": forbidden, "dpo": forbidden, "platform_admin": nil,
		"finance_admin": nil, "talent_auditor": forbidden,
	}
	admin := map[string]error{"platform_admin": nil, "finance_admin": nil}
	for _, act := range []authz.Action{authz.EntitlementsEntitlementList, authz.EntitlementsEntitlementRead, authz.EntitlementsUsageList} {
		for _, tc := range []struct {
			org  string
			want map[string]error
		}{{"company_a", companyA}, {"company_b", companyB}, {"club_a", clubA}} {
			for _, p := range authztest.Personas() {
				want, ok := tc.want[p.Name]
				if !ok {
					want = notFound
				}
				wantErr(t, authz.Can(ctx, p.Actor, act, entitlementsOf(tc.org)), want, false)
			}
		}
	}
	for _, act := range []authz.Action{authz.EntitlementsEntitlementGrant, authz.EntitlementsEntitlementRevoke} {
		for _, p := range authztest.Personas() {
			want, ok := withAnon(admin)[p.Name]
			if !ok {
				want = forbidden // admin surface: 403, never 404
			}
			wantErr(t, authz.Can(ctx, p.Actor, act, adminSurface), want, false)
		}
	}
}

func TestEntitlementsPolicy_EdgeCases(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// company_owner counts as company_admin (S).
	wantErr(t, authz.Can(ctx, persona(t, "company_owner_a"), authz.EntitlementsEntitlementList, entitlementsOf("company_a")), nil, false)

	// k-mfa: company_admin, platform_admin and finance_admin need an MFA
	// session; company_member (recruiter) does not.
	cma := persona(t, "company_admin")
	cma.MFAVerified = false
	wantErr(t, authz.Can(ctx, cma, authz.EntitlementsUsageList, entitlementsOf("company_a")), authz.ErrMFARequired, false)
	cmm := persona(t, "company_member")
	cmm.MFAVerified = false
	wantErr(t, authz.Can(ctx, cmm, authz.EntitlementsUsageList, entitlementsOf("company_a")), nil, false)
	for _, name := range []string{"platform_admin", "finance_admin"} {
		a := persona(t, name)
		a.MFAVerified = false
		wantErr(t, authz.Can(ctx, a, authz.EntitlementsEntitlementGrant, adminSurface), authz.ErrMFARequired, false)
		wantErr(t, authz.Can(ctx, a, authz.EntitlementsEntitlementRead, entitlementsOf("company_a")), authz.ErrMFARequired, false)
	}

	// An org resource without OrgID is nobody's surface but the staff's.
	bare := authz.Resource{Type: authz.TypeEntitlement, ID: uuid.NewV7()}
	wantErr(t, authz.Can(ctx, persona(t, "company_admin"), authz.EntitlementsEntitlementList, bare), authz.ErrNotFound, false)
	wantErr(t, authz.Can(ctx, persona(t, "finance_admin"), authz.EntitlementsEntitlementList, bare), nil, false)

	// The admin actions are bound to the admin surface, the org actions to
	// an entitlement resource.
	wantErr(t, authz.Can(ctx, persona(t, "platform_admin"), authz.EntitlementsEntitlementGrant, entitlementsOf("company_a")), authz.ErrNotFound, false)
	wantErr(t, authz.Can(ctx, persona(t, "platform_admin"), authz.EntitlementsEntitlementList, adminSurface), authz.ErrNotFound, false)

	// The expiry job is a system action (03 §3.7); no user may run it.
	wantErr(t, authz.Can(ctx, authz.SystemActor("entitlements.expire"), authz.EntitlementsEntitlementExpire, authz.Resource{Type: authz.TypeEntitlement}), nil, false)
	wantErr(t, authz.Can(ctx, persona(t, "platform_admin"), authz.EntitlementsEntitlementExpire, entitlementsOf("company_a")), authz.ErrForbidden, false)
	wantErr(t, authz.Can(ctx, authz.SystemActor("entitlements.expire"), authz.EntitlementsEntitlementGrant, adminSurface), authz.ErrForbidden, false)
}
