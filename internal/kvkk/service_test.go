package kvkk_test

import (
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/kvkk"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/problem"
	"koro/internal/platform/problem/problemtest"
)

// TestCreateDSR_ErasureRequiresReauth (03 k31, §6.2): an erasure request
// without, or with a wrong, re-authentication proof is refused and audited
// as denied; with the proof it is received, identity-verified at that
// moment and the DPO is notified (dsr.submitted).
func TestCreateDSR_ErasureRequiresReauth(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx, student := t.Context(), actor(t, "student")

	_, err := e.svc.CreateMine(ctx, student, kvkk.MyRequest{Type: kvkk.TypeErasure})
	problemtest.WantErr(t, err, errBadProof)
	_, err = e.svc.CreateMine(ctx, student, kvkk.MyRequest{Type: kvkk.TypeErasure, Proof: kvkk.ReauthProof{Password: "wrong"}})
	problemtest.WantErr(t, err, errBadProof)
	denied := e.audit.of(string(authz.KvkkDsrCreate))
	if len(denied) != 2 || denied[0].Outcome != audit.Denied || denied[0].Reason != "reauth_failed" {
		t.Fatalf("denied audit rows = %+v", denied)
	}
	if rows, _ := e.store.AllOf(ctx, student.UserID); len(rows) != 0 || len(e.topics()) != 0 {
		t.Fatalf("a refused request left %d rows and events %v", len(rows), e.topics())
	}

	d, err := e.svc.CreateMine(ctx, student, kvkk.MyRequest{Type: kvkk.TypeErasure, Details: "Hesabımı silin.", Proof: proof})
	if err != nil {
		t.Fatal(err)
	}
	now := e.clk.Now()
	if d.Status != kvkk.StatusReceived || d.Channel != kvkk.ChannelInApp || d.IdentityVerifiedAt == nil ||
		!d.IdentityVerifiedAt.Equal(now) || *d.UserID != student.UserID {
		t.Fatalf("erasure request = %+v", d)
	}
	ok := e.audit.of(string(authz.KvkkDsrCreate))
	if last := ok[len(ok)-1]; last.Outcome != audit.Success || last.Metadata[audit.MetaReauth] != "password" {
		t.Fatalf("audit = %+v", last)
	}
	if !slices.Equal(e.topics(), []string{domainevent.TopicDSRSubmitted}) {
		t.Fatalf("topics = %v", e.topics())
	}
	// The DPO mailbox learns the ID, type and deadline — never the content.
	if _, err := e.deliver(t, 0); err != nil {
		t.Fatal(err)
	}
	mails := e.mail.of("kvkk.dsr_submitted")
	if len(mails) != 1 || mails[0].ToEmail != dpoMailbox || mails[0].Data["dsr_id"] != d.ID.String() {
		t.Fatalf("DPO mails = %+v", mails)
	}
	for _, v := range mails[0].Data {
		if s, ok := v.(string); ok && strings.Contains(s, "Hesabımı") {
			t.Fatal("the request details reached the DPO e-mail")
		}
	}
	// One open request per type.
	_, err = e.svc.CreateMine(ctx, student, kvkk.MyRequest{Type: kvkk.TypeErasure, Proof: proof})
	problemtest.WantErr(t, err, kvkk.ErrDuplicateRequest)
	// Other types need no proof: the session is the identity check.
	x, err := e.svc.CreateMine(ctx, student, kvkk.MyRequest{Type: kvkk.TypeExport})
	if err != nil || x.IdentityVerifiedAt == nil {
		t.Fatalf("export request = %+v, %v", x, err)
	}
}

// TestDSR_DeadlineIs30Days (KVKK md. 13): due_at = received_at + 30 days
// for in-app and DPO-entered requests; days_left counts down to it.
func TestDSR_DeadlineIs30Days(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	d, err := e.svc.CreateMine(ctx, actor(t, "student"), kvkk.MyRequest{Type: kvkk.TypeAccess})
	if err != nil {
		t.Fatal(err)
	}
	if want := d.ReceivedAt.Add(30 * 24 * time.Hour); !d.DueAt.Equal(want) || kvkk.LegalPeriod != 30*24*time.Hour {
		t.Fatalf("due_at = %s, want %s", d.DueAt, want)
	}
	a, err := e.svc.AdminCreate(ctx, actor(t, "dpo"), kvkk.AdminRequest{RequesterEmail: "basvuru@example.test",
		RequesterName: "Ayşe Kaya", Type: kvkk.TypeInfo, Channel: kvkk.ChannelPost})
	if err != nil {
		t.Fatal(err)
	}
	if !a.DueAt.Equal(a.ReceivedAt.Add(kvkk.LegalPeriod)) || a.IdentityVerifiedAt != nil {
		t.Fatalf("DPO-entered request = %+v", a)
	}
	for _, tc := range []struct {
		after time.Duration
		left  int
	}{{0, 30}, {time.Hour, 30}, {23 * 24 * time.Hour, 7}, {30 * 24 * time.Hour, 0}, {31 * 24 * time.Hour, -1}} {
		if got := d.DaysLeft(d.ReceivedAt.Add(tc.after)); got != tc.left {
			t.Errorf("DaysLeft(+%s) = %d, want %d", tc.after, got, tc.left)
		}
	}
}

func TestAdminFlow_UpdateIfMatchAndState(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx, dpo := t.Context(), actor(t, "dpo")
	d, err := e.svc.AdminCreate(ctx, dpo, kvkk.AdminRequest{RequesterEmail: "basvuru@example.test", Type: kvkk.TypeRectification, Channel: kvkk.ChannelKEP})
	if err != nil {
		t.Fatal(err)
	}
	// Not verified yet: cannot be completed.
	_, err = e.svc.Complete(ctx, dpo, d.ID, "Düzeltildi.", proof)
	problemtest.WantErr(t, err, kvkk.ErrIdentityNotVerified)
	// If-Match is mandatory and must be fresh.
	st := kvkk.StatusIdentityCheck
	_, err = e.svc.AdminUpdate(ctx, dpo, d.ID, "", kvkk.Patch{Status: &st})
	problemtest.WantErr(t, err, problem.ErrPreconditionRequired)
	_, err = e.svc.AdminUpdate(ctx, dpo, d.ID, `"stale"`, kvkk.Patch{Status: &st})
	problemtest.WantErr(t, err, problem.ErrPreconditionFailed)
	at := e.clk.Now().Add(-time.Minute)
	me := dpo.UserID
	u, err := e.svc.AdminUpdate(ctx, dpo, d.ID, kvkk.ETag(d), kvkk.Patch{Status: &st, IdentityVerifiedAt: &at, AssignedTo: &me})
	if err != nil {
		t.Fatal(err)
	}
	if u.Status != kvkk.StatusIdentityCheck || u.IdentityVerifiedAt == nil || *u.AssignedTo != me || kvkk.ETag(u) == kvkk.ETag(d) {
		t.Fatalf("updated = %+v", u)
	}
	_, err = e.svc.AdminUpdate(ctx, dpo, d.ID, kvkk.ETag(d), kvkk.Patch{Status: &st})
	problemtest.WantErr(t, err, problem.ErrPreconditionFailed)
	bad := kvkk.StatusCompleted
	_, err = e.svc.AdminUpdate(ctx, dpo, d.ID, kvkk.ETag(u), kvkk.Patch{Status: &bad})
	problemtest.WantErr(t, err, kvkk.ErrInvalidRequest)
	// Answer: the applicant without an account is e-mailed at the address
	// the DPO entered.
	c := e.completeAs(t, d.ID)
	if c.Status != kvkk.StatusCompleted || c.CompletedAt == nil {
		t.Fatalf("completed = %+v", c)
	}
	if _, err := e.deliver(t, 0); err != nil {
		t.Fatal(err)
	}
	if m := e.mail.of("kvkk.dsr_completed"); len(m) != 1 || m[0].ToEmail != "basvuru@example.test" || m[0].Data["outcome"] != kvkk.OutcomeAnswered {
		t.Fatalf("completion mail = %+v", m)
	}
	_, err = e.svc.Reject(ctx, dpo, d.ID, "Geç kaldı.")
	problemtest.WantErr(t, err, kvkk.ErrState)
	_, err = e.svc.AdminUpdate(ctx, dpo, d.ID, kvkk.ETag(c), kvkk.Patch{Status: &st})
	problemtest.WantErr(t, err, kvkk.ErrState)
}

func TestAdmin_PlatformAdminSeesAwaitingOnly_AndNoOwnRequest(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	open, err := e.svc.CreateMine(ctx, actor(t, "student"), kvkk.MyRequest{Type: kvkk.TypeAccess})
	if err != nil {
		t.Fatal(err)
	}
	prof, err := e.svc.CreateMine(ctx, actor(t, "student"), kvkk.MyRequest{Type: kvkk.TypeProfilingAccess})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Approve(ctx, actor(t, "dpo"), prof.ID, kvkk.StageFirst); err != nil {
		t.Fatal(err)
	}
	p, err := e.svc.AdminList(ctx, actor(t, "platform_admin"), kvkk.AdminFilter{}, nil, 0)
	if err != nil || len(p.Items) != 1 || p.Items[0].ID != prof.ID {
		t.Fatalf("platform_admin list = %+v, %v", p.Items, err)
	}
	_, err = e.svc.AdminGet(ctx, actor(t, "platform_admin"), open.ID)
	problemtest.WantErr(t, err, authz.ErrNotFound)
	if all, _ := e.svc.AdminList(ctx, actor(t, "dpo"), kvkk.AdminFilter{}, nil, 0); len(all.Items) != 2 {
		t.Fatalf("dpo list = %d items", len(all.Items))
	}
	// Staff never handle their own request.
	mine, err := e.svc.CreateMine(ctx, actor(t, "dpo"), kvkk.MyRequest{Type: kvkk.TypeAccess})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.Complete(ctx, actor(t, "dpo"), mine.ID, "Kendime yanıt.", proof)
	problemtest.WantErr(t, err, kvkk.ErrOwnRequest)
	// A non-staff user is refused before anything is read.
	_, err = e.svc.AdminGet(ctx, actor(t, "student"), uuid.NewV7())
	problemtest.WantErr(t, err, authz.ErrForbidden)
	// Another user's request is not found on /me.
	_, err = e.svc.GetMine(ctx, actor(t, "student_b"), open.ID)
	problemtest.WantErr(t, err, kvkk.ErrNotFound)
}

// TestProfilingAccess_TwoApproversThenReport: the profiling report is
// released only after two different approvers; the export carries the
// talent auditor report (or "not available" without the hook).
func TestProfilingAccess_TwoApproversThenReport(t *testing.T) {
	t.Parallel()
	talent := &fakeTalent{}
	e := newEnv(t, func(d *kvkk.Deps) { d.Talent = talent })
	ctx := t.Context()
	d, err := e.svc.CreateMine(ctx, actor(t, "student"), kvkk.MyRequest{Type: kvkk.TypeProfilingAccess})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.Complete(ctx, actor(t, "dpo"), d.ID, "Rapor ektedir.", proof)
	problemtest.WantErr(t, err, kvkk.ErrSecondApprovalRequired)
	if _, err := e.svc.Approve(ctx, actor(t, "dpo"), d.ID, kvkk.StageFirst); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.Approve(ctx, actor(t, "dpo"), d.ID, kvkk.StageSecond)
	problemtest.WantErr(t, err, authz.ErrFourEyes)
	_, err = e.svc.Approve(ctx, actor(t, "platform_admin"), d.ID, kvkk.StageFirst)
	problemtest.WantErr(t, err, authz.ErrForbidden) // visible now (awaiting), but never a first approver (k32)
	dpo2, _ := authztest.Get("dpo_2")
	if _, err := e.svc.Approve(ctx, dpo2.Actor, d.ID, kvkk.StageSecond); err != nil {
		t.Fatal(err)
	}
	e.completeAs(t, d.ID)
	if _, err := e.deliver(t, 0); err != nil {
		t.Fatal(err)
	}
	if talent.calls != 1 {
		t.Fatalf("talent report asked %d times", talent.calls)
	}
	names, _ := zipOf(t, e, d.ID)
	if !slices.Equal(names, []string{"manifest.json", "talent_report.json"}) {
		t.Fatalf("profiling export = %v", names)
	}
}

func TestCancelAndReject(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx, student := t.Context(), actor(t, "student")
	d, err := e.svc.CreateMine(ctx, student, kvkk.MyRequest{Type: kvkk.TypeObjection})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.CancelMine(ctx, actor(t, "student_b"), d.ID)
	problemtest.WantErr(t, err, kvkk.ErrNotFound)
	c, err := e.svc.CancelMine(ctx, student, d.ID)
	if err != nil || c.Status != kvkk.StatusCancelled {
		t.Fatalf("cancel = %+v, %v", c, err)
	}
	_, err = e.svc.CancelMine(ctx, student, d.ID)
	problemtest.WantErr(t, err, kvkk.ErrState)

	r, err := e.svc.CreateMine(ctx, student, kvkk.MyRequest{Type: kvkk.TypeRestriction})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.Reject(ctx, actor(t, "dpo"), r.ID, "  ")
	problemtest.WantErr(t, err, kvkk.ErrInvalidRequest)
	rj, err := e.svc.Reject(ctx, actor(t, "dpo"), r.ID, "Kanuni dayanak mevcut.")
	if err != nil || rj.Status != kvkk.StatusRejected || rj.CompletedAt == nil || rj.RejectionReason == "" {
		t.Fatalf("reject = %+v, %v", rj, err)
	}
	if _, err := e.deliver(t, 0); err != nil {
		t.Fatal(err)
	}
	if m := e.mail.of("kvkk.dsr_completed"); len(m) != 1 || m[0].Data["outcome"] != kvkk.OutcomeRejected {
		t.Fatalf("rejection mail = %+v", m)
	}
}

func TestComplete_ProfilingObjectionEmitsEvent(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	d, err := e.svc.CreateMine(t.Context(), actor(t, "student"), kvkk.MyRequest{Type: kvkk.TypeProfilingObjection})
	if err != nil {
		t.Fatal(err)
	}
	e.completeAs(t, d.ID)
	want := []string{domainevent.TopicDSRSubmitted, domainevent.TopicDSRProfilingObjection, domainevent.TopicDSRCompleted}
	if !slices.Equal(e.topics(), want) {
		t.Fatalf("topics = %v, want %v", e.topics(), want)
	}
	// Without the proof the answer is refused.
	_, err = e.svc.Complete(t.Context(), actor(t, "dpo"), d.ID, "x", kvkk.ReauthProof{})
	problemtest.WantErr(t, err, errBadProof)
}
