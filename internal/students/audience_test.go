package students_test

import (
	"errors"
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/platform/domainevent"
	"koro/internal/students"
)

// basics writes department, class year and expected graduation.
func (e *env) basics(t *testing.T, a authz.Actor, dept uuid.UUID, year int16, grad time.Time) students.Profile {
	t.Helper()
	g := ym(grad.Year(), grad.Month())
	if _, err := e.svc.UpdateMyProfile(t.Context(), a, students.ProfilePatch{Department: &dept, ClassYear: &year,
		ExpectedGraduation: &g}); err != nil {
		t.Fatal(err)
	}
	return e.profile(t, a.UserID)
}

func keysOf(e *env, uni, dept uuid.UUID, year string, clubs ...uuid.UUID) []string {
	keys := []string{"all", "uni:" + uni.String(), "dept:" + dept.String(), "year:" + year}
	for _, c := range clubs {
		keys = append(keys, students.ClubKey(c))
	}
	slices.Sort(keys)
	return keys
}

// TestAudienceKeys_ExcludeSensitiveClubs (02 §9.1, 03 Y2): the stored keys
// hold the profile keys and the NON-sensitive clubs only; a sensitive club
// membership never reaches audience_keys, whatever path computed them.
func TestAudienceKeys_ExcludeSensitiveClubs(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.student(t, e.uniA, false)
	e.orgs.join(a.UserID, e.clubPlain, authz.MemberMember)
	e.orgs.join(a.UserID, e.clubSens, authz.MemberClubAdmin)
	p := e.basics(t, a, e.deptA, 2, t0.AddDate(2, 0, 0))
	if want := keysOf(e, e.uniA, e.deptA, "2", e.clubPlain); !slices.Equal(p.AudienceKeys, want) || !p.AudienceEligible {
		t.Fatalf("keys = %v eligible=%v, want %v", p.AudienceKeys, p.AudienceEligible, want)
	}
	// The event path and the nightly path agree.
	e.deliver(t, students.AddClubKeyTaskName, domainevent.MembershipStarted{MembershipID: uuid.NewV7(), OrgID: e.clubSens,
		OrgKind: "club", UserID: a.UserID, Role: "club_admin"}, e.clubSens)
	e.job(t, students.AudienceRefreshJobName)
	if p := e.profile(t, a.UserID); slices.Contains(p.AudienceKeys, students.ClubKey(e.clubSens)) {
		t.Fatalf("sensitive club key stored: %v", p.AudienceKeys)
	}
}

// TestAudienceKeys_MinorNotEligible (03 Y4, agenda §6): a minor's profile
// is never audience eligible (not counted, not targeted, not discoverable);
// turning 18 makes it eligible at the nightly refresh.
func TestAudienceKeys_MinorNotEligible(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	m := e.student(t, e.uniA, true)
	p := e.basics(t, m, e.deptA, 1, t0.AddDate(3, 0, 0))
	if p.AudienceEligible {
		t.Fatal("a minor is audience eligible")
	}
	ap, err := e.svc.AudienceProfile(ctx, m.UserID)
	if err != nil || ap.Eligible || !ap.IsMinor || ap.UniversityID != e.uniA {
		t.Fatalf("AudienceProfile = %+v, %v", ap, err)
	}
	c, err := e.svc.Cohorts(ctx, []uuid.UUID{m.UserID, uuid.NewV7()})
	if err != nil || len(c) != 1 || !c[m.UserID].IsMinor {
		t.Fatalf("Cohorts = %+v, %v", c, err)
	}
	e.users.setMinor(m.UserID, false) // birthday
	e.job(t, students.AudienceRefreshJobName)
	if !e.profile(t, m.UserID).AudienceEligible {
		t.Fatal("not eligible after turning 18")
	}
}

// TestViewerKeys_IncludeSensitiveClubForReadOnly (01 §4.9): the feed's
// viewer keys add every active club at read time, sensitive clubs
// included (a club's posts reach its own members), and are never stored.
// A user without a profile still has 'all' and the club keys.
func TestViewerKeys_IncludeSensitiveClubForReadOnly(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	a := e.student(t, e.uniA, false)
	e.orgs.join(a.UserID, e.clubPlain, authz.MemberMember)
	e.orgs.join(a.UserID, e.clubSens, authz.MemberMember)
	e.basics(t, a, e.deptA, 3, t0.AddDate(1, 0, 0))
	vk, err := e.svc.ViewerKeys(ctx, a.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if want := keysOf(e, e.uniA, e.deptA, "3", e.clubPlain, e.clubSens); !slices.Equal(vk.Keys, want) {
		t.Fatalf("viewer keys = %v, want %v", vk.Keys, want)
	}
	if p := e.profile(t, a.UserID); slices.Contains(p.AudienceKeys, students.ClubKey(e.clubSens)) {
		t.Fatal("the sensitive key leaked into the stored audience")
	}
	nobody := uuid.NewV7()
	e.orgs.join(nobody, e.clubSens, authz.MemberMember)
	vk, err = e.svc.ViewerKeys(ctx, nobody)
	if err != nil || !slices.Equal(vk.Keys, []string{"all", students.ClubKey(e.clubSens)}) {
		t.Fatalf("viewer keys without a profile = %v, %v", vk.Keys, err)
	}
}

// TestClubSensitivityChanged_RemovesKey (T-80): when a club becomes
// sensitive its key leaves every stored profile; when the flag is cleared
// the club's members get it back. Idempotent.
func TestClubSensitivityChanged_RemovesKey(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	club := e.clubPlain
	var members []uuid.UUID
	for range 3 {
		a := e.student(t, e.uniA, false)
		e.orgs.join(a.UserID, club, authz.MemberMember)
		e.basics(t, a, e.deptA, 2, t0.AddDate(2, 0, 0))
		members = append(members, a.UserID)
	}
	for _, u := range members {
		if !slices.Contains(e.profile(t, u).AudienceKeys, students.ClubKey(club)) {
			t.Fatal("setup: club key missing")
		}
	}
	e.catalog.setSensitive(club, true)
	for range 2 { // a redelivery changes nothing
		e.deliver(t, students.DropSensitiveClubKeyTaskName, domainevent.ClubSensitivityChanged{ClubOrgID: club, Sensitive: true}, club)
	}
	for _, u := range members {
		if p := e.profile(t, u); slices.Contains(p.AudienceKeys, students.ClubKey(club)) || !p.AudienceEligible {
			t.Fatalf("after the flag: %v eligible=%v", p.AudienceKeys, p.AudienceEligible)
		}
	}
	e.catalog.setSensitive(club, false)
	e.deliver(t, students.DropSensitiveClubKeyTaskName, domainevent.ClubSensitivityChanged{ClubOrgID: club, Sensitive: false}, club)
	for _, u := range members {
		if !slices.Contains(e.profile(t, u).AudienceKeys, students.ClubKey(club)) {
			t.Fatal("the key did not come back after the flag was cleared")
		}
	}
}

// TestMembershipEnded_RemovesClubKey (01 §6.2): membership.ended of a club
// removes its key; another org kind is ignored; a second active
// membership of the same club (a role change) keeps it.
func TestMembershipEnded_RemovesClubKey(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.student(t, e.uniA, false)
	e.basics(t, a, e.deptA, 2, t0.AddDate(2, 0, 0))
	e.orgs.join(a.UserID, e.clubPlain, authz.MemberMember)
	started := domainevent.MembershipStarted{MembershipID: uuid.NewV7(), OrgID: e.clubPlain, OrgKind: "club", UserID: a.UserID, Role: "member"}
	e.deliver(t, students.AddClubKeyTaskName, started, e.clubPlain)
	if !slices.Contains(e.profile(t, a.UserID).AudienceKeys, students.ClubKey(e.clubPlain)) {
		t.Fatal("membership.started did not add the key")
	}
	ended := domainevent.MembershipEnded{MembershipID: started.MembershipID, OrgID: e.clubPlain, OrgKind: "club",
		UserID: a.UserID, Role: "member", Reason: "left"}
	company := uuid.NewV7()
	e.deliver(t, students.RemoveClubKeyTaskName, domainevent.MembershipEnded{MembershipID: uuid.NewV7(), OrgID: company,
		OrgKind: "company", UserID: a.UserID, Role: "recruiter", Reason: "left"}, company)
	e.deliver(t, students.RemoveClubKeyTaskName, ended, e.clubPlain) // still an active member: kept
	if !slices.Contains(e.profile(t, a.UserID).AudienceKeys, students.ClubKey(e.clubPlain)) {
		t.Fatal("the key went while the membership is still active")
	}
	e.orgs.leave(a.UserID, e.clubPlain)
	e.deliver(t, students.RemoveClubKeyTaskName, ended, e.clubPlain)
	if slices.Contains(e.profile(t, a.UserID).AudienceKeys, students.ClubKey(e.clubPlain)) {
		t.Fatal("membership.ended did not remove the key")
	}
}

// TestEduLapsed_AudienceIneligible (01 §6.2): a lapsed verification takes
// the profile out of the audience; a renewal brings it back.
func TestEduLapsed_AudienceIneligible(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.student(t, e.uniA, false)
	if !e.basics(t, a, e.deptA, 2, t0.AddDate(2, 0, 0)).AudienceEligible {
		t.Fatal("setup: not eligible")
	}
	e.users.lapse(a.UserID)
	e.deliver(t, students.ApplyEduLapsedTaskName, domainevent.UserEduVerificationLapsed{UserID: a.UserID, VerificationID: uuid.NewV7()}, a.UserID)
	if e.profile(t, a.UserID).AudienceEligible {
		t.Fatal("eligible after the verification lapsed")
	}
	e.users.verify(a.UserID, e.uniA, t0)
	e.deliver(t, students.ApplyEduVerifiedTaskName, domainevent.UserEduVerified{UserID: a.UserID, UniversityID: e.uniA,
		ExpiresAt: t0.AddDate(1, 0, 0), Renewal: true}, a.UserID)
	if !e.profile(t, a.UserID).AudienceEligible {
		t.Fatal("not eligible after the renewal")
	}
}

// TestEduVerified_CreatesProfile: a newly verified student without a
// profile gets one (university of the verification) so that 'all' and
// 'uni:' targeting reach them; a redelivery changes nothing.
func TestEduVerified_CreatesProfile(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.student(t, e.uniB, false)
	ev := domainevent.UserEduVerified{UserID: a.UserID, UniversityID: e.uniB, ExpiresAt: t0.AddDate(1, 0, 0)}
	e.deliver(t, students.ApplyEduVerifiedTaskName, ev, a.UserID)
	p := e.profile(t, a.UserID)
	if p.UniversityID != e.uniB || !p.AudienceEligible || !slices.Equal(p.AudienceKeys, []string{"all", "uni:" + e.uniB.String()}) {
		t.Fatalf("profile = %+v", p)
	}
	e.deliver(t, students.ApplyEduVerifiedTaskName, ev, a.UserID)
	if q := e.profile(t, a.UserID); !q.UpdatedAt.Equal(p.UpdatedAt) {
		t.Fatal("a redelivery rewrote the profile")
	}
}

// TestGraduationCheck_EmitsGraduated (01 §7): an active profile whose
// expected graduation month is over and whose verification was not
// renewed since is graduated (out of the audience) and student.graduated
// is written in the transaction; a renewed student, a profile graduating
// this month and a profile without a date stay; a later verification
// re-activates (T-56).
func TestGraduationCheck_EmitsGraduated(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	// Today is 2026-10-04.
	gone := e.student(t, e.uniA, false)
	e.basics(t, gone, e.deptA, 4, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	e.users.verify(gone.UserID, e.uniA, time.Date(2025, 9, 15, 9, 0, 0, 0, time.UTC)) // still valid, not renewed since June
	renewed := e.student(t, e.uniA, false)
	e.basics(t, renewed, e.deptA, 4, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	e.users.verify(renewed.UserID, e.uniA, time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC))
	thisMonth := e.student(t, e.uniA, false)
	e.basics(t, thisMonth, e.deptA, 4, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	undated := e.student(t, e.uniA, false)
	if _, err := e.svc.UpdateMyProfile(ctx, undated, students.ProfilePatch{Headline: ptr("Tarihsiz")}); err != nil {
		t.Fatal(err)
	}

	e.job(t, students.GraduationCheckJobName)
	e.job(t, students.GraduationCheckJobName) // idempotent

	p := e.profile(t, gone.UserID)
	if p.Status != students.ProfileGraduated || p.GraduatedAt == nil || p.AudienceEligible {
		t.Fatalf("graduate = %+v", p)
	}
	var graduated []uuid.UUID
	for _, ev := range e.out.Events() {
		if ev.Topic == domainevent.TopicStudentGraduated {
			g, err := domainevent.Decode[domainevent.StudentGraduated](ev)
			if err != nil {
				t.Fatal(err)
			}
			graduated = append(graduated, g.UserID)
		}
	}
	if !slices.Equal(graduated, []uuid.UUID{gone.UserID}) {
		t.Fatalf("student.graduated for %v", graduated)
	}
	for _, u := range []uuid.UUID{renewed.UserID, thisMonth.UserID, undated.UserID} {
		if e.profile(t, u).Status != students.ProfileActive {
			t.Fatalf("%v graduated", u)
		}
	}
	if got, err := e.svc.GraduatedAmong(ctx, []uuid.UUID{gone.UserID, renewed.UserID}); err != nil || !got[gone.UserID] || got[renewed.UserID] {
		t.Fatalf("GraduatedAmong = %v, %v", got, err)
	}
	if len(e.audit.of("students.profile.graduate")) != 1 {
		t.Fatal("graduation not audited")
	}
	// The graduate verifies again (e.g. a master's): active again.
	e.users.verify(gone.UserID, e.uniA, t0)
	e.deliver(t, students.ApplyEduVerifiedTaskName, domainevent.UserEduVerified{UserID: gone.UserID, UniversityID: e.uniA,
		ExpiresAt: t0.AddDate(1, 0, 0)}, gone.UserID)
	if p := e.profile(t, gone.UserID); p.Status != students.ProfileActive || p.GraduatedAt != nil || !p.AudienceEligible {
		t.Fatalf("after a new verification = %+v", p)
	}
	e.job(t, students.GraduationCheckJobName)
	if e.profile(t, gone.UserID).Status != students.ProfileActive {
		t.Fatal("graduated again despite the renewal")
	}
}

// TestAudienceRefresh_FixesDrift (01 §7): the nightly job repairs stored
// audiences that drifted from the facts (a missed membership event, a
// sensitive key that slipped in, a lapsed verification, a moved
// university, a missing refresh stamp) and turns off mirrored flags whose
// consent is no longer in force or that a minor holds.
func TestAudienceRefresh_FixesDrift(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	a := e.student(t, e.uniA, false)
	b := e.student(t, e.uniA, false)
	c := e.student(t, e.uniA, false)
	for _, x := range []authz.Actor{a, b, c} {
		e.basics(t, x, e.deptA, 2, t0.AddDate(2, 0, 0))
	}
	// a: a membership whose event was lost; a sensitive key written by hand.
	e.orgs.join(a.UserID, e.clubPlain, authz.MemberMember)
	pa := e.profile(t, a.UserID)
	pa.AudienceKeys = append(pa.AudienceKeys, students.ClubKey(e.clubSens))
	if _, err := e.store.UpdateProfile(ctx, pa); err != nil {
		t.Fatal(err)
	}
	// b: the verification lapsed without an event.
	e.users.lapse(b.UserID)
	// c: verified at another university now.
	e.users.verify(c.UserID, e.uniB, t0)

	// Mirrored flags: a turned discoverable on, then withdrew the consent
	// through the consent ledger; c holds contact sharing with consent.
	if _, err := e.svc.UpdateMyPrivacy(ctx, a, students.PrivacyPatch{Discoverable: ptr(true), DiscoveryConsent: proof()}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.UpdateMyPrivacy(ctx, c, students.PrivacyPatch{ShowContactOnGrant: ptr(true), ContactConsent: proof()}); err != nil {
		t.Fatal(err)
	}
	e.consent.mu.Lock()
	e.consent.removed[a.UserID] = true
	e.consent.mu.Unlock()

	e.job(t, students.AudienceRefreshJobName)

	if p := e.profile(t, a.UserID); !slices.Equal(p.AudienceKeys, keysOf(e, e.uniA, e.deptA, "2", e.clubPlain)) || !p.AudienceEligible {
		t.Fatalf("a = %v eligible=%v", p.AudienceKeys, p.AudienceEligible)
	}
	if e.profile(t, b.UserID).AudienceEligible {
		t.Fatal("b is still eligible without a verification")
	}
	if p := e.profile(t, c.UserID); p.UniversityID != e.uniB || p.DepartmentID != nil || !p.AudienceEligible ||
		!slices.Equal(p.AudienceKeys, []string{"all", "uni:" + e.uniB.String(), "year:2"}) {
		t.Fatalf("c = %+v", p)
	}
	if pr, _ := e.svc.GetMyPrivacy(ctx, a); pr.Discoverable {
		t.Fatal("a stays discoverable without the consent")
	}
	if pr, _ := e.svc.GetMyPrivacy(ctx, c); !pr.ShowContactOnGrant {
		t.Fatal("c lost contact sharing despite the consent")
	}
	if len(e.audit.of("students.privacy.reconcile")) != 1 || len(e.audit.of("students.profile.follow_verification")) != 1 {
		t.Fatalf("reconcile audit = %d, university audit = %d",
			len(e.audit.of("students.privacy.reconcile")), len(e.audit.of("students.profile.follow_verification")))
	}
	// A second run finds nothing to fix.
	before := len(e.audit.events)
	pc := e.profile(t, c.UserID)
	e.job(t, students.AudienceRefreshJobName)
	if len(e.audit.events) != before || !e.profile(t, c.UserID).UpdatedAt.Equal(pc.UpdatedAt) {
		t.Fatal("the second refresh changed something")
	}
}

// TestRefresh_UnknownAccountFailsClosed: a profile whose account identity
// does not know (or no longer active) is not eligible.
func TestRefresh_UnknownAccountFailsClosed(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.student(t, e.uniA, false)
	e.basics(t, a, e.deptA, 1, t0.AddDate(3, 0, 0))
	e.users.mu.Lock()
	s := e.users.sum[a.UserID]
	s.Status = identity.StatusSuspended
	e.users.sum[a.UserID] = s
	e.users.mu.Unlock()
	e.job(t, students.AudienceRefreshJobName)
	if e.profile(t, a.UserID).AudienceEligible {
		t.Fatal("a suspended account is eligible")
	}
	e.users.mu.Lock()
	delete(e.users.sum, a.UserID)
	e.users.mu.Unlock()
	ap, err := e.svc.AudienceProfile(t.Context(), a.UserID)
	if err != nil || ap.Eligible || !ap.IsMinor {
		t.Fatalf("AudienceProfile of an unknown account = %+v, %v", ap, err)
	}
	if _, err := e.svc.AudienceProfile(t.Context(), uuid.NewV7()); !errors.Is(err, students.ErrNotFound) {
		t.Fatalf("AudienceProfile without a profile: %v", err)
	}
}
