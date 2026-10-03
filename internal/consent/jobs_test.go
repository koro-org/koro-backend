package consent_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/queue"
)

// TestIYSReconcile_RetriesFailed (01 §7 consent.iys_reconcile): failed and
// stuck submissions are delivered again; fresh, sent and skipped ones are
// left alone.
func TestIYSReconcile_RetriesFailed(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	text := e.consentText(t, consent.PurposeCommercialEmail, "2026.10.01")
	failedID := e.record(t, grant(actor(t, "student").UserID, text, consent.ChannelWeb))

	// İYS is down: the task fails, the row records the failure, the task is retried.
	e.iys.Fail(errors.New("iys: 503 from api.iys.org.tr for ada@example.com"))
	errs := e.deliverAll(t)
	if len(errs) != 1 || errs[0] == nil || queue.IsPermanent(errs[0]) {
		t.Fatalf("delivery while İYS is down = %v, want a retryable error", errs)
	}
	sub, _ := e.store.SubmissionOf(ctx, failedID)
	if sub.Status != consent.SubmissionFailed || sub.Attempts != 1 || sub.LastError == nil ||
		strings.Contains(*sub.LastError, "ada@example.com") {
		t.Fatalf("failed submission = %+v (last_error must be scrubbed)", sub)
	}

	// A pending submission whose event was lost, and a fresh one.
	stuckID := e.record(t, grant(actor(t, "student_b").UserID, text, consent.ChannelWeb))
	reconcile := e.job(t, consent.ReconcileJobName)
	if reconcile.Cron != "30 5 * * *" || reconcile.Queue != queue.Default || reconcile.Unique <= 0 {
		t.Fatalf("job = %+v", reconcile)
	}

	// Within the grace period nothing is touched.
	e.iys.Fail(nil)
	if err := reconcile.Handle(ctx); err != nil {
		t.Fatal(err)
	}
	if len(e.iys.Sent()) != 0 {
		t.Fatalf("reconcile sent fresh submissions: %+v", e.iys.Sent())
	}

	e.clk.Advance(consent.ReconcileGrace + time.Minute)
	freshID := e.record(t, grant(actor(t, "club_member").UserID, text, consent.ChannelWeb))
	if err := reconcile.Handle(ctx); err != nil {
		t.Fatal(err)
	}
	check := func(name string, sub consent.Submission, want consent.SubmissionStatus, attempts int) {
		t.Helper()
		if sub.Status != want || sub.Attempts != attempts {
			t.Fatalf("%s submission = %s after %d attempts, want %s after %d", name, sub.Status, sub.Attempts, want, attempts)
		}
	}
	s1, _ := e.store.SubmissionOf(ctx, failedID)
	check("failed", s1, consent.SubmissionSent, 2)
	if s1.LastError != nil || s1.SentAt == nil {
		t.Fatalf("sent submission keeps an error or lacks sent_at: %+v", s1)
	}
	s2, _ := e.store.SubmissionOf(ctx, stuckID)
	check("stuck", s2, consent.SubmissionSent, 1)
	s3, _ := e.store.SubmissionOf(ctx, freshID)
	check("fresh", s3, consent.SubmissionPending, 0)

	// Running again sends nothing twice.
	e.clk.Advance(24 * time.Hour)
	if err := reconcile.Handle(ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(e.iys.Sent()); got != 3 {
		t.Fatalf("İYS received %d submissions, want 3 (failed, stuck, then fresh once)", got)
	}
}

// TestSubmitIYS_NoopClientSkips: without a real İYS client (dev) the
// submission ends as skipped instead of failing forever.
func TestSubmitIYS_NoopClientSkips(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(d *consent.Deps) { d.IYS = nil })
	ctx := t.Context()
	text := e.consentText(t, consent.PurposeCommercialEmail, "2026.10.01")
	id := e.record(t, grant(actor(t, "student").UserID, text, consent.ChannelWeb))
	for _, err := range e.deliverAll(t) {
		if err != nil {
			t.Fatal(err)
		}
	}
	sub, _ := e.store.SubmissionOf(ctx, id)
	if sub.Status != consent.SubmissionSkipped || sub.LastError == nil || sub.Attempts != 0 {
		t.Fatalf("submission = %+v", sub)
	}
	// A bad payload is permanent.
	ev, _ := domainevent.New(domainevent.ConsentRecorded{ConsentID: text.ID}, "consent", text.ID)
	if err := e.subscription(t).Handle(ctx, ev); !queue.IsPermanent(err) {
		t.Fatalf("event of a missing submission = %v, want permanent", err)
	}
	// CONSENT_IYS_REQUIRED refuses the no-op client.
	if _, err := consent.New(consent.Deps{IYSRequired: true}); err == nil || !strings.Contains(err.Error(), "CONSENT_IYS_REQUIRED") {
		t.Fatalf("New with IYSRequired and no client = %v", err)
	}
}

// TestPrivacy_ExportKeepsLedger (02 §4.3): the export lists every act with
// its proof; erasure deletes nothing, withdraws what is still granted
// (İYS learns of it) and is idempotent.
func TestPrivacy_ExportKeepsLedger(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	user := actor(t, "student").UserID
	email := e.consentText(t, consent.PurposeCommercialEmail, "2026.10.01")
	push := e.consentText(t, consent.PurposeCommercialPush, "2026.10.01")
	e.record(t, grant(user, email, consent.ChannelWeb))
	e.record(t, grant(user, push, consent.ChannelWeb))
	e.record(t, withdraw(user, consent.PurposeCommercialPush))

	sec, err := e.mod.Privacy.Export(ctx, user)
	if err != nil || sec.Module != "consent" || len(sec.Items) != 3 || sec.Items[0]["text_sha256"] == nil || sec.Items[0]["action"] != "granted" {
		t.Fatalf("Export = %+v, %v", sec, err)
	}
	events := len(e.out.Events())

	res, err := e.mod.Privacy.Erase(ctx, user)
	if err != nil || res.Deleted != 0 || res.Retained != 4 || res.RetainedReason == "" {
		t.Fatalf("Erase = %+v, %v", res, err)
	}
	if on, _ := e.svc.HasActive(ctx, user, consent.PurposeCommercialEmail, nil); on {
		t.Fatal("a consent survived the erasure")
	}
	if got := len(e.out.Events()); got != events+1 {
		t.Fatalf("erasure emitted %d İYS events, want 1 (commercial_email withdrawal)", got-events)
	}
	evs := e.audit.of(string(authz.ConsentRecordCreate))
	if last := evs[len(evs)-1]; last.SystemJob != consent.ErasureJob || last.Metadata[audit.MetaSubjectUserID] != user.String() {
		t.Fatalf("erasure audit = %+v", last)
	}
	again, err := e.mod.Privacy.Erase(ctx, user)
	if err != nil || again.Retained != 4 {
		t.Fatalf("second Erase = %+v, %v (must be idempotent)", again, err)
	}
}

// TestAdminListUser_AuditedBreakGlass (03 §6.3, §6.5): every proof read is
// audited; support's is a break-glass access with its reason.
func TestAdminListUser_AuditedBreakGlass(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	user := actor(t, "student").UserID
	text := e.consentText(t, consent.PurposeContactInfoShare, "2026.10.01")
	for range 3 {
		e.record(t, grant(user, text, consent.ChannelWeb))
	}

	_, err := e.svc.AdminListUser(ctx, actor(t, "support"), user, consent.Access{}, consent.PageQuery{})
	wantErr(t, err, authz.ErrBreakGlassReason)
	_, err = e.svc.AdminListUser(ctx, actor(t, "platform_admin"), user, consent.Access{Reason: "Ticket 4711: checking"}, consent.PageQuery{})
	wantErr(t, err, authz.ErrForbidden)

	reason := "Ticket 4711: user cannot unsubscribe"
	p, err := e.svc.AdminListUser(ctx, actor(t, "support"), user, consent.Access{Reason: reason, TicketRef: "T-4711"}, consent.PageQuery{Limit: 2})
	if err != nil || len(p.Items) != 2 || p.Next == nil || p.Items[0].IP != "" {
		t.Fatalf("page 1 = %+v, %v", p, err)
	}
	p2, err := e.svc.AdminListUser(ctx, actor(t, "dpo"), user, consent.Access{}, consent.PageQuery{Limit: 2, After: p.Next})
	if err != nil || len(p2.Items) != 1 || p2.Next != nil || !p2.Items[0].OccurredAt.Before(p.Items[1].OccurredAt) {
		t.Fatalf("page 2 = %+v, %v", p2, err)
	}

	evs := e.audit.of(string(authz.ConsentRecordListAdmin))
	if len(evs) != 2 {
		t.Fatalf("audit rows = %d, want 2", len(evs))
	}
	bg, plain := evs[0], evs[1]
	if bg.Reason != reason || bg.Metadata[audit.MetaBreakGlass] != true || bg.Metadata[audit.MetaTicketRef] != "T-4711" ||
		bg.Metadata[audit.MetaSubjectUserID] != user.String() || *bg.ResourceID != user {
		t.Fatalf("break-glass audit = %+v", bg)
	}
	if _, ok := plain.Metadata[audit.MetaBreakGlass]; ok {
		t.Fatalf("dpo read flagged as break-glass: %+v", plain)
	}
}
