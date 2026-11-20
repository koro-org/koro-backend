package authz_test

import (
	"testing"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
)

// TestIngestPolicy_QuickImportIsTheClubAdmins (BE-211): the quick import
// of a club (ingest.quick_import.run, the path club's TypeOrg resource) is
// its club_admin's only, as the createEvent it prepares: a member without
// the role 403, the platform roles 403 (they see the org surface but hold
// no grant, not even platform_admin), everyone else — another club's admin
// included — 404.
func TestIngestPolicy_QuickImportIsTheClubAdmins(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	res := authz.OrgResource(authztest.ID("club_a"), string(authz.OrgClub), "active")
	forbidden, notFound := authz.ErrForbidden, authz.ErrNotFound
	want := map[string]error{
		"club_admin": nil, "club_member": forbidden, "anonymous": forbidden,
		"moderator": forbidden, "support": forbidden, "dpo": forbidden, "platform_admin": forbidden,
		"finance_admin": forbidden, "talent_auditor": forbidden,
	}
	for _, p := range authztest.Personas() {
		w, ok := want[p.Name]
		if !ok {
			w = notFound
		}
		t.Run(p.Name, func(t *testing.T) {
			t.Parallel()
			wantErr(t, authz.Can(ctx, p.Actor, authz.IngestQuickImportRun, res), w, false)
		})
	}
	// A company's admin on its own company: not a club, no grant.
	p, _ := authztest.Get("company_admin")
	company := authz.OrgResource(authztest.ID("company_a"), string(authz.OrgCompany), "active")
	wantErr(t, authz.Can(ctx, p.Actor, authz.IngestQuickImportRun, company), forbidden, false)
}
