package students_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/problem"
	"koro/internal/students"
)

// sharedWith reports whether org's level 3 view of s carries the e-mail.
func (e *env) sharedWith(t *testing.T, rec authz.Actor, org uuid.UUID, s authz.Actor) bool {
	t.Helper()
	d, err := e.svc.GetCompanyStudentProfile(t.Context(), rec, org, s.UserID)
	if err != nil {
		t.Fatalf("level 3 of %s: %v", org, err)
	}
	return d.Contact != nil
}

// grantOf returns s's open grant of org.
func (e *env) grantOf(t *testing.T, s authz.Actor, org uuid.UUID) students.AccessGrant {
	t.Helper()
	gs, err := e.store.AllGrants(t.Context(), s.UserID)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range gs {
		if g.CompanyOrgID == org && g.Open() {
			return g
		}
	}
	t.Fatalf("no open grant of %s", org)
	return students.AccessGrant{}
}

// TestShareContactWithCompany_OnlyThatCompany (T-345, 01 §6.3; BE-089):
// messaging's acceptance with share_contact shares the e-mail with the
// request's company alone: its grant's contact_shared_at, the
// contact_info_share consent recorded with the shown text (in the
// settings' transaction, the setting mirroring it), the audit record of
// the student's act in the company's scope. Another company holding an
// active grant does not see it; sharing with it later needs its own act
// and records no second consent while the first is in force.
func TestShareContactWithCompany_OnlyThatCompany(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	orgA, recA, _ := e.company("Acme")
	orgB, recB, _ := e.company("Beta")
	s, req := e.granted(t, orgA)
	if err := e.svc.Grant(ctx, s.UserID, orgB, students.SourceContactRequest, uuid.NewV7()); err != nil {
		t.Fatal(err)
	}
	pr := proof()
	if err := e.svc.ShareContactWithCompany(ctx, s, orgA, *pr, consent.ChannelMobile, req); err != nil {
		t.Fatal(err)
	}
	if !e.sharedWith(t, recA, orgA, s) || e.sharedWith(t, recB, orgB, s) {
		t.Fatal("the e-mail must go to the accepted company alone")
	}
	if g := e.grantOf(t, s, orgA); g.ContactSharedAt == nil || !g.ContactSharedAt.Equal(t0) {
		t.Fatalf("company A's grant = %+v", g)
	}
	if g := e.grantOf(t, s, orgB); g.ContactSharedAt != nil {
		t.Fatalf("company B's grant = %+v", g)
	}
	recs := e.consent.records()
	if len(recs) != 1 || recs[0].Purpose != consent.PurposeContactInfoShare || recs[0].Action != consent.Granted ||
		*recs[0].NoticeVersionID != pr.NoticeVersionID || recs[0].Channel != consent.ChannelMobile {
		t.Fatalf("consent records = %+v", recs)
	}
	if e.consent.tx[0] == 0 || len(e.store.privacyTx) != 1 || e.consent.tx[0] != e.store.privacyTx[0] {
		t.Fatalf("consent in tx %d, setting in tx %v: not one transaction", e.consent.tx[0], e.store.privacyTx)
	}
	if p, _ := e.svc.GetMyPrivacy(ctx, s); !p.ShowContactOnGrant {
		t.Fatal("the setting does not mirror the consent given")
	}
	ev := e.audit.Of("students.access_grant.share_contact")
	if len(ev) != 1 || ev[0].OrgID == nil || *ev[0].OrgID != orgA || ev[0].Metadata["contact_request_id"] != req.String() ||
		ev[0].Metadata["contact_info_share_consent_id"] == nil || ev[0].ActorUserID == nil || *ev[0].ActorUserID != s.UserID {
		t.Fatalf("audit = %+v", ev)
	}

	// Company B later: no second consent while the first is in force.
	if err := e.svc.ShareContactWithCompany(ctx, s, orgB, *proof(), consent.ChannelWeb, uuid.NewV7()); err != nil {
		t.Fatal(err)
	}
	if !e.sharedWith(t, recB, orgB, s) || len(e.consent.records()) != 1 {
		t.Fatalf("share with B: consents %d", len(e.consent.records()))
	}
	// Again with A: nothing written.
	if err := e.svc.ShareContactWithCompany(ctx, s, orgA, *proof(), consent.ChannelWeb, uuid.NewV7()); err != nil {
		t.Fatal(err)
	}
	if len(e.audit.Of("students.access_grant.share_contact")) != 2 {
		t.Fatal("sharing again with the same company wrote")
	}
}

// TestUnshareMyAccessGrantContact (T-345; BE-089): the student stops
// sharing with one company: that company's view loses the e-mail at once
// while its level 3 access stays; the other company keeps it; a grant that
// shares nothing changes nothing; another user's grant is 404.
func TestUnshareMyAccessGrantContact(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	orgA, recA, _ := e.company("Acme")
	orgB, recB, _ := e.company("Beta")
	s, req := e.granted(t, orgA)
	if err := e.svc.Grant(ctx, s.UserID, orgB, students.SourceContactRequest, uuid.NewV7()); err != nil {
		t.Fatal(err)
	}
	for _, org := range []uuid.UUID{orgA, orgB} {
		if err := e.svc.ShareContactWithCompany(ctx, s, org, *proof(), consent.ChannelWeb, req); err != nil {
			t.Fatal(err)
		}
	}
	gA := e.grantOf(t, s, orgA)
	if err := e.svc.UnshareMyAccessGrantContact(ctx, s, gA.ID); err != nil {
		t.Fatal(err)
	}
	if e.sharedWith(t, recA, orgA, s) || !e.sharedWith(t, recB, orgB, s) {
		t.Fatal("unsharing with A must hide the e-mail from A only (and keep A's access)")
	}
	ev := e.audit.Of(authz.StudentsAccessGrantRevoke)
	if len(ev) != 1 || ev[0].Metadata["revoked"] != "contact" || *ev[0].OrgID != orgA || *ev[0].ResourceID != gA.ID {
		t.Fatalf("audit = %+v", ev)
	}
	if err := e.svc.UnshareMyAccessGrantContact(ctx, s, gA.ID); err != nil ||
		len(e.audit.Of(authz.StudentsAccessGrantRevoke)) != 1 {
		t.Fatalf("unshare again = %v (it must change nothing)", err)
	}
	other := e.profiled(t)
	if err := e.svc.UnshareMyAccessGrantContact(ctx, other, gA.ID); !errors.Is(err, problem.ErrNotFound) {
		t.Fatalf("another user's grant = %v, want 404", err)
	}
	if err := e.svc.UnshareMyAccessGrantContact(ctx, s, uuid.Nil()); !errors.Is(err, problem.ErrNotFound) {
		t.Fatalf("unknown grant = %v, want 404", err)
	}
}

// TestShareContact_PrivacyOffStopsEveryShare_NoRevival (BE-089): turning
// show_contact_on_grant off withdraws the consent and stops every share in
// the same transaction; turning it on again shares with nobody (a share
// never outlives the consent it was given under); a new acceptance shares
// with its company alone.
func TestShareContact_PrivacyOffStopsEveryShare_NoRevival(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	orgA, recA, _ := e.company("Acme")
	orgB, recB, _ := e.company("Beta")
	s, req := e.granted(t, orgA)
	if err := e.svc.Grant(ctx, s.UserID, orgB, students.SourceContactRequest, uuid.NewV7()); err != nil {
		t.Fatal(err)
	}
	for _, org := range []uuid.UUID{orgA, orgB} {
		if err := e.svc.ShareContactWithCompany(ctx, s, org, *proof(), consent.ChannelWeb, req); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.svc.UpdateMyPrivacy(ctx, s, students.PrivacyPatch{ShowContactOnGrant: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	if e.sharedWith(t, recA, orgA, s) || e.sharedWith(t, recB, orgB, s) || e.grantOf(t, s, orgA).ContactSharedAt != nil ||
		e.grantOf(t, s, orgB).ContactSharedAt != nil {
		t.Fatal("turning contact sharing off left a share")
	}
	if n := len(e.audit.Of(authz.StudentsAccessGrantRevoke)); n != 2 {
		t.Fatalf("unshare audit records = %d, want 2", n)
	}
	if _, err := e.svc.UpdateMyPrivacy(ctx, s, students.PrivacyPatch{ShowContactOnGrant: ptr(true), ContactConsent: proof()}); err != nil {
		t.Fatal(err)
	}
	if e.sharedWith(t, recA, orgA, s) || e.sharedWith(t, recB, orgB, s) {
		t.Fatal("a new consent revived an old share")
	}
	if err := e.svc.ShareContactWithCompany(ctx, s, orgB, *proof(), consent.ChannelWeb, uuid.NewV7()); err != nil {
		t.Fatal(err)
	}
	if e.sharedWith(t, recA, orgA, s) || !e.sharedWith(t, recB, orgB, s) {
		t.Fatal("a new share must reach its company alone")
	}
}

// TestShareContact_ConsentWithdrawnElsewhere (BE-089): a contact_info_share
// withdrawal recorded by the consent module hides the e-mail at once (the
// consent is read at every view) and, through consent.recorded
// (students.clear_contact_shares), clears the shares given before it, so a
// consent given later revives none. A share given while the consent is not
// in force clears the stale ones first.
func TestShareContact_ConsentWithdrawnElsewhere(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	orgA, recA, _ := e.company("Acme")
	orgB, _, _ := e.company("Beta")
	s, req := e.granted(t, orgA)
	if err := e.svc.Grant(ctx, s.UserID, orgB, students.SourceContactRequest, uuid.NewV7()); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.ShareContactWithCompany(ctx, s, orgA, *proof(), consent.ChannelWeb, req); err != nil {
		t.Fatal(err)
	}
	e.consent.mu.Lock()
	e.consent.removed[s.UserID] = true // withdrawn in the consent module
	e.consent.mu.Unlock()
	if e.sharedWith(t, recA, orgA, s) {
		t.Fatal("the e-mail outlived the consent")
	}
	// Another purpose's record changes nothing.
	e.deliver(t, students.ClearContactSharesTaskName, domainevent.ConsentRecorded{ConsentID: uuid.NewV7(), UserID: s.UserID,
		Purpose: string(consent.PurposeDiscoveryProfile), Action: string(consent.Withdrawn)}, uuid.NewV7())
	if e.grantOf(t, s, orgA).ContactSharedAt == nil {
		t.Fatal("a discovery_profile withdrawal cleared the contact share")
	}
	e.deliver(t, students.ClearContactSharesTaskName, domainevent.ConsentRecorded{ConsentID: uuid.NewV7(), UserID: s.UserID,
		Purpose: string(consent.PurposeContactInfoShare), Action: string(consent.Withdrawn)}, uuid.NewV7())
	if e.grantOf(t, s, orgA).ContactSharedAt != nil {
		t.Fatal("the withdrawal did not clear the share")
	}
	e.consent.mu.Lock()
	delete(e.consent.removed, s.UserID) // consent given again
	e.consent.mu.Unlock()
	if e.sharedWith(t, recA, orgA, s) {
		t.Fatal("a new consent revived the old share")
	}

	// Withdrawn elsewhere, then a share before the task ran: the stale
	// share goes first.
	if err := e.svc.ShareContactWithCompany(ctx, s, orgA, *proof(), consent.ChannelWeb, req); err != nil {
		t.Fatal(err)
	}
	e.consent.mu.Lock()
	e.consent.removed[s.UserID] = true
	e.consent.mu.Unlock()
	e.clk.Advance(time.Minute)
	if err := e.svc.ShareContactWithCompany(ctx, s, orgB, *proof(), consent.ChannelWeb, uuid.NewV7()); err != nil {
		t.Fatal(err)
	}
	if e.grantOf(t, s, orgA).ContactSharedAt != nil || e.grantOf(t, s, orgB).ContactSharedAt == nil {
		t.Fatal("a share under a new consent kept the stale one")
	}
}

// TestShareContactWithCompany_Refusals (03 k9): a minor never shares (422
// students.minor_restricted); without an active grant of the company the
// share is not found; a consent record the consent module refuses fails
// the sharing and nothing is shared (the PostgreSQL rollback:
// TestShareContactWithCompany_ConsentFailureRollsBack_Postgres).
func TestShareContactWithCompany_Refusals(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	orgA, _, _ := e.company("Acme")
	m := e.student(t, e.uniA, true)
	err := e.svc.ShareContactWithCompany(ctx, m, orgA, *proof(), consent.ChannelWeb, uuid.NewV7())
	wantCode(t, err, "students.minor_restricted")
	s := e.profiled(t)
	if err := e.svc.ShareContactWithCompany(ctx, s, orgA, *proof(), consent.ChannelWeb, uuid.NewV7()); !errors.Is(err, students.ErrNotFound) {
		t.Fatalf("no grant = %v, want not found", err)
	}
	if err := e.svc.Grant(ctx, s.UserID, orgA, students.SourceContactRequest, uuid.NewV7()); err != nil {
		t.Fatal(err)
	}
	e.consent.mu.Lock()
	e.consent.fail = consent.ErrTextMismatch
	e.consent.mu.Unlock()
	if err := e.svc.ShareContactWithCompany(ctx, s, orgA, *proof(), consent.ChannelWeb, uuid.NewV7()); !errors.Is(err, consent.ErrTextMismatch) {
		t.Fatalf("refused consent = %v", err)
	}
	if len(e.consent.records()) != 0 || len(e.audit.Of("students.access_grant.share_contact")) != 0 {
		t.Fatal("a refused sharing wrote")
	}
}

// TestStore_GrantContacts_Mem pins the per-company sharing store calls on
// the in-memory store (the PostgreSQL twin: TestStore_GrantContacts_Gorm).
func TestStore_GrantContacts_Mem(t *testing.T) {
	t.Parallel()
	grantContactsContract(t, students.NewMemStore(func() time.Time { return t0 }), memWorld())
}

// grantContactsContract: a share is set once on an open grant, never before
// the grant (profile_access_grants_contact_shared_chk), taken back by its
// student only, cleared all at once or up to a time (the old values
// returned for the audit); a revoked grant cannot share.
func grantContactsContract(t *testing.T, s students.Store, w world) {
	t.Helper()
	ctx := t.Context()
	u, v := w.user(t), w.user(t)
	c1, c2 := w.company(t), w.company(t)
	grant := func(student, company uuid.UUID) students.AccessGrant {
		t.Helper()
		g, ok, err := s.InsertGrant(ctx, students.AccessGrant{ID: uuid.NewV7(), StudentUserID: student, CompanyOrgID: company,
			Source: students.SourceContactRequest, SourceID: uuid.NewV7(), GrantedAt: t0})
		if err != nil || !ok {
			t.Fatalf("InsertGrant: ok=%v err=%v", ok, err)
		}
		return g
	}
	g1, g2, g3 := grant(u, c1), grant(u, c2), grant(v, c1)
	if g1.ContactSharedAt != nil {
		t.Fatalf("a new grant shares: %+v", g1)
	}
	if _, err := s.ShareGrantContact(ctx, g1.ID, t0.Add(-time.Second)); !isInvalid(err) {
		t.Fatalf("share before the grant = %v, want 422 (CHECK)", err)
	}
	at1, at2 := t0.Add(5*time.Minute), t0.Add(6*time.Minute)
	if ok, err := s.ShareGrantContact(ctx, g1.ID, at1); err != nil || !ok {
		t.Fatalf("share = %v, %v", ok, err)
	}
	if ok, err := s.ShareGrantContact(ctx, g1.ID, at2); err != nil || ok {
		t.Fatalf("share again = %v, %v (want false)", ok, err)
	}
	if ok, err := s.ShareGrantContact(ctx, g2.ID, at2); err != nil || !ok {
		t.Fatalf("share g2 = %v, %v", ok, err)
	}
	if ok, err := s.ShareGrantContact(ctx, g3.ID, at1); err != nil || !ok {
		t.Fatalf("share g3 = %v, %v", ok, err)
	}
	if got, err := s.GetGrant(ctx, u, g1.ID); err != nil || got.ContactSharedAt == nil || !got.ContactSharedAt.Equal(at1) {
		t.Fatalf("stored = %+v, %v", got, err)
	}
	if ok, err := s.UnshareGrantContact(ctx, v, g1.ID); err != nil || ok {
		t.Fatalf("another student's unshare = %v, %v", ok, err)
	}
	if ok, err := s.UnshareGrantContact(ctx, u, g1.ID); err != nil || !ok {
		t.Fatalf("unshare = %v, %v", ok, err)
	}
	if ok, err := s.UnshareGrantContact(ctx, u, g1.ID); err != nil || ok {
		t.Fatalf("unshare again = %v, %v", ok, err)
	}
	at3 := t0.Add(7 * time.Minute)
	if ok, err := s.ShareGrantContact(ctx, g1.ID, at3); err != nil || !ok {
		t.Fatalf("share again after unshare = %v, %v", ok, err)
	}
	cut := at2
	cleared, err := s.ClearGrantContacts(ctx, u, &cut)
	if err != nil || len(cleared) != 1 || cleared[0].ID != g2.ID || cleared[0].ContactSharedAt == nil ||
		!cleared[0].ContactSharedAt.Equal(at2) {
		t.Fatalf("clear up to %s = %+v, %v", cut, cleared, err)
	}
	if cleared, err = s.ClearGrantContacts(ctx, u, nil); err != nil || len(cleared) != 1 || cleared[0].ID != g1.ID {
		t.Fatalf("clear all = %+v, %v", cleared, err)
	}
	if got, _ := s.GetGrant(ctx, v, g3.ID); got.ContactSharedAt == nil {
		t.Fatal("another student's share was cleared")
	}
	if _, err := s.RevokeGrants(ctx, u, c2, students.RevokedByStudent, nil, t0.Add(8*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ShareGrantContact(ctx, g2.ID, t0.Add(9*time.Minute)); err != nil || ok {
		t.Fatalf("share a revoked grant = %v, %v", ok, err)
	}
}

// deliverAt hands a consent.recorded event written at `at` to
// students.clear_contact_shares.
func (e *env) deliverAt(t *testing.T, p domainevent.ConsentRecorded, at time.Time) {
	t.Helper()
	ev, err := domainevent.New(p, "consent", p.ConsentID)
	if err != nil {
		t.Fatal(err)
	}
	ev.ID, ev.OccurredAt = uuid.NewV7(), at
	body, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range e.subs.For(domainevent.TopicConsentRecorded) {
		if s.Name == students.ClearContactSharesTaskName {
			if err := s.HandleTask(t.Context(), body); err != nil {
				t.Fatalf("%s: %v", s.Name, err)
			}
			return
		}
	}
	t.Fatalf("no subscription %s", students.ClearContactSharesTaskName)
}

// setConsent marks s's consents as withdrawn elsewhere (off) or given
// again (on) in the fake ledger.
func (e *env) setConsent(s authz.Actor, on bool) {
	e.consent.mu.Lock()
	defer e.consent.mu.Unlock()
	if on {
		delete(e.consent.removed, s.UserID)
	} else {
		e.consent.removed[s.UserID] = true
	}
}

// TestClearContactShares_FollowsTheConsentInForce (BE-104, T-355): the
// withdrawal task reads the consent under the student's grant lock. Still
// not in force: every share goes, also one written after the event's time
// (given concurrently with the withdrawal) — a share never outlives the
// consent it was given under. Given again before the task ran: the shares
// of the withdrawn consent go, the share given under the new consent stays
// (a late redelivery too), so the new consent revives nothing old.
func TestClearContactShares_FollowsTheConsentInForce(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	orgA, recA, _ := e.company("Acme")
	orgB, recB, _ := e.company("Beta")
	s, req := e.granted(t, orgA)
	if err := e.svc.Grant(ctx, s.UserID, orgB, students.SourceContactRequest, uuid.NewV7()); err != nil {
		t.Fatal(err)
	}
	withdrawal := domainevent.ConsentRecorded{ConsentID: uuid.NewV7(), UserID: s.UserID,
		Purpose: string(consent.PurposeContactInfoShare), Action: string(consent.Withdrawn)}

	// Concurrent: the withdrawal's event is older than the share.
	e.clk.Advance(time.Minute)
	if err := e.svc.ShareContactWithCompany(ctx, s, orgA, *proof(), consent.ChannelWeb, req); err != nil {
		t.Fatal(err)
	}
	e.setConsent(s, false)
	e.deliverAt(t, withdrawal, e.clk.Now().Add(-time.Second))
	if g := e.grantOf(t, s, orgA); g.ContactSharedAt != nil {
		t.Fatalf("a share outlived the withdrawn consent: %+v", g)
	}
	if ev := e.audit.Of(authz.StudentsAccessGrantRevoke); len(ev) != 1 || ev[0].Metadata["by"] != "consent_withdrawn" ||
		ev[0].SystemJob != "students.contact_shares" || *ev[0].OrgID != orgA {
		t.Fatalf("unshare audit = %+v", ev)
	}

	// Shared with A, withdrawn, given again (a share with B under the new
	// consent) before the task ran.
	e.setConsent(s, true)
	e.clk.Advance(time.Minute)
	if err := e.svc.ShareContactWithCompany(ctx, s, orgA, *proof(), consent.ChannelWeb, req); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(time.Minute)
	withdrawnAt := e.clk.Now()
	e.setConsent(s, false)
	e.clk.Advance(time.Minute)
	e.setConsent(s, true)
	if err := e.svc.ShareContactWithCompany(ctx, s, orgB, *proof(), consent.ChannelWeb, uuid.NewV7()); err != nil {
		t.Fatal(err)
	}
	for range 2 { // at least once
		e.deliverAt(t, withdrawal, withdrawnAt)
		if e.grantOf(t, s, orgA).ContactSharedAt != nil || e.grantOf(t, s, orgB).ContactSharedAt == nil {
			t.Fatalf("after the withdrawal task: A %+v, B %+v", e.grantOf(t, s, orgA), e.grantOf(t, s, orgB))
		}
		if e.sharedWith(t, recA, orgA, s) || !e.sharedWith(t, recB, orgB, s) {
			t.Fatal("the new consent must show the e-mail to B alone")
		}
	}
	// A grant, another purpose: nothing changes.
	for _, p := range []domainevent.ConsentRecorded{
		{ConsentID: uuid.NewV7(), UserID: s.UserID, Purpose: string(consent.PurposeContactInfoShare), Action: string(consent.Granted)},
		{ConsentID: uuid.NewV7(), UserID: s.UserID, Purpose: string(consent.PurposeCommercialEmail), Action: string(consent.Withdrawn)},
	} {
		e.setConsent(s, false)
		e.deliverAt(t, p, e.clk.Now())
		if e.grantOf(t, s, orgB).ContactSharedAt == nil {
			t.Fatalf("%s %s cleared a share", p.Purpose, p.Action)
		}
	}
}

// TestReconcilePrivacy_ClearsContactShares (BE-104): the nightly refresh
// that turns show_contact_on_grant off for a consent no longer in force
// (its consent.recorded lost, or legacy rows of an earlier withdrawal)
// also stops every share, as the student's own switch does; a consent
// given later revives none.
func TestReconcilePrivacy_ClearsContactShares(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	orgA, recA, _ := e.company("Acme")
	s, req := e.granted(t, orgA)
	if err := e.svc.ShareContactWithCompany(ctx, s, orgA, *proof(), consent.ChannelWeb, req); err != nil {
		t.Fatal(err)
	}
	e.setConsent(s, false) // withdrawn through the consent ledger, no event delivered
	e.job(t, students.AudienceRefreshJobName)
	if p, _ := e.svc.GetMyPrivacy(ctx, s); p.ShowContactOnGrant {
		t.Fatal("the mirror stays on without the consent")
	}
	if g := e.grantOf(t, s, orgA); g.ContactSharedAt != nil {
		t.Fatalf("the share outlived the consent: %+v", g)
	}
	ev := e.audit.Of(authz.StudentsAccessGrantRevoke)
	if len(ev) != 1 || ev[0].Metadata["by"] != "consent_not_in_force" || ev[0].SystemJob != students.AudienceRefreshJobName ||
		ev[0].Metadata["contact_shared_at"] == nil {
		t.Fatalf("unshare audit = %+v", ev)
	}
	e.setConsent(s, true)
	if e.sharedWith(t, recA, orgA, s) {
		t.Fatal("a new consent revived the old share")
	}
}
