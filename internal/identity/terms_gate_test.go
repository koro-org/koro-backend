package identity_test

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/identity"
)

// newTerms returns a new terms of use version (audience all) of the fake.
func newTerms(version string) identity.NoticeInfo {
	return identity.NoticeInfo{ID: uuid.NewV7(), Kind: consent.KindTermsOfUse, Audience: consent.AudienceAll, Version: version,
		BodySHA256: consent.BodyDigest("Kullanım koşulları " + version), Published: true}
}

// TestTermsGate_CachesPassesAndRefusesUntilAccepted (BE-124): a user who
// accepted the terms in force passes; repeated checks read nothing (one
// ledger read for the first, one version-in-force read per minute). A new
// version is seen within termsCurrentTTL: from then on every check is
// consent.terms_acceptance_required, each decided from the ledger (a
// refusal is never cached), until acceptTerms records it — then the next
// check passes at once.
func TestTermsGate_CachesPassesAndRefusesUntilAccepted(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	u := e.activeUser(t, "gate@example.com")
	gate := e.mod.TermsGate
	reads := func() (int, int) { return e.terms.reads() }

	status0, current0 := reads()
	if err := gate.CheckTerms(ctx, u.ID); err != nil {
		t.Fatalf("accepted user = %v", err)
	}
	for range 5 {
		if err := gate.CheckTerms(ctx, u.ID); err != nil {
			t.Fatal(err)
		}
	}
	if s, c := reads(); s != status0+1 || c != current0 {
		t.Fatalf("six checks read the ledger %d times and the version in force %d times, want 1 and 0", s-status0, c-current0)
	}
	e.clk.Advance(2 * time.Minute) // the version in force is refreshed, the pass stays
	for range 3 {
		if err := gate.CheckTerms(ctx, u.ID); err != nil {
			t.Fatal(err)
		}
	}
	if s, c := reads(); s != status0+1 || c != current0+1 {
		t.Fatalf("after a minute: %d ledger and %d version reads, want 1 and 1", s-status0, c-current0)
	}

	// A version published elsewhere: seen within a minute.
	v2 := newTerms("2026.11.01")
	e.terms.publish(v2)
	if err := gate.CheckTerms(ctx, u.ID); err != nil {
		t.Fatalf("within the cache period = %v (the previous version still counts)", err)
	}
	e.clk.Advance(time.Minute)
	statusBefore, _ := reads()
	for range 3 {
		if err := gate.CheckTerms(ctx, u.ID); !errors.Is(err, consent.ErrTermsAcceptanceRequired) {
			t.Fatalf("after the new version = %v, want consent.terms_acceptance_required", err)
		}
	}
	if s, _ := reads(); s != statusBefore+3 {
		t.Fatalf("three refusals read the ledger %d times: a refusal was cached", s-statusBefore)
	}
	me := authz.Actor{Kind: authz.KindUser, UserID: u.ID}
	if _, err := e.svc.AcceptTerms(ctx, me, identity.TermsAcceptanceInput{NoticeVersionID: v2.ID, Client: identity.ClientWeb}); err != nil {
		t.Fatal(err)
	}
	if err := gate.CheckTerms(ctx, u.ID); err != nil {
		t.Fatalf("after acceptTerms = %v", err)
	}

	// A version published through this process (consent's TermsWatcher):
	// the gate drops its versions in force after the commit, at once.
	v3 := newTerms("2026.12.01")
	e.terms.publish(v3)
	e.mod.TermsPublished(ctx, v3.ID)
	if err := gate.CheckTerms(ctx, u.ID); !errors.Is(err, consent.ErrTermsAcceptanceRequired) {
		t.Fatalf("right after a local publication = %v, want the refusal", err)
	}

	// No terms published: nothing to accept.
	e.terms.retire()
	e.mod.TermsPublished(ctx, v3.ID)
	if err := gate.CheckTerms(ctx, u.ID); err != nil {
		t.Fatalf("without published terms = %v", err)
	}
	// An account gone since its session was resolved.
	if err := gate.CheckTerms(ctx, uuid.NewV7()); !errors.Is(err, identity.ErrUnauthenticated) {
		t.Fatalf("unknown user = %v, want unauthenticated", err)
	}
}

// TestTermsGate_LocaleChangeForgetsThePass (BE-124): updateMe with a new
// locale drops the user's pass (the terms in force may be another
// language's version): the next check reads the ledger again. A name
// change keeps it.
func TestTermsGate_LocaleChangeForgetsThePass(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	u := e.activeUser(t, "locale@example.com")
	me := authz.Actor{Kind: authz.KindUser, UserID: u.ID}
	if err := e.mod.TermsGate.CheckTerms(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	status, _ := e.terms.reads()
	name := "Ayşe"
	if _, err := e.svc.UpdateMe(ctx, me, identity.MePatch{FirstName: &name}); err != nil {
		t.Fatal(err)
	}
	status++ // UpdateMe answers MeDTO with its terms block
	if err := e.mod.TermsGate.CheckTerms(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if s, _ := e.terms.reads(); s != status {
		t.Fatalf("a name change dropped the pass (%d extra ledger reads)", s-status)
	}
	en := identity.LocaleEN
	if _, err := e.svc.UpdateMe(ctx, me, identity.MePatch{Locale: &en}); err != nil {
		t.Fatal(err)
	}
	status++
	if err := e.mod.TermsGate.CheckTerms(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if s, _ := e.terms.reads(); s != status+1 {
		t.Fatalf("after a locale change the gate read the ledger %d times, want 1", s-status)
	}
}
