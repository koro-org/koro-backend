package authz_test

import (
	"errors"
	"testing"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
)

// TestTalentPolicy_SystemActionsOnly (03 §3.7, §5.12; BE-047): the talent
// consumers may write the ledger as a system job (talent.ledger.write;
// talent.score.compute for BE-052's nightly run), no user persona holds
// either, and every talent HTTP action is still refused to every persona,
// talent_auditor included, until BE-052 writes the policy.
func TestTalentPolicy_SystemActionsOnly(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	for _, act := range []authz.Action{authz.TalentLedgerWrite, authz.TalentScoreCompute} {
		if err := authz.Can(ctx, authz.SystemActor("talent.ingest_checkin"), act, authz.Resource{}); err != nil {
			t.Fatalf("system %s = %v", act, err)
		}
		if err := authz.Can(ctx, authz.SystemActor(""), act, authz.Resource{}); !errors.Is(err, authz.ErrForbidden) {
			t.Fatalf("system without a job %s = %v", act, err)
		}
	}
	subject := authz.Resource{Type: authz.TypeTalentSubject}
	for _, p := range append(authztest.Personas(), authztest.Helpers()...) {
		for _, act := range append(authz.ModuleActions("talent"), authz.TalentLedgerWrite, authz.TalentScoreCompute) {
			if err := authz.Can(ctx, p.Actor, act, subject); !errors.Is(err, authz.ErrForbidden) {
				t.Fatalf("%s as %s = %v, want ErrForbidden", act, p.Name, err)
			}
		}
	}
}
