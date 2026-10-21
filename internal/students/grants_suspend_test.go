package students_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/notify"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/problem"
	"koro/internal/students"
)

// event is one outbox delivery of p (its ID and occurrence fixed, so a
// retry is the same event).
func (e *env) event(t *testing.T, p domainevent.Payload, at time.Time) []byte {
	t.Helper()
	env, err := domainevent.New(p, "edu_verification", uuid.NewV7())
	if err != nil {
		t.Fatal(err)
	}
	env.ID, env.OccurredAt = uuid.NewV7(), at
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// handle runs subscription name of topic on body and returns its error.
func (e *env) handle(t *testing.T, topic, name string, body []byte) error {
	t.Helper()
	for _, s := range e.subs.For(topic) {
		if s.Name == name {
			return s.HandleTask(t.Context(), body)
		}
	}
	t.Fatalf("no subscription %s on %s", name, topic)
	return nil
}

// lapse ends a's verification in identity and delivers
// user.edu_verification_lapsed to the grant suspension (as identity.edu_expire
// and the worker would).
func (e *env) lapse(t *testing.T, a authz.Actor) authz.Actor {
	t.Helper()
	e.users.lapse(a.UserID)
	body := e.event(t, domainevent.UserEduVerificationLapsed{UserID: a.UserID, VerificationID: uuid.NewV7()}, e.clk.Now())
	if err := e.handle(t, domainevent.TopicUserEduVerificationLapsed, students.SuspendLapsedGrantsTaskName, body); err != nil {
		t.Fatalf("%s: %v", students.SuspendLapsedGrantsTaskName, err)
	}
	return authz.Actor{Kind: authz.KindUser, UserID: a.UserID, StudentStatus: authz.StudentLapsed}
}

// reverify gives a's account a new verification at uni and delivers
// user.edu_verified (renewal: whether it replaced a verification in
// effect) to both students consumers.
func (e *env) reverify(t *testing.T, a authz.Actor, uni uuid.UUID, renewal bool) {
	t.Helper()
	now := e.clk.Now()
	e.users.verify(a.UserID, uni, now)
	p := domainevent.UserEduVerified{UserID: a.UserID, UniversityID: uni, ExpiresAt: now.AddDate(1, 0, 0), Renewal: renewal}
	body := e.event(t, p, now)
	for _, name := range []string{students.ApplyEduVerifiedTaskName, students.SuspendGapGrantsTaskName} {
		if err := e.handle(t, domainevent.TopicUserEduVerified, name, body); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// companyHidden fails unless every company view of the student is the
// unknown student's 404, the company list leaves the student out and Has
// is false.
func (e *env) companyHidden(t *testing.T, step string, rec authz.Actor, org, student uuid.UUID) {
	t.Helper()
	ctx := t.Context()
	if _, err := e.svc.GetCompanyStudentProfile(ctx, rec, org, student); !errors.Is(err, problem.ErrNotFound) {
		t.Fatalf("%s: profile %v, want 404", step, err)
	}
	if _, err := e.svc.DownloadCompanyStudentCV(ctx, rec, org, student); !errors.Is(err, problem.ErrNotFound) {
		t.Fatalf("%s: CV %v, want 404", step, err)
	}
	p, err := e.svc.ListCompanyStudents(ctx, rec, org, students.CompanyStudentQuery{})
	if err != nil {
		t.Fatalf("%s: company list: %v", step, err)
	}
	for _, c := range p.Items {
		if c.UserID == student.String() {
			t.Fatalf("%s: the company list shows the student", step)
		}
	}
	if ok, err := e.svc.Has(ctx, student, org); err != nil || ok {
		t.Fatalf("%s: Has = %v, %v", step, ok, err)
	}
}

// TestSuspend_LapseCutsCompanyAccess (BE-077): the company loses a
// student the moment the verification ends (read time: profile, CV, list
// and Has answer as for an unknown student), before identity.edu_expire
// records the lapse; the lapse event suspends every active grant (one
// system audit record each, reason only), tells the student once in-app
// (fixed text, no company named, a link to the grant list) and nobody else;
// a redelivery changes nothing; a revoked grant stays revoked.
func TestSuspend_LapseCutsCompanyAccess(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	orgA, recA, _ := e.company("Acme")
	orgB, recB, _ := e.company("Beta")
	orgC, _, _ := e.company("Gamma")
	s, _ := e.granted(t, orgA)
	e.withCV(t, s)
	if err := e.svc.Grant(ctx, s.UserID, orgB, students.SourceContactRequest, uuid.NewV7()); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Grant(ctx, s.UserID, orgC, students.SourceContactRequest, uuid.NewV7()); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.RevokeCompanyAccessGrant(ctx, mustRecruiter(t, e, orgC), orgC, s.UserID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.GetCompanyStudentProfile(ctx, recA, orgA, s.UserID); err != nil {
		t.Fatal(err)
	}
	other, _ := e.granted(t, orgA) // another student of the same company keeps its grant

	// The verification ends: the company views miss the student at once.
	e.users.lapse(s.UserID)
	e.companyHidden(t, "verification ended", recA, orgA, s.UserID)
	if g, err := e.store.ActiveGrant(ctx, s.UserID, orgA); err != nil || !g.Active() {
		t.Fatalf("the grant is suspended before the lapse event: %+v, %v", g, err)
	}

	// identity.edu_expire records the lapse: every active grant is suspended.
	e.clk.Advance(time.Hour)
	at := e.clk.Now()
	body := e.event(t, domainevent.UserEduVerificationLapsed{UserID: s.UserID, VerificationID: uuid.NewV7()}, at)
	if err := e.handle(t, domainevent.TopicUserEduVerificationLapsed, students.SuspendLapsedGrantsTaskName, body); err != nil {
		t.Fatal(err)
	}
	all, _ := e.store.AllGrants(ctx, s.UserID)
	states := map[uuid.UUID]students.GrantStatus{}
	for _, g := range all {
		states[g.CompanyOrgID] = g.Status()
		if g.Status() == students.GrantSuspended && !g.SuspendedAt.Equal(at) {
			t.Fatalf("suspended at %v, want the lapse %v", g.SuspendedAt, at)
		}
	}
	if states[orgA] != students.GrantSuspended || states[orgB] != students.GrantSuspended || states[orgC] != students.GrantRevoked {
		t.Fatalf("grant states = %v", states)
	}
	rows := e.audit.of("students.access_grant.suspend")
	if len(rows) != 2 {
		t.Fatalf("suspension audit rows = %d, want 2", len(rows))
	}
	for _, r := range rows {
		if r.SystemJob != students.SuspendLapsedGrantsTaskName || r.ActorUserID != nil || r.Metadata["reason"] != "verification_lapsed" ||
			r.Metadata["subject_user_id"] != s.UserID.String() {
			t.Fatalf("suspension audit = %+v", r)
		}
	}
	msgs := e.notices.messages()
	if len(msgs) != 1 {
		t.Fatalf("notices = %+v, want one to the student", msgs)
	}
	m := msgs[0]
	if *m.UserID != s.UserID || m.Channel != notify.ChannelInApp || m.Category != notify.CategoryTransactional ||
		m.Template != "students.access_grants_suspended" || m.LinkPath != "/me/access-grants" || len(m.Data) != 0 ||
		m.SourceOrgID != nil || m.DedupeKey == "" {
		t.Fatalf("notice = %+v", m)
	}
	e.companyHidden(t, "suspended", recA, orgA, s.UserID)
	e.companyHidden(t, "suspended (company B)", recB, orgB, s.UserID)
	if ok, err := e.svc.Has(ctx, other.UserID, orgA); err != nil || !ok {
		t.Fatalf("another student's grant: Has = %v, %v", ok, err)
	}

	// A redelivery suspends nothing new; the notice is the same message
	// (notify dedupes on its key).
	if err := e.handle(t, domainevent.TopicUserEduVerificationLapsed, students.SuspendLapsedGrantsTaskName, body); err != nil {
		t.Fatal(err)
	}
	if n := len(e.audit.of("students.access_grant.suspend")); n != 2 {
		t.Fatalf("a redelivery audited again: %d", n)
	}
	if msgs = e.notices.messages(); len(msgs) != 2 || msgs[1].DedupeKey != m.DedupeKey {
		t.Fatalf("redelivered notice = %+v", msgs)
	}
	// A lapse of a student without open grants tells nobody.
	quiet := e.profiled(t)
	e.lapse(t, quiet)
	if n := len(e.notices.messages()); n != 2 {
		t.Fatalf("a student without grants was told: %d notices", n)
	}
}

// mustRecruiter is a recruiter of org (cmm, no MFA).
func mustRecruiter(t *testing.T, _ *env, org uuid.UUID) authz.Actor {
	t.Helper()
	return authz.Actor{Kind: authz.KindUser, UserID: uuid.NewV7(), StudentStatus: authz.StudentNone,
		Memberships: []authz.Membership{{OrgID: org, OrgKind: authz.OrgCompany, Role: authz.MemberRecruiter}}}
}

// TestSuspend_ReverificationDoesNotReopen (BE-077, consent principle): a
// student who verifies again finds the grants still suspended and the
// company still sees nothing; while lapsed no new grant is given (403
// students.verification_required); a new contact request accepted after
// the verification gives a fresh active grant and ends the suspended one
// (by the system, audited).
func TestSuspend_ReverificationDoesNotReopen(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	org, rec, _ := e.company("Acme")
	s, _ := e.granted(t, org)
	e.withCV(t, s)
	lapsed := e.lapse(t, s)
	if err := e.svc.Grant(ctx, s.UserID, org, students.SourceContactRequest, uuid.NewV7()); !errors.Is(err, students.ErrVerificationRequired) {
		t.Fatalf("a grant while lapsed: %v, want students.verification_required", err)
	}
	mine, err := e.svc.ListMyAccessGrants(ctx, lapsed, nil, 0)
	if err != nil || len(mine.Items) != 1 || mine.Items[0].Grant.Status() != students.GrantSuspended {
		t.Fatalf("my grants while lapsed = %+v, %v", mine, err)
	}
	old := mine.Items[0].Grant.ID

	e.clk.Advance(24 * time.Hour)
	e.reverify(t, s, e.uniA, false)
	e.companyHidden(t, "verified again", rec, org, s.UserID)
	if g, err := e.store.GetGrant(ctx, s.UserID, old); err != nil || g.Status() != students.GrantSuspended {
		t.Fatalf("the suspended grant after the new verification = %+v, %v", g, err)
	}
	if n := len(e.audit.of("students.access_grant.suspend")); n != 1 {
		t.Fatalf("suspension audit rows = %d, want 1 (the new verification suspends nothing more)", n)
	}

	// The student accepts a new request of the company.
	e.clk.Advance(time.Hour)
	if err := e.svc.Grant(ctx, s.UserID, org, students.SourceContactRequest, uuid.NewV7()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.GetCompanyStudentProfile(ctx, rec, org, s.UserID); err != nil {
		t.Fatalf("after a new acceptance: %v", err)
	}
	if g, err := e.store.GetGrant(ctx, s.UserID, old); err != nil || g.Status() != students.GrantRevoked ||
		*g.RevokedByKind != students.RevokedBySystem {
		t.Fatalf("the replaced suspended grant = %+v, %v", g, err)
	}
	sys := e.audit.of(string(authz.StudentsAccessGrantRevoke))
	if len(sys) != 1 || sys[0].SystemJob != "students.access_grants" || *sys[0].ResourceID != old {
		t.Fatalf("replacement audit = %+v", sys)
	}
	mine, err = e.svc.ListMyAccessGrants(ctx, s, nil, 0)
	if err != nil || len(mine.Items) != 1 || mine.Items[0].Grant.ID == old || mine.Items[0].Grant.Status() != students.GrantActive {
		t.Fatalf("my grants after the new acceptance = %+v, %v", mine, err)
	}
}

// TestSuspend_LapsedStudentListsAndRevokes (BE-077, KVKK md. 11): a
// student whose verification lapsed lists the suspended grants (status,
// suspended_at) and revokes them without a verification (by the student,
// audited); a second revocation changes nothing; the other own-profile
// actions keep k-uv (403); the company cannot end a suspended grant (404:
// it never learns of the suspension); the system's Revoke (messaging) ends
// a suspended grant too.
func TestSuspend_LapsedStudentListsAndRevokes(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	orgA, recA, _ := e.company("Acme")
	orgB, _, _ := e.company("Beta")
	s, _ := e.granted(t, orgA)
	reqB := uuid.NewV7()
	if err := e.svc.Grant(ctx, s.UserID, orgB, students.SourceContactRequest, reqB); err != nil {
		t.Fatal(err)
	}
	lapsed := e.lapse(t, s)
	if _, err := e.svc.GetMyProfile(ctx, lapsed); !errors.Is(err, students.ErrVerificationRequired) {
		t.Fatalf("own profile while lapsed: %v, want k-uv 403", err)
	}
	mine, err := e.svc.ListMyAccessGrants(ctx, lapsed, nil, 0)
	if err != nil || len(mine.Items) != 2 {
		t.Fatalf("my grants = %+v, %v", mine, err)
	}
	var grantA uuid.UUID
	for _, v := range mine.Items {
		if v.Grant.Status() != students.GrantSuspended || v.Grant.SuspendedAt == nil {
			t.Fatalf("listed grant = %+v", v.Grant)
		}
		if v.CompanyName == "Acme" {
			grantA = v.Grant.ID
		}
	}

	if err := e.svc.RevokeCompanyAccessGrant(ctx, recA, orgA, s.UserID, ""); !errors.Is(err, problem.ErrNotFound) {
		t.Fatalf("the company on a suspended grant: %v, want 404", err)
	}
	if g, _ := e.store.GetGrant(ctx, s.UserID, grantA); g.Status() != students.GrantSuspended {
		t.Fatalf("the company's refused revocation changed the grant: %+v", g)
	}

	if err := e.svc.RevokeMyAccessGrant(ctx, lapsed, grantA, "Mezun oldum"); err != nil {
		t.Fatal(err)
	}
	g, err := e.store.GetGrant(ctx, s.UserID, grantA)
	if err != nil || g.Status() != students.GrantRevoked || *g.RevokedByKind != students.RevokedByStudent {
		t.Fatalf("revoked grant = %+v, %v", g, err)
	}
	rows := e.audit.of(string(authz.StudentsAccessGrantRevoke))
	if len(rows) != 1 || *rows[0].ActorUserID != s.UserID || rows[0].Metadata["revoked_by"] != "student" {
		t.Fatalf("revocation audit = %+v", rows)
	}
	if err := e.svc.RevokeMyAccessGrant(ctx, lapsed, grantA, ""); err != nil {
		t.Fatalf("revoking again: %v", err)
	}
	if n := len(e.audit.of(string(authz.StudentsAccessGrantRevoke))); n != 1 {
		t.Fatalf("a second revocation was audited: %d", n)
	}

	// messaging's Revoke (system) ends company B's suspended grant.
	if err := e.svc.Revoke(ctx, s.UserID, orgB, students.SourceContactRequest, reqB, "Engellendi"); err != nil {
		t.Fatal(err)
	}
	if mine, _ = e.svc.ListMyAccessGrants(ctx, lapsed, nil, 0); len(mine.Items) != 0 {
		t.Fatalf("my grants after both revocations = %+v", mine.Items)
	}
}

// TestSuspend_GapRenewalSuspends (BE-077): a verification renewed after
// its expiry but before identity.edu_expire lapsed it (user.edu_verified,
// renewal false, no lapse event) suspends the grants given before it; a
// renewal of a verification in effect changes nothing; a grant given
// after the event is never suspended by it, however late the task runs.
func TestSuspend_GapRenewalSuspends(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	org, rec, _ := e.company("Acme")
	s, _ := e.granted(t, org)

	e.reverify(t, s, e.uniA, true) // an ordinary renewal
	if _, err := e.svc.GetCompanyStudentProfile(ctx, rec, org, s.UserID); err != nil {
		t.Fatalf("after a renewal in effect: %v", err)
	}

	// The year ran out; the student verified again before the nightly lapse.
	e.users.lapse(s.UserID)
	e.companyHidden(t, "expired, not yet lapsed", rec, org, s.UserID)
	e.clk.Advance(time.Hour)
	at := e.clk.Now()
	e.users.verify(s.UserID, e.uniA, at)
	body := e.event(t, domainevent.UserEduVerified{UserID: s.UserID, UniversityID: e.uniA, ExpiresAt: at.AddDate(1, 0, 0)}, at)

	// A new request accepted after the verification, before the task runs.
	e.clk.Advance(time.Minute)
	other, _, _ := e.company("Beta")
	if err := e.svc.Grant(ctx, s.UserID, other, students.SourceContactRequest, uuid.NewV7()); err != nil {
		t.Fatal(err)
	}
	if err := e.handle(t, domainevent.TopicUserEduVerified, students.SuspendGapGrantsTaskName, body); err != nil {
		t.Fatal(err)
	}
	e.companyHidden(t, "after the gap", rec, org, s.UserID)
	if ok, err := e.svc.Has(ctx, s.UserID, other); err != nil || !ok {
		t.Fatalf("the grant given after the verification: Has = %v, %v", ok, err)
	}
	rows := e.audit.of("students.access_grant.suspend")
	if len(rows) != 1 || rows[0].SystemJob != students.SuspendGapGrantsTaskName || rows[0].Metadata["reason"] != "verification_gap" {
		t.Fatalf("gap suspension audit = %+v", rows)
	}
	if n := len(e.notices.messages()); n != 1 {
		t.Fatalf("notices = %d, want 1", n)
	}
}

// TestSuspend_NoticeRetriedAfterSendFailure: a transient notify failure
// fails the task after the suspension committed; the retry sends the
// notice (same dedupe key) without auditing again; a permanent failure is
// logged and the task succeeds.
func TestSuspend_NoticeRetriedAfterSendFailure(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	org, rec, _ := e.company("Acme")
	s, _ := e.granted(t, org)
	e.users.lapse(s.UserID)
	body := e.event(t, domainevent.UserEduVerificationLapsed{UserID: s.UserID, VerificationID: uuid.NewV7()}, e.clk.Now())

	e.notices.mu.Lock()
	e.notices.fail = notify.ErrDeliveryFailed
	e.notices.mu.Unlock()
	if err := e.handle(t, domainevent.TopicUserEduVerificationLapsed, students.SuspendLapsedGrantsTaskName, body); err == nil {
		t.Fatal("a transient notify failure did not fail the task")
	}
	e.companyHidden(t, "suspended, notice pending", rec, org, s.UserID)
	e.notices.mu.Lock()
	e.notices.fail = nil
	e.notices.mu.Unlock()
	if err := e.handle(t, domainevent.TopicUserEduVerificationLapsed, students.SuspendLapsedGrantsTaskName, body); err != nil {
		t.Fatal(err)
	}
	if n := len(e.notices.messages()); n != 1 {
		t.Fatalf("notices after the retry = %d, want 1", n)
	}
	if n := len(e.audit.of("students.access_grant.suspend")); n != 1 {
		t.Fatalf("suspension audit rows = %d, want 1", n)
	}

	// A permanent failure (the account cannot receive it) does not fail.
	s2, _ := e.granted(t, org)
	e.users.lapse(s2.UserID)
	e.notices.mu.Lock()
	e.notices.fail = notify.ErrDeliveryRejected
	e.notices.mu.Unlock()
	body = e.event(t, domainevent.UserEduVerificationLapsed{UserID: s2.UserID, VerificationID: uuid.NewV7()}, e.clk.Now())
	if err := e.handle(t, domainevent.TopicUserEduVerificationLapsed, students.SuspendLapsedGrantsTaskName, body); err != nil {
		t.Fatalf("a permanent notify failure: %v", err)
	}
}

// TestHTTP_LapsedStudentListsAndRevokesGrants (BE-077, 04 §2.4): over
// HTTP a student whose verification lapsed lists the suspended grant
// (status suspended, suspended_at) and revokes it (204) without a student
// verification; the company's profile and CV paths are 404 throughout; an
// account that never gave a grant lists nothing (200).
func TestHTTP_LapsedStudentListsAndRevokesGrants(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	app := newHTTP(t, e)
	org, rec, _ := e.company("Acme")
	s, _ := e.granted(t, org)
	e.withCV(t, s)
	lapsed := e.lapse(t, s)

	base := "/api/v1/orgs/" + org.String() + "/students/" + s.UserID.String()
	for _, path := range []string{base, base + "/cv"} {
		if r := e.send(t, app, rec, http.MethodGet, path, ""); r.Code != http.StatusNotFound || code(r) != "not_found" {
			t.Fatalf("company %s while suspended = %d %s", path, r.Code, r.Body)
		}
	}
	r := e.send(t, app, lapsed, http.MethodGet, "/api/v1/me/access-grants", "")
	var mine students.StudentAccessGrantList
	if r.Code != http.StatusOK || json.Unmarshal(r.Body.Bytes(), &mine) != nil || len(mine.Items) != 1 ||
		mine.Items[0].Status != "suspended" || mine.Items[0].SuspendedAt == nil || mine.Items[0].Company.Name != "Acme" {
		t.Fatalf("lapsed student's grants = %d %s", r.Code, r.Body)
	}
	if r = e.send(t, app, lapsed, http.MethodPost, "/api/v1/me/access-grants/"+mine.Items[0].GrantID+"/revoke",
		`{"reason":"Mezun oldum"}`); r.Code != http.StatusNoContent {
		t.Fatalf("lapsed student's revocation = %d %s", r.Code, r.Body)
	}
	r = e.send(t, app, lapsed, http.MethodGet, "/api/v1/me/access-grants", "")
	if r.Code != http.StatusOK || json.Unmarshal(r.Body.Bytes(), &mine) != nil || len(mine.Items) != 0 {
		t.Fatalf("grants after the revocation = %d %s", r.Code, r.Body)
	}
	if r = e.send(t, app, rec, http.MethodGet, base, ""); r.Code != http.StatusNotFound {
		t.Fatalf("company profile after the revocation = %d", r.Code)
	}
	if r = e.send(t, app, e.unverified(), http.MethodGet, "/api/v1/me/access-grants", ""); r.Code != http.StatusOK ||
		json.Unmarshal(r.Body.Bytes(), &mine) != nil || len(mine.Items) != 0 {
		t.Fatalf("an account without grants = %d %s", r.Code, r.Body)
	}
}
