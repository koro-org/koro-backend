package authz_test

import (
	"errors"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
)

// TestTalentPolicy_SystemActionsOnly (03 §3.7; BE-047, BE-052): the talent
// consumers write the ledger as system jobs (talent.ledger.write), the
// nightly run scores as one (talent.score.compute); a system actor without
// a job is refused, and no persona holds either system action.
func TestTalentPolicy_SystemActionsOnly(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	for _, c := range []struct {
		job string
		act authz.Action
	}{{"talent.ingest_checkin", authz.TalentLedgerWrite}, {"talent.recompute", authz.TalentScoreCompute}} {
		if err := authz.Can(ctx, authz.SystemActor(c.job), c.act, authz.Resource{}); err != nil {
			t.Fatalf("system %s = %v", c.act, err)
		}
		if err := authz.Can(ctx, authz.SystemActor(""), c.act, authz.Resource{}); !errors.Is(err, authz.ErrForbidden) {
			t.Fatalf("system without a job %s = %v", c.act, err)
		}
	}
	// A system job holds no HTTP talent action (03 §3.7 is a closed list).
	for _, act := range authz.ModuleActions("talent") {
		if err := authz.Can(ctx, authz.SystemActor("talent.recompute"), act, authz.Resource{Type: authz.TypeAdminSurface}); err == nil {
			t.Fatalf("system job holds %s", act)
		}
	}
	for _, p := range append(authztest.Personas(), authztest.Helpers()...) {
		for _, act := range []authz.Action{authz.TalentLedgerWrite, authz.TalentScoreCompute} {
			if err := authz.Can(ctx, p.Actor, act, authz.Resource{}); !errors.Is(err, authz.ErrForbidden) {
				t.Fatalf("%s as %s = %v, want ErrForbidden", act, p.Name, err)
			}
		}
	}
}

// TestTalentPolicy_AuditorOnly (03 §5.12, Y1, k-mfa; BE-052): the six talent
// HTTP actions are the talent_auditor's alone, on a subject and on the
// admin surface; every other persona — platform_admin, the dpo and the
// subject herself included — gets 403 (the admin surface is visible to
// every user: nothing is read first), the anonymous actor too; an
// auditor's session without MFA gets authz.mfa_required. The flag is not
// authz's (the service answers 404 first).
func TestTalentPolicy_AuditorOnly(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	student, _ := authztest.Get("student")
	self := student.User()
	resources := []authz.Resource{
		{Type: authz.TypeTalentSubject, OwnerUserID: &self},
		{Type: authz.TypeTalentSubject},
		{Type: authz.TypeAdminSurface},
	}
	acts := authz.ModuleActions("talent")
	if len(acts) != 6 {
		t.Fatalf("talent HTTP actions = %d, want 6 (03 §8)", len(acts))
	}
	for _, p := range append(authztest.Personas(), authztest.Helpers()...) {
		for _, act := range acts {
			for _, r := range resources {
				err := authz.Can(ctx, p.Actor, act, r)
				switch {
				case p.Actor.HasPlatformRole(authz.PlatformTalentAuditor):
					if err != nil {
						t.Fatalf("%s as %s on %s = %v, want allowed", act, p.Name, r.Type, err)
					}
				case !errors.Is(err, authz.ErrForbidden) || errors.Is(err, authz.ErrMFARequired):
					t.Fatalf("%s as %s on %s = %v, want ErrForbidden", act, p.Name, r.Type, err)
				}
			}
		}
	}
	auditor, _ := authztest.Get("talent_auditor")
	noMFA := auditor.Actor
	noMFA.MFAVerified = false
	for _, act := range acts {
		if err := authz.Can(ctx, noMFA, act, authz.Resource{Type: authz.TypeAdminSurface}); !errors.Is(err, authz.ErrMFARequired) {
			t.Fatalf("%s without MFA = %v, want authz.mfa_required", act, err)
		}
	}
	// Another resource type is not the talent surface.
	other := uuid.NewV7()
	if err := authz.Can(ctx, auditor.Actor, authz.TalentReportRead, authz.Resource{Type: authz.TypeUser, ID: other}); err == nil {
		t.Fatal("talent.report.read on a user resource allowed")
	}
}
