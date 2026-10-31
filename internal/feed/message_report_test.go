package feed_test

import (
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/feed"
	"koro/internal/platform/problem/problemtest"
)

// TestMessageReport_QueueEntry (BE-162b, Q-43): messaging files a report on
// a conversation message into the same queue (target_type message): one
// open report per reporter and message, an audit record without org scope
// and the moderators' event for a new one; the reason and the details are
// checked; the queue lists it under target_type message; the report view
// has no target (the messages are messaging's, read with a justification).
func TestMessageReport_QueueEntry(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	student, mod := persona(t, "student"), persona(t, "moderator")
	msg := uuid.NewV7()
	in := feed.MessageReportInput{MessageID: msg, Reason: feed.ReasonHarassment, Details: "Israrlı mesajlar"}
	first, err := e.svc.FileMessageReport(ctx, student, in)
	if err != nil || !first.Created {
		t.Fatalf("FileMessageReport = %+v, %v", first, err)
	}
	again, err := e.svc.FileMessageReport(ctx, student, feed.MessageReportInput{MessageID: msg, Reason: feed.ReasonSpam})
	if err != nil || again.Created || again.ID != first.ID {
		t.Fatalf("again = %+v, %v", again, err)
	}
	if a := e.audit.Of(authz.FeedReportCreate); len(a) != 1 || a[0].OrgID != nil || a[0].Metadata["target_type"] != "message" {
		t.Fatalf("audit = %+v", a)
	}
	_, err = e.svc.FileMessageReport(ctx, student, feed.MessageReportInput{MessageID: uuid.NewV7(), Reason: "rude"})
	problemtest.WantProblem(t, err, 422, "validation_failed")
	_, err = e.svc.FileMessageReport(ctx, persona(t, "anonymous"), in)
	problemtest.WantErr(t, err, authz.ErrForbidden)

	page, err := e.svc.ListReports(ctx, mod, feed.ReportQuery{Filter: feed.ReportFilter{TargetType: feed.TargetMessage}})
	if err != nil || len(page.Items) != 1 || page.Items[0].Report.TargetID != msg {
		t.Fatalf("message reports = %+v, %v", page, err)
	}
	if posts, _ := e.svc.ListReports(ctx, mod, feed.ReportQuery{Filter: feed.ReportFilter{TargetType: feed.TargetPost}}); len(posts.Items) != 0 {
		t.Fatalf("post filter lists the message report: %+v", posts)
	}
	v, err := e.svc.GetReport(ctx, mod, first.ID)
	if err != nil || v.Target != nil {
		t.Fatalf("report view = %+v, %v", v, err)
	}
	if d := feed.ToReportDTO(v); d.TargetType != "message" || d.Target != nil {
		t.Fatalf("report DTO = %+v", d)
	}
}

// TestMessageReport_ActionedNeedsANote: a message report has no moderation
// action of the feed (its measure is the company's or the user's
// suspension, or the conversation closed), so actioned needs a resolution
// note saying what was done, now or from in_review; dismissed needs
// nothing; a post report still needs its moderation action.
func TestMessageReport_ActionedNeedsANote(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	mod := persona(t, "moderator")
	file := func() uuid.UUID {
		r, err := e.svc.FileMessageReport(ctx, persona(t, "student"), feed.MessageReportInput{MessageID: uuid.NewV7(),
			Reason: feed.ReasonHarassment})
		if err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	a := file()
	_, err := e.svc.ResolveReport(ctx, mod, a, feed.ResolveInput{Status: feed.ReportActioned})
	problemtest.WantProblem(t, err, 422, "validation_failed")
	v, err := e.svc.ResolveReport(ctx, mod, a, feed.ResolveInput{Status: feed.ReportActioned,
		Note: "Şirket askıya alındı (orgs), konuşma kapatıldı"})
	if err != nil || v.Report.Status != feed.ReportActioned || v.Report.ResolutionNote == nil {
		t.Fatalf("actioned = %+v, %v", v.Report, err)
	}
	b := file()
	if _, err := e.svc.ResolveReport(ctx, mod, b, feed.ResolveInput{Status: feed.ReportInReview, Note: "İnceleniyor: uyarı verilecek"}); err != nil {
		t.Fatal(err)
	}
	if v, err := e.svc.ResolveReport(ctx, mod, b, feed.ResolveInput{Status: feed.ReportActioned}); err != nil ||
		v.Report.Status != feed.ReportActioned {
		t.Fatalf("actioned with the earlier note = %+v, %v", v.Report, err)
	}
	if v, err := e.svc.ResolveReport(ctx, mod, file(), feed.ResolveInput{Status: feed.ReportDismissed}); err != nil ||
		v.Report.Status != feed.ReportDismissed {
		t.Fatalf("dismissed = %+v, %v", v.Report, err)
	}

	post := e.published(t, "club_admin", "club_a", feed.PostInput{Kind: feed.KindText, Body: doc("Duyuru")})
	r := e.report(t, "student", post.Post.ID)
	_, err = e.svc.ResolveReport(ctx, mod, r.ID, feed.ResolveInput{Status: feed.ReportActioned, Note: "not alone"})
	problemtest.WantProblem(t, err, 409, "feed.moderation_action_required")
}
