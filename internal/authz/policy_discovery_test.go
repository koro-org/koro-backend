package authz_test

import (
	"testing"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
)

// anonProfileOf is the Resource the discovery service builds (03 §3.5;
// H-27): anon_profile on the path's company, the pseudonym's company in
// Attrs company_org_id.
func anonProfileOf(org, pseudonymOrg string) authz.Resource {
	id := authztest.ID(org)
	return authz.Resource{Type: authz.TypeAnonProfile, OrgID: &id,
		Attrs: map[string]string{"company_org_id": authztest.ID(pseudonymOrg).String()}}
}

// TestDiscoveryPolicy_Matrix pins 03 §5.10 for the 18 personas (the HTTP
// matrix, testdata/authz/discovery.yaml, checks the same through the
// API): cmm S and cma S on the company's surface, platform roles 403,
// members of another org and everyone else 404.
func TestDiscoveryPolicy_Matrix(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	forbidden, notFound := authz.ErrForbidden, authz.ErrNotFound
	staff := map[string]error{
		"anonymous": forbidden, "moderator": forbidden, "support": forbidden, "dpo": forbidden,
		"platform_admin": forbidden, "finance_admin": forbidden, "talent_auditor": forbidden,
	}
	with := func(extra map[string]error) map[string]error {
		out := map[string]error{}
		for k, v := range staff {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	for _, tc := range []struct {
		org  string
		want map[string]error
	}{
		{"company_a", with(map[string]error{"company_member": nil, "company_admin": nil})},
		{"company_b", with(map[string]error{"other_company_member": nil})},
		// club_a's members see the surface (403); nobody holds a company role there.
		{"club_a", with(map[string]error{"club_member": forbidden, "club_admin": forbidden})},
	} {
		for _, act := range []authz.Action{authz.DiscoveryProfileSearch, authz.DiscoveryProfileRead} {
			for _, p := range authztest.Personas() {
				want, ok := tc.want[p.Name]
				if !ok {
					want = notFound
				}
				wantErr(t, authz.Can(ctx, p.Actor, act, anonProfileOf(tc.org, tc.org)), want, false)
			}
		}
	}
}

func TestDiscoveryPolicy_EdgeCases(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// company_owner counts as company_admin (S).
	wantErr(t, authz.Can(ctx, persona(t, "company_owner_a"), authz.DiscoveryProfileSearch, anonProfileOf("company_a", "company_a")), nil, false)

	// k-mfa: company_admin needs an MFA session; the recruiter does not.
	cma := persona(t, "company_admin")
	cma.MFAVerified = false
	wantErr(t, authz.Can(ctx, cma, authz.DiscoveryProfileSearch, anonProfileOf("company_a", "company_a")), authz.ErrMFARequired, false)
	cmm := persona(t, "company_member")
	cmm.MFAVerified = false
	wantErr(t, authz.Can(ctx, cmm, authz.DiscoveryProfileRead, anonProfileOf("company_a", "company_a")), nil, false)

	// A pseudonym of another company is never visible, not even on the
	// actor's own surface (the service resolves under the path's company
	// only; this is the second line).
	for _, name := range []string{"company_member", "company_admin", "other_company_member", "platform_admin"} {
		wantErr(t, authz.Can(ctx, persona(t, name), authz.DiscoveryProfileRead, anonProfileOf("company_a", "company_b")),
			authz.ErrNotFound, false)
	}

	// The mandatory Attrs key (03 §3.5) and the resource type.
	id := authztest.ID("company_a")
	wantErr(t, authz.Can(ctx, persona(t, "company_member"), authz.DiscoveryProfileSearch,
		authz.Resource{Type: authz.TypeAnonProfile, OrgID: &id}), authz.ErrForbidden, true)
	wantErr(t, authz.Can(ctx, persona(t, "company_member"), authz.DiscoveryProfileSearch,
		authz.Resource{Type: authz.TypeStudentProfile, OrgID: &id,
			Attrs: map[string]string{"access_grant": "none", "discoverable": "true", "minor": "false"}}), authz.ErrNotFound, false)
	// Without an org (a malformed request) nothing is visible.
	wantErr(t, authz.Can(ctx, persona(t, "company_member"), authz.DiscoveryProfileSearch,
		authz.Resource{Type: authz.TypeAnonProfile, Attrs: map[string]string{"company_org_id": id.String()}}), authz.ErrNotFound, false)

	// Discovery is never a system action.
	wantErr(t, authz.Can(ctx, authz.SystemActor("discovery.test"), authz.DiscoveryProfileSearch, anonProfileOf("company_a", "company_a")),
		authz.ErrForbidden, false)
}
