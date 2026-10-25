package students_test

import (
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/notify"
	"koro/internal/platform/queue"
	"koro/internal/students"
)

// runJob runs the scheduled job name and returns its error.
func (e *env) runJob(t *testing.T, name string) error {
	t.Helper()
	for _, j := range e.mod.Jobs() {
		if j.Name == name {
			return j.Handle(t.Context())
		}
	}
	t.Fatalf("no job %s", name)
	return nil
}

// expiryNotices returns the expiry notices sent so far to user.
func (e *env) expiryNotices(user uuid.UUID) []notify.Message {
	var out []notify.Message
	for _, m := range e.notices.messages() {
		if m.Template == "students.access_grants_expired" && m.UserID != nil && *m.UserID == user {
			out = append(out, m)
		}
	}
	return out
}

// TestExpire_JobScheduled: students.expire_suspended_grants runs nightly
// at 02:30 on the default queue, once a day (01 §7).
func TestExpire_JobScheduled(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, j := range e.mod.Jobs() {
		if j.Name == students.ExpireSuspendedGrantsJobName {
			if j.Cron != "30 2 * * *" || j.Queue != queue.Default || j.Unique != 23*time.Hour || j.Handle == nil {
				t.Fatalf("job = %+v", j)
			}
			return
		}
	}
	t.Fatal("students.expire_suspended_grants is not scheduled")
}

// TestExpire_AfterWindowRevokedAndStudentTold (BE-088, Q-03/T-331): at
// exactly S + window a suspended grant stays open; the first run after it
// revokes every grant of the student suspended before the cut-off (by the
// system, reason suspension_expired, one audit record each with the job as
// actor) and tells the student once in-app (fixed text, no company named,
// no data, a link to the grant list); nobody else is told; another
// student's active grant and a later suspension are kept; the revoked
// grant cannot be resumed (409); a second run revokes nothing and repeats
// only the same notice (same dedupe key: notify keeps one); after the
// lookback no notice is repeated.
func TestExpire_AfterWindowRevokedAndStudentTold(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	orgA, recA, _ := e.company("Acme")
	orgB, _, _ := e.company("Beta")
	s, _ := e.granted(t, orgA)
	if err := e.svc.Grant(ctx, s.UserID, orgB, students.SourceContactRequest, uuid.NewV7()); err != nil {
		t.Fatal(err)
	}
	keeper, _ := e.granted(t, orgA)
	e.lapse(t, s)
	at := e.clk.Now()
	gA := e.suspendedGrant(t, s.UserID, orgA)

	// Another student lapses a day later: still inside the window then.
	e.clk.Advance(24 * time.Hour)
	late, _ := e.granted(t, orgB)
	e.lapse(t, late)

	e.clk.Set(at.Add(window))
	if err := e.runJob(t, students.ExpireSuspendedGrantsJobName); err != nil {
		t.Fatal(err)
	}
	if got := e.suspendedGrant(t, s.UserID, orgA); got.RevokedAt != nil {
		t.Fatalf("revoked at the end of the window: %+v", got)
	}
	if n := len(e.expiryNotices(s.UserID)); n != 0 {
		t.Fatalf("notices before the expiry: %d", n)
	}

	e.clk.Set(at.Add(window + time.Microsecond))
	now := e.clk.Now()
	if err := e.runJob(t, students.ExpireSuspendedGrantsJobName); err != nil {
		t.Fatal(err)
	}
	all, _ := e.store.AllGrants(ctx, s.UserID)
	if len(all) != 2 {
		t.Fatalf("grants = %+v", all)
	}
	for _, g := range all {
		if g.Status() != students.GrantRevoked || *g.RevokedByKind != students.RevokedBySystem || !g.RevokedAt.Equal(now) ||
			g.RevokeReason == nil || *g.RevokeReason != students.ReasonSuspensionExpired || !g.SuspendedAt.Equal(at) {
			t.Fatalf("expired grant = %+v", g)
		}
	}
	rows := e.audit.Of(authz.StudentsAccessGrantRevoke)
	if len(rows) != 2 {
		t.Fatalf("expiry audit rows = %d, want 2", len(rows))
	}
	for _, r := range rows {
		if r.SystemJob != students.ExpireSuspendedGrantsJobName || r.ActorUserID != nil || r.Metadata["revoked_by"] != "system" ||
			r.Metadata["reason"] != students.ReasonSuspensionExpired || r.Metadata["subject_user_id"] != s.UserID.String() {
			t.Fatalf("expiry audit = %+v", r)
		}
	}
	notes := e.expiryNotices(s.UserID)
	if len(notes) != 1 {
		t.Fatalf("expiry notices = %+v, want one", notes)
	}
	m := notes[0]
	if m.UserID == nil || *m.UserID != s.UserID || m.Channel != notify.ChannelInApp || m.Category != notify.CategoryTransactional ||
		m.LinkPath != "/me/access-grants" || len(m.Data) != 0 || m.SourceOrgID != nil || m.DedupeKey == "" {
		t.Fatalf("expiry notice = %+v", m)
	}
	if ok, err := e.svc.Has(ctx, keeper.UserID, orgA); err != nil || !ok {
		t.Fatalf("another student's active grant: Has = %v, %v", ok, err)
	}
	if g := e.suspendedGrant(t, late.UserID, orgB); g.RevokedAt != nil {
		t.Fatalf("a suspension inside its window was revoked: %+v", g)
	}
	e.companyHidden(t, "expired", recA, orgA, s.UserID)
	if mine, err := e.svc.ListMyAccessGrants(ctx, s, nil, 0); err != nil || len(mine.Items) != 0 {
		t.Fatalf("my grants after the expiry = %+v, %v", mine, err)
	}
	e.users.verify(s.UserID, e.uniA, now) // verified again: the ended grant is still not resumable
	_, err := e.svc.ResumeMyAccessGrant(ctx, s, gA.ID)
	wantCode(t, err, "students.invalid_state")

	// A second run changes nothing; its notice is the same message.
	e.clk.Advance(time.Hour)
	if err := e.runJob(t, students.ExpireSuspendedGrantsJobName); err != nil {
		t.Fatal(err)
	}
	if n := len(e.audit.Of(authz.StudentsAccessGrantRevoke)); n != 2 {
		t.Fatalf("a second run audited again: %d", n)
	}
	if len(e.expiryNotices(late.UserID)) != 0 {
		t.Fatal("a student inside the window was told of an expiry")
	}
	if notes = e.expiryNotices(s.UserID); len(notes) != 2 || notes[1].DedupeKey != m.DedupeKey {
		t.Fatalf("second run notices = %+v", notes)
	}
	// After the lookback the expiry is not told again.
	e.clk.Advance(72 * time.Hour)
	if err := e.runJob(t, students.ExpireSuspendedGrantsJobName); err != nil {
		t.Fatal(err)
	}
	if n := len(e.expiryNotices(s.UserID)); n != 2 {
		t.Fatalf("notices after the lookback = %d, want 2", n)
	}
	for _, msg := range e.notices.messages() {
		if *msg.UserID != s.UserID && *msg.UserID != late.UserID {
			t.Fatalf("a notice went to someone else: %+v", msg)
		}
	}
}

// TestExpire_NoticeRetriedAfterSendFailure: a transient notify failure
// fails the run after the revocation committed; the next run sends the
// notice without revoking or auditing again; a permanent failure is
// logged and the run succeeds.
func TestExpire_NoticeRetriedAfterSendFailure(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	org, _, _ := e.company("Acme")
	s, _ := e.granted(t, org)
	e.lapse(t, s)
	e.clk.Advance(window + time.Hour)

	e.notices.mu.Lock()
	e.notices.fail = notify.ErrDeliveryFailed
	e.notices.mu.Unlock()
	if err := e.runJob(t, students.ExpireSuspendedGrantsJobName); err == nil {
		t.Fatal("a transient notify failure did not fail the run")
	}
	if all, _ := e.store.AllGrants(t.Context(), s.UserID); len(all) != 1 || all[0].Status() != students.GrantRevoked {
		t.Fatalf("grants after the failed run = %+v", all)
	}
	e.notices.mu.Lock()
	e.notices.fail = nil
	e.notices.mu.Unlock()
	e.clk.Advance(time.Minute)
	if err := e.runJob(t, students.ExpireSuspendedGrantsJobName); err != nil {
		t.Fatal(err)
	}
	if n := len(e.expiryNotices(s.UserID)); n != 1 {
		t.Fatalf("notices after the retry = %d, want 1", n)
	}
	if n := len(e.audit.Of(authz.StudentsAccessGrantRevoke)); n != 1 {
		t.Fatalf("revocation audit rows = %d, want 1", n)
	}

	s2, _ := e.granted(t, org)
	e.lapse(t, s2)
	e.clk.Advance(window + time.Hour)
	e.notices.mu.Lock()
	e.notices.fail = notify.ErrDeliveryRejected
	e.notices.mu.Unlock()
	if err := e.runJob(t, students.ExpireSuspendedGrantsJobName); err != nil {
		t.Fatalf("a permanent notify failure: %v", err)
	}
}

// TestExpire_ResumedGrantKept: a grant resumed inside its window is never
// revoked by the expiry; a configured window replaces the 90 days.
func TestExpire_ResumedGrantKept(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(d *students.Deps) { d.SuspensionWindow = 24 * time.Hour })
	ctx := t.Context()
	org, rec, _ := e.company("Acme")
	kept, _ := e.granted(t, org)
	gone, _ := e.granted(t, org)
	e.lapse(t, kept)
	e.lapse(t, gone)
	e.clk.Advance(time.Hour)
	e.reverify(t, kept, e.uniA, false)
	if _, err := e.svc.ResumeMyAccessGrant(ctx, kept, e.suspendedGrant(t, kept.UserID, org).ID); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(24 * time.Hour)
	if err := e.runJob(t, students.ExpireSuspendedGrantsJobName); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.GetCompanyStudentProfile(ctx, rec, org, kept.UserID); err != nil {
		t.Fatalf("the resumed grant after the expiry run: %v", err)
	}
	if all, _ := e.store.AllGrants(ctx, gone.UserID); len(all) != 1 || all[0].Status() != students.GrantRevoked {
		t.Fatalf("the 24-hour window did not expire: %+v", all)
	}
}
