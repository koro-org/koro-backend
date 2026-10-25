package identity_test

import (
	"errors"
	"net/http"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/identity"
	"koro/internal/platform/problem/problemtest"
)

// TestRegister_WithoutCurrentTerms_422 (BE-118, Q-17): no accepted terms,
// a retired version, the privacy notice in their place or an unknown ID
// are all 422 consent.terms_not_current, before anything is hashed or
// looked up: a taken address fails alike (no oracle); no account exists.
func TestRegister_WithoutCurrentTerms_422(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	e.activeUser(t, "taken@example.com")
	hashes := e.hasher.hash.Load()
	for name, id := range map[string]uuid.UUID{
		"not accepted": uuid.Nil(), "retired version": e.notices.oldTerms.ID, "privacy notice": e.notices.privacy.ID,
		"consent text": e.notices.ccEmail.ID, "unknown": uuid.NewV7(),
	} {
		for _, address := range []string{"new@example.com", "taken@example.com"} {
			r := e.registration(address)
			r.TermsNoticeID = id
			err := e.svc.Register(ctx, authz.Anonymous(), r)
			if st, code := problemtest.StatusCode(err); st != http.StatusUnprocessableEntity || code != "consent.terms_not_current" {
				t.Errorf("%s (%s) = %d %s, want 422 consent.terms_not_current", name, address, st, code)
			}
		}
	}
	if got := e.hasher.hash.Load() - hashes; got != 0 {
		t.Fatalf("a refused registration reached the hasher %d times", got)
	}
	if _, found, _ := e.svc.FindByEmail(ctx, "new@example.com"); found {
		t.Fatal("an account exists without accepted terms")
	}
	if len(e.terms.calls) != 1 { // the taken account's own registration
		t.Fatalf("terms ledger calls = %d, want 1", len(e.terms.calls))
	}
}

// TestRegister_RecordsTermsInSameTx: the acceptance goes to the terms
// ledger in the registration transaction with the persona's audience and
// the client's channel; a taken address records nothing; a ledger failure
// (or a version retired since the check) fails the registration
// transaction that inserted the account.
func TestRegister_RecordsTermsInSameTx(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	r := e.registration("terms@example.com")
	r.Client = identity.ClientMobile
	if err := e.svc.Register(t.Context(), authz.Anonymous(), r); err != nil {
		t.Fatal(err)
	}
	if len(e.terms.calls) != 1 || len(e.store.insertTx) != 1 {
		t.Fatalf("terms calls %d, user inserts %d", len(e.terms.calls), len(e.store.insertTx))
	}
	if e.terms.txs[0] == nil || e.terms.txs[0] != e.store.insertTx[0] {
		t.Fatalf("terms recorded in tx %v, account inserted in tx %v: want the same transaction", e.terms.txs[0], e.store.insertTx[0])
	}
	id, _, _ := e.svc.FindByEmail(t.Context(), "terms@example.com")
	if c := e.terms.calls[0]; c.UserID != id || c.NoticeVersionID != e.notices.terms.ID || c.Audience != consent.AudienceStudent ||
		c.Channel != consent.ChannelMobile {
		t.Fatalf("terms input = %+v", c)
	}
	// The taken address: 202, nothing recorded for the existing account.
	if err := e.svc.Register(t.Context(), authz.Anonymous(), e.registration("terms@example.com")); err != nil {
		t.Fatal(err)
	}
	if len(e.terms.calls) != 1 {
		t.Fatalf("a taken registration recorded terms: %d calls", len(e.terms.calls))
	}

	for name, fail := range map[string]error{"ledger down": errors.New("terms ledger down"), "retired meanwhile": consent.ErrTermsNotCurrent} {
		e2 := newEnv(t)
		e2.terms.err = fail
		err := e2.svc.Register(t.Context(), authz.Anonymous(), e2.registration("rollback@example.com"))
		if err == nil {
			t.Fatalf("%s: registration succeeded", name)
		}
		if errors.Is(fail, consent.ErrTermsNotCurrent) {
			problemtest.WantErr(t, err, consent.ErrTermsNotCurrent)
		}
		// The in-memory store does not roll back (NopTx): the account insert
		// ran in the transaction that failed (PostgreSQL: TestRegister_Postgres_UserAndConsentCommitTogether).
		if len(e2.tx.failed) != 1 || e2.tx.failed[0] != e2.store.insertTx[0] {
			t.Fatalf("%s: failed transactions %v, account insert in %v", name, e2.tx.failed, e2.store.insertTx)
		}
		if len(e2.out.Events()) != 0 {
			t.Fatalf("%s: an event was committed for a rolled-back registration", name)
		}
	}
}

// TestAcceptTerms_NewVersion: getMe reports the terms accepted at
// registration; a new version makes acceptance_required true until
// acceptTerms records it; the old or an unknown version is 422; accepting
// again changes nothing; another user's acceptance is not the caller's.
func TestAcceptTerms_NewVersion(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	u := e.activeUser(t, "accept@example.com")
	other := e.activeUser(t, "other@example.com")
	a := authz.Actor{Kind: authz.KindUser, UserID: u.ID, StudentStatus: authz.StudentNone}
	me, err := e.svc.Me(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if me.Terms.Required || me.Terms.CurrentID == nil || *me.Terms.CurrentID != e.notices.terms.ID || me.Terms.CurrentVersion != "2026.10.01" ||
		me.Terms.AcceptedID == nil || *me.Terms.AcceptedID != e.notices.terms.ID || me.Terms.AcceptedAt == nil {
		t.Fatalf("after registration: terms = %+v", me.Terms)
	}

	v2 := identity.NoticeInfo{ID: uuid.NewV7(), Kind: consent.KindTermsOfUse, Audience: consent.AudienceAll, Version: "2026.11.01",
		BodySHA256: consent.BodyDigest("Yeni koşullar")}
	e.terms.publish(v2)
	me, _ = e.svc.Me(ctx, a)
	if !me.Terms.Required || *me.Terms.CurrentID != v2.ID || *me.Terms.AcceptedID != e.notices.terms.ID {
		t.Fatalf("after a new version: terms = %+v", me.Terms)
	}
	for name, id := range map[string]uuid.UUID{"old version": e.notices.terms.ID, "unknown": uuid.NewV7(), "nil": uuid.Nil()} {
		_, err := e.svc.AcceptTerms(ctx, a, identity.TermsAcceptanceInput{NoticeVersionID: id, Client: identity.ClientWeb})
		if st, code := problemtest.StatusCode(err); st != http.StatusUnprocessableEntity || code != "consent.terms_not_current" {
			t.Errorf("%s = %d %s, want 422 consent.terms_not_current", name, st, code)
		}
	}
	st, err := e.svc.AcceptTerms(ctx, a, identity.TermsAcceptanceInput{NoticeVersionID: v2.ID, Client: identity.ClientMobile})
	if err != nil || st.Required || *st.AcceptedID != v2.ID {
		t.Fatalf("AcceptTerms = %+v, %v", st, err)
	}
	last := e.terms.calls[len(e.terms.calls)-1]
	if last.UserID != u.ID || last.Audience != consent.AudienceStudent || last.Channel != consent.ChannelMobile || last.NoticeVersionID != v2.ID {
		t.Fatalf("terms input = %+v", last)
	}
	again, err := e.svc.AcceptTerms(ctx, a, identity.TermsAcceptanceInput{NoticeVersionID: v2.ID, Client: identity.ClientWeb})
	if err != nil || again.Required || !again.AcceptedAt.Equal(*st.AcceptedAt) {
		t.Fatalf("second AcceptTerms = %+v, %v", again, err)
	}
	if me, _ := e.svc.Me(ctx, a); me.Terms.Required {
		t.Fatalf("after acceptTerms: terms = %+v", me.Terms)
	}
	// updateMe answers the same terms block.
	en := identity.LocaleEN
	if me, err := e.svc.UpdateMe(ctx, a, identity.MePatch{Locale: &en}); err != nil || me.Terms.Required || *me.Terms.AcceptedID != v2.ID {
		t.Fatalf("UpdateMe terms = %+v, %v", me.Terms, err)
	}
	// Another account still has to accept v2; an anonymous caller is refused.
	if me, _ := e.svc.Me(ctx, authz.Actor{Kind: authz.KindUser, UserID: other.ID}); !me.Terms.Required {
		t.Fatalf("another account's terms = %+v", me.Terms)
	}
	if _, err := e.svc.AcceptTerms(ctx, authz.Anonymous(), identity.TermsAcceptanceInput{NoticeVersionID: v2.ID}); err == nil {
		t.Fatal("an anonymous caller accepted terms")
	}
}
