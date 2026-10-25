package students_test

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/problem"
	"koro/internal/students"
)

// window is the default suspension window (Q-03: 90 days).
const window = 90 * 24 * time.Hour

// suspendedGrant returns the student's only open grant of org, which must
// be suspended.
func (e *env) suspendedGrant(t *testing.T, student, org uuid.UUID) students.AccessGrant {
	t.Helper()
	all, err := e.store.AllGrants(t.Context(), student)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range all {
		if g.CompanyOrgID == org && g.Open() {
			if g.Status() != students.GrantSuspended {
				t.Fatalf("grant of %s is %s, want suspended", org, g.Status())
			}
			return g
		}
	}
	t.Fatalf("no open grant of %s", org)
	return students.AccessGrant{}
}

// TestResume_WithinWindowCompanySeesAgain (BE-088, T-332): a student whose
// verification lapsed sees the end of the window on the suspended grant;
// without a verification the resume is 403 students.verification_required;
// verified again, another student's grant and an unknown one are 404, the
// own grant becomes active (resumed_at, the company's name in the answer,
// one audit record with the student as actor, nobody told) and the company
// sees the student again; a second resume is 409 students.invalid_state.
func TestResume_WithinWindowCompanySeesAgain(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	org, rec, _ := e.company("Acme")
	s, _ := e.granted(t, org)
	e.withCV(t, s)
	lapsed := e.lapse(t, s)
	suspendedAt := e.clk.Now()
	g := e.suspendedGrant(t, s.UserID, org)

	mine, err := e.svc.ListMyAccessGrants(ctx, lapsed, nil, 0)
	if err != nil || len(mine.Items) != 1 || mine.Items[0].ResumableUntil == nil ||
		!mine.Items[0].ResumableUntil.Equal(suspendedAt.Add(window)) {
		t.Fatalf("my grants while lapsed = %+v, %v", mine, err)
	}
	_, err = e.svc.ResumeMyAccessGrant(ctx, lapsed, g.ID)
	wantCode(t, err, "students.verification_required")

	e.clk.Advance(24 * time.Hour)
	e.reverify(t, s, e.uniA, false)
	e.companyHidden(t, "verified again, not resumed", rec, org, s.UserID)

	other := e.profiled(t)
	if _, err := e.svc.ResumeMyAccessGrant(ctx, other, g.ID); !errors.Is(err, problem.ErrNotFound) {
		t.Fatalf("another student's grant: %v, want 404", err)
	}
	if _, err := e.svc.ResumeMyAccessGrant(ctx, s, uuid.NewV7()); !errors.Is(err, problem.ErrNotFound) {
		t.Fatalf("an unknown grant: %v, want 404", err)
	}
	if got := e.suspendedGrant(t, s.UserID, org); got.ResumedAt != nil {
		t.Fatalf("a refused resume changed the grant: %+v", got)
	}

	before := len(e.notices.messages())
	v, err := e.svc.ResumeMyAccessGrant(ctx, s, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v.Grant.ID != g.ID || v.Grant.Status() != students.GrantActive || v.CompanyName != "Acme" || v.ResumableUntil != nil ||
		v.Grant.ResumedAt == nil || !v.Grant.ResumedAt.Equal(e.clk.Now()) || v.Grant.SuspendedAt != nil ||
		!v.Grant.GrantedAt.Equal(g.GrantedAt) {
		t.Fatalf("resumed grant = %+v", v)
	}
	if _, err := e.svc.GetCompanyStudentProfile(ctx, rec, org, s.UserID); err != nil {
		t.Fatalf("the company after the resume: %v", err)
	}
	if _, err := e.svc.DownloadCompanyStudentCV(ctx, rec, org, s.UserID); err != nil {
		t.Fatalf("the company's CV download after the resume: %v", err)
	}
	if ok, err := e.svc.Has(ctx, s.UserID, org); err != nil || !ok {
		t.Fatalf("Has after the resume = %v, %v", ok, err)
	}
	rows := e.audit.Of(authz.StudentsAccessGrantResume)
	if len(rows) != 1 || rows[0].ActorUserID == nil || *rows[0].ActorUserID != s.UserID || *rows[0].ResourceID != g.ID ||
		*rows[0].OrgID != org || rows[0].Metadata["subject_user_id"] != s.UserID.String() || rows[0].Metadata["suspended_at"] == nil {
		t.Fatalf("resume audit = %+v", rows)
	}
	if n := len(e.notices.messages()); n != before {
		t.Fatalf("the resume sent %d notices, want none (the company is not told)", n-before)
	}

	_, err = e.svc.ResumeMyAccessGrant(ctx, s, g.ID)
	wantCode(t, err, "students.invalid_state")
	if n := len(e.audit.Of(authz.StudentsAccessGrantResume)); n != 1 {
		t.Fatalf("a refused resume was audited: %d", n)
	}
}

// TestResume_WindowBoundary (BE-088, T-331): a grant suspended at S may be
// resumed at exactly S + window; one microsecond later the resume is 409
// students.resume_window_expired and the grant stays suspended (the
// nightly expiry revokes it); a configured window replaces the 90 days.
func TestResume_WindowBoundary(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		window time.Duration
	}{{"default 90 days", 0}, {"configured 24 hours", 24 * time.Hour}} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, func(d *students.Deps) { d.SuspensionWindow = c.window })
			w := c.window
			if w == 0 {
				w = window
			}
			ctx := t.Context()
			org, _, _ := e.company("Acme")
			a, _ := e.granted(t, org)
			b, _ := e.granted(t, org)
			e.users.lapse(a.UserID)
			e.users.lapse(b.UserID)
			at := e.clk.Now()
			for _, s := range []authz.Actor{a, b} {
				body := e.event(t, domainevent.UserEduVerificationLapsed{UserID: s.UserID, VerificationID: uuid.NewV7()}, at)
				if err := e.handle(t, domainevent.TopicUserEduVerificationLapsed, students.SuspendLapsedGrantsTaskName, body); err != nil {
					t.Fatal(err)
				}
				e.users.verify(s.UserID, e.uniA, at.Add(time.Hour))
			}
			ga, gb := e.suspendedGrant(t, a.UserID, org), e.suspendedGrant(t, b.UserID, org)

			e.clk.Set(at.Add(w))
			if v, err := e.svc.ResumeMyAccessGrant(ctx, a, ga.ID); err != nil || v.Grant.Status() != students.GrantActive {
				t.Fatalf("resume at the end of the window = %+v, %v", v, err)
			}
			e.clk.Set(at.Add(w + time.Microsecond))
			_, err := e.svc.ResumeMyAccessGrant(ctx, b, gb.ID)
			wantCode(t, err, "students.resume_window_expired")
			if got := e.suspendedGrant(t, b.UserID, org); got.ResumedAt != nil || !got.SuspendedAt.Equal(at) {
				t.Fatalf("the expired grant changed: %+v", got)
			}
		})
	}
}

// TestResume_NotSuspended409: an active grant and a revoked one cannot be
// resumed (409 students.invalid_state); a minor's account never holds a
// grant (404).
func TestResume_NotSuspended409(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	org, _, _ := e.company("Acme")
	s, _ := e.granted(t, org)
	mine, _ := e.svc.ListMyAccessGrants(ctx, s, nil, 0)
	active := mine.Items[0].Grant.ID
	_, err := e.svc.ResumeMyAccessGrant(ctx, s, active)
	wantCode(t, err, "students.invalid_state")
	if err := e.svc.RevokeMyAccessGrant(ctx, s, active, ""); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.ResumeMyAccessGrant(ctx, s, active)
	wantCode(t, err, "students.invalid_state")
	minor := e.student(t, e.uniA, true)
	if _, err := e.svc.ResumeMyAccessGrant(ctx, minor, active); !errors.Is(err, problem.ErrNotFound) {
		t.Fatalf("a minor on another user's grant: %v, want 404", err)
	}
}

// TestResume_VerificationReadNow: an actor resolved as verified whose
// verification ended since (identity has no active one now) is refused
// like an unverified account (403 students.verification_required).
func TestResume_VerificationReadNow(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	org, _, _ := e.company("Acme")
	s, _ := e.granted(t, org)
	e.lapse(t, s)
	g := e.suspendedGrant(t, s.UserID, org)
	_, err := e.svc.ResumeMyAccessGrant(t.Context(), s, g.ID) // s still says verified
	wantCode(t, err, "students.verification_required")
}

// TestResume_LateSuspensionTaskKeepsResumed (BE-088): a suspension task
// whose event is older than the resume (a late gap task, a redelivered
// lapse) never suspends the resumed grant again; a new lapse after the
// resume suspends it as any active grant.
func TestResume_LateSuspensionTaskKeepsResumed(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	org, rec, _ := e.company("Acme")
	s, _ := e.granted(t, org)

	e.users.lapse(s.UserID)
	lapseBody := e.event(t, domainevent.UserEduVerificationLapsed{UserID: s.UserID, VerificationID: uuid.NewV7()}, e.clk.Now())
	if err := e.handle(t, domainevent.TopicUserEduVerificationLapsed, students.SuspendLapsedGrantsTaskName, lapseBody); err != nil {
		t.Fatal(err)
	}
	g := e.suspendedGrant(t, s.UserID, org)

	// The student verifies again; the gap task of that verification is late.
	e.clk.Advance(time.Hour)
	verifiedAt := e.clk.Now()
	e.users.verify(s.UserID, e.uniA, verifiedAt)
	gapBody := e.event(t, domainevent.UserEduVerified{UserID: s.UserID, UniversityID: e.uniA,
		ExpiresAt: verifiedAt.AddDate(1, 0, 0)}, verifiedAt)
	e.clk.Advance(time.Minute)
	if _, err := e.svc.ResumeMyAccessGrant(ctx, s, g.ID); err != nil {
		t.Fatal(err)
	}
	for name, run := range map[string]func() error{
		"late gap task": func() error {
			return e.handle(t, domainevent.TopicUserEduVerified, students.SuspendGapGrantsTaskName, gapBody)
		},
		"redelivered lapse": func() error {
			return e.handle(t, domainevent.TopicUserEduVerificationLapsed, students.SuspendLapsedGrantsTaskName, lapseBody)
		},
	} {
		if err := run(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got, err := e.store.GetGrant(ctx, s.UserID, g.ID); err != nil || got.Status() != students.GrantActive {
			t.Fatalf("%s suspended the resumed grant: %+v, %v", name, got, err)
		}
	}
	if _, err := e.svc.GetCompanyStudentProfile(ctx, rec, org, s.UserID); err != nil {
		t.Fatalf("the company after the late tasks: %v", err)
	}

	// A new lapse suspends it again.
	e.clk.Advance(24 * time.Hour)
	e.lapse(t, s)
	if got := e.suspendedGrant(t, s.UserID, org); !got.SuspendedAt.Equal(e.clk.Now()) || got.ResumedAt == nil {
		t.Fatalf("after a new lapse: %+v", got)
	}
}
