package feed_test

import (
	"context"
	"slices"
	"testing"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/feed"
	"koro/internal/platform/problem"
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

// messageReportOfCompanyA files a student's report of a conversation of
// company_a and returns its ID.
func messageReportOfCompanyA(ctx context.Context, t *testing.T, e *env) uuid.UUID {
	t.Helper()
	student := persona(t, "student")
	r, err := e.svc.FileMessageReport(ctx, student, feed.MessageReportInput{MessageID: uuid.NewV7(), Reason: feed.ReasonHarassment})
	if err != nil {
		t.Fatal(err)
	}
	e.parties.set(r.ID, feed.MessageReportParty{OrgID: orgID("company_a"), StudentUserID: student.UserID})
	return r.ID
}

// TestMessageReport_SideOfTheConversation (BE-183): a member of the reported
// conversation's company, or its student, neither reads nor resolves the
// report (403 with a denied record, no org scope), whatever their platform
// role, and does not see it in the queue; another moderator works it.
func TestMessageReport_SideOfTheConversation(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	member := func(name string, org uuid.UUID) authz.Actor {
		a := persona(t, name)
		a.Memberships = append(slices.Clone(a.Memberships), authz.Membership{OrgID: org, OrgKind: authz.OrgCompany, Role: authz.MemberRecruiter})
		return a
	}
	studentMod := persona(t, "moderator")
	studentMod.UserID = persona(t, "student").UserID
	for name, a := range map[string]authz.Actor{
		"moderator of the company": member("moderator", orgID("company_a")),
		"admin of the company":     member("platform_admin", orgID("company_a")),
		"the student":              studentMod,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			e := newEnv(t)
			id := messageReportOfCompanyA(ctx, t, e)
			_, err := e.svc.GetReport(ctx, a, id)
			problemtest.WantErr(t, err, authz.ErrForbidden)
			_, err = e.svc.ResolveReport(ctx, a, id, feed.ResolveInput{Status: feed.ReportDismissed})
			problemtest.WantErr(t, err, authz.ErrForbidden)
			for _, act := range []authz.Action{authz.FeedReportRead, authz.FeedReportResolve} {
				ev := e.audit.Of(act)
				if len(ev) != 1 || ev[0].Outcome != audit.Denied || ev[0].OrgID != nil || *ev[0].ResourceID != id {
					t.Fatalf("%s audit = %+v", act, ev)
				}
			}
			v, err := e.svc.GetReport(ctx, persona(t, "moderator"), id)
			if err != nil || v.Report.Status != feed.ReportOpen {
				t.Fatalf("after the refusals = %+v, %v", v.Report, err)
			}
			page, err := e.svc.ListReports(ctx, a, feed.ReportQuery{})
			if err != nil || len(page.Items) != 0 {
				t.Fatalf("the queue shows the report: %+v, %v", page, err)
			}
			if _, err := e.svc.ResolveReport(ctx, persona(t, "moderator"), id, feed.ResolveInput{Status: feed.ReportDismissed}); err != nil {
				t.Fatal(err)
			}
		})
	}

	// A member of another company works the report; the page keeps its
	// position when it holds only a report left out.
	e := newEnv(t)
	first := messageReportOfCompanyA(ctx, t, e)
	second := messageReportOfCompanyA(ctx, t, e)
	e.parties.set(second, feed.MessageReportParty{OrgID: orgID("company_b"), StudentUserID: uuid.NewV7()})
	other := member("moderator", orgID("company_c"))
	if _, err := e.svc.GetReport(ctx, other, first); err != nil {
		t.Fatal(err)
	}
	a := member("moderator", orgID("company_a"))
	page, err := e.svc.ListReports(ctx, a, feed.ReportQuery{Limit: 1})
	if err != nil || len(page.Items) != 0 || page.Next == nil {
		t.Fatalf("first page = %+v, %v", page, err)
	}
	page, err = e.svc.ListReports(ctx, a, feed.ReportQuery{Limit: 1, After: page.Next})
	if err != nil || len(page.Items) != 1 || page.Items[0].Report.ID != second {
		t.Fatalf("second page = %+v, %v", page, err)
	}
}

// TestMessageReport_SidesUnknown (BE-183): without messaging nobody decides on
// a message report (503), and the queue answers the same; a post report does
// not depend on it.
func TestMessageReport_SidesUnknown(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e := newEnvWith(t, func(d *feed.Deps) { d.MessageParties = nil })
	mod := persona(t, "moderator")
	post := e.published(t, "club_admin", "club_a", feed.PostInput{Kind: feed.KindText, Body: doc("Duyuru")})
	r := e.report(t, "student", post.Post.ID)
	if _, err := e.svc.GetReport(ctx, mod, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ListReports(ctx, mod, feed.ReportQuery{}); err != nil {
		t.Fatal(err)
	}
	id := messageReportOfCompanyA(ctx, t, e)
	_, err := e.svc.GetReport(ctx, mod, id)
	problemtest.WantErr(t, err, problem.ErrUnavailable)
	_, err = e.svc.ResolveReport(ctx, mod, id, feed.ResolveInput{Status: feed.ReportDismissed})
	problemtest.WantErr(t, err, problem.ErrUnavailable)
	_, err = e.svc.ListReports(ctx, mod, feed.ReportQuery{})
	problemtest.WantErr(t, err, problem.ErrUnavailable)
}

// TestMessageReportFacts (BE-183): the reporter and the status of a message
// report; another target's report answers like a missing one.
func TestMessageReportFacts(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	id := messageReportOfCompanyA(ctx, t, e)
	f, err := e.svc.MessageReportFacts(ctx, id)
	if err != nil || f.ReporterUserID != persona(t, "student").UserID || f.Status != feed.ReportOpen {
		t.Fatalf("facts = %+v, %v", f, err)
	}
	post := e.published(t, "club_admin", "club_a", feed.PostInput{Kind: feed.KindText, Body: doc("Duyuru")})
	_, err = e.svc.MessageReportFacts(ctx, e.report(t, "student", post.Post.ID).ID)
	problemtest.WantErr(t, err, feed.ErrNotFound)
	_, err = e.svc.MessageReportFacts(ctx, uuid.NewV7())
	problemtest.WantErr(t, err, feed.ErrNotFound)
}
