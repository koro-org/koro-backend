package students_test

import (
	"testing"
	"time"
	"uuid"

	"koro/internal/platform/privacy"
	"koro/internal/students"
)

// TestRetention_RevokedGrants (02 §9.7, §24.2 access_grant_revoked; BE-046):
// the sweeper claims its policy only; an unapproved policy counts the
// grants revoked more than 6 months ago without deleting; an approved one
// deletes exactly those, active and recently revoked grants stay; a second
// run deletes nothing.
func TestRetention_RevokedGrants(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	w := e.mod.Sweeper
	if got := w.PolicyCodes(); len(got) != 1 || got[0] != students.PolicyAccessGrantRevoked {
		t.Fatalf("PolicyCodes = %v", got)
	}
	if _, err := w.Sweep(ctx, privacy.RetentionPolicy{Code: "club_claim"}, t0); err == nil {
		t.Fatal("a foreign policy was swept")
	}
	org, rec, _ := e.company("Acme")
	old, _ := e.granted(t, org)
	recent, _ := e.granted(t, org)
	active, _ := e.granted(t, org)
	if err := e.svc.RevokeCompanyAccessGrant(ctx, rec, org, old.UserID, ""); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(200 * 24 * time.Hour)
	if err := e.svc.RevokeCompanyAccessGrant(ctx, rec, org, recent.UserID, ""); err != nil {
		t.Fatal(err)
	}
	now := e.clk.Now()
	policy := privacy.RetentionPolicy{Code: students.PolicyAccessGrantRevoked, OwnerModule: "students",
		Target: "profile_access_grants", Retention: 183 * 24 * time.Hour, Method: privacy.MethodHardDelete, Enabled: true}

	res, err := w.Sweep(ctx, policy, now)
	if err != nil || res.Count != 1 || res.Table != "profile_access_grants" || res.Method != privacy.MethodHardDelete {
		t.Fatalf("dry run = %+v, %v", res, err)
	}
	if all, _ := e.store.AllGrants(ctx, old.UserID); len(all) != 1 {
		t.Fatal("the dry run deleted")
	}
	policy.LegalApproved = true
	if res, err = w.Sweep(ctx, policy, now); err != nil || res.Count != 1 {
		t.Fatalf("sweep = %+v, %v", res, err)
	}
	for user, want := range map[uuid.UUID]int{old.UserID: 0, recent.UserID: 1, active.UserID: 1} {
		if all, _ := e.store.AllGrants(ctx, user); len(all) != want {
			t.Errorf("grants of %v = %d, want %d", user, len(all), want)
		}
	}
	if res, err = w.Sweep(ctx, policy, now); err != nil || res.Count != 0 {
		t.Fatalf("second sweep = %+v, %v", res, err)
	}
}
