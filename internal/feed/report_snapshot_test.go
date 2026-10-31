package feed_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/events"
	"koro/internal/feed"
)

// TestReport_SnapshotKeepsReportedText (BE-160, 00 T-398): a post report
// keeps the post's title, plain text and opportunity as they stood when
// it was filed; an edit of the published post changes the post the
// moderator sees, not the snapshot; the reporter's open report returned
// again keeps its first snapshot; another user's report after the edit
// holds the edited text; resolving the report keeps it; an org report has
// none. It is shown on the moderators' report view only: not in the queue
// list nor in the reporter's KVKK export.
func TestReport_SnapshotKeepsReportedText(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	admin, mod := persona(t, "club_admin"), persona(t, "moderator")
	const reportedBody = "Başvuru ücreti 500 TL, IBAN'a yatırın"
	v := e.published(t, "club_admin", "club_a", feed.PostInput{Kind: feed.KindOpportunityCall, Title: "Ücretli staj",
		Body: doc(reportedBody), Opportunity: &feed.Opportunity{Type: "internship", ApplyVia: "external_url",
			ExternalURL: "https://odeme.koro.test/basvuru"}})
	reportedAt := e.clk.Now()
	first := e.report(t, "student", v.Post.ID)

	// The author edits every text field of the published post.
	e.clk.Advance(time.Minute)
	if _, err := e.svc.UpdatePost(ctx, admin, v.Post.ID, v.ETag, feed.PostPatch{Title: new("Staj"), Body: doc("Başvuru formu"),
		Opportunity: &feed.Opportunity{Type: "internship", ApplyVia: "external_url", ExternalURL: "https://forms.koro.test/staj"}}); err != nil {
		t.Fatal(err)
	}
	// The same reporter again: the open report, its first snapshot.
	e.clk.Advance(time.Minute)
	if again := e.report(t, "student", v.Post.ID); again.Created || again.ID != first.ID {
		t.Fatalf("again = %+v", again)
	}
	other := e.report(t, "club_member", v.Post.ID)

	view, err := e.svc.GetReport(ctx, mod, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	p := feed.ToReportDTO(view).Target.Post
	if p.Title != "Staj" || p.BodyText != "Başvuru formu" || p.Opportunity == nil ||
		p.Opportunity.ExternalURL != "https://forms.koro.test/staj" || p.EditedAt == nil {
		t.Fatalf("current post = %+v", p)
	}
	s := p.ReportedSnapshot
	if s == nil || s.Title != "Ücretli staj" || s.BodyText != reportedBody || s.Opportunity == nil ||
		s.Opportunity.ExternalURL != "https://odeme.koro.test/basvuru" || s.Opportunity.ApplyVia != "external_url" ||
		!s.TakenAt.Equal(reportedAt) || !p.EditedAt.After(s.TakenAt) || s.EventID != "" {
		t.Fatalf("reported snapshot = %+v", s)
	}
	ov, err := e.svc.GetReport(ctx, mod, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if s := feed.ToReportDTO(ov).Target.Post.ReportedSnapshot; s == nil || s.Title != "Staj" || s.BodyText != "Başvuru formu" ||
		s.Opportunity.ExternalURL != "https://forms.koro.test/staj" {
		t.Fatalf("snapshot of the report after the edit = %+v", s)
	}

	// Resolving keeps it (adminResolveReport answers the same view).
	res, err := e.svc.ResolveReport(ctx, mod, first.ID, feed.ResolveInput{Status: feed.ReportDismissed})
	if err != nil {
		t.Fatal(err)
	}
	if s := feed.ToReportDTO(res).Target.Post.ReportedSnapshot; s == nil || s.Title != "Ücretli staj" {
		t.Fatalf("snapshot after the resolution = %+v", s)
	}

	// The queue list has no target, so no snapshot.
	page, err := e.svc.ListReports(ctx, mod, feed.ReportQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range page.Items {
		d, _ := json.Marshal(feed.ToReportDTO(it))
		if strings.Contains(string(d), "reported_snapshot") || strings.Contains(string(d), "IBAN") {
			t.Fatalf("the queue list shows a snapshot: %s", d)
		}
	}

	// The reporter's export lists the report, not the reported org's text.
	exp, err := e.mod.Privacy.Export(ctx, persona(t, "student").UserID)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(exp.Items)
	if !strings.Contains(string(out), first.ID.String()) || strings.Contains(string(out), "IBAN") ||
		strings.Contains(string(out), "odeme.koro.test") || strings.Contains(string(out), "Ücretli staj") {
		t.Fatalf("export = %s", out)
	}

	// An org report has no snapshot.
	ro, err := e.svc.CreateReport(ctx, persona(t, "student"), feed.ReportInput{TargetType: feed.TargetOrg, TargetID: orgID("club_a"),
		Reason: feed.ReasonSpam})
	if err != nil {
		t.Fatal(err)
	}
	if r, err := e.mem.GetReport(ctx, ro.ID); err != nil || r.SnapshotAt != nil {
		t.Fatalf("org report = %+v, %v", r.PostSnapshot, err)
	}
}

// TestReport_SnapshotKeepsSharedEvent (BE-160): an event_share post that
// swaps its event after a report still shows the moderator which event
// was reported; a post without text keeps an empty snapshot with its time.
func TestReport_SnapshotKeepsSharedEvent(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	start := time.Date(2026, 11, 20, 15, 0, 0, 0, time.UTC)
	reported := events.EventSummary{ID: uuid.NewV7(), OrgID: orgID("club_b"), Title: "Kumar gecesi", Slug: "kumar-gecesi",
		Status: events.StatusPublished, StartsAt: start, EndsAt: start.Add(time.Hour), Visibility: events.VisibilityPublic}
	benign := reported
	benign.ID, benign.Title, benign.Slug = uuid.NewV7(), "Kitap kulübü", "kitap-kulubu"
	e.events.put(reported)
	e.events.put(benign)
	v := e.published(t, "club_admin", "club_a", feed.PostInput{Kind: feed.KindEventShare, EventID: &reported.ID})
	at := e.clk.Now()
	r := e.report(t, "student", v.Post.ID)
	e.clk.Advance(time.Minute)
	if _, err := e.svc.UpdatePost(ctx, persona(t, "club_admin"), v.Post.ID, v.ETag, feed.PostPatch{EventID: &benign.ID}); err != nil {
		t.Fatal(err)
	}
	view, err := e.svc.GetReport(ctx, persona(t, "moderator"), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	p := feed.ToReportDTO(view).Target.Post
	if p.EventID != benign.ID.String() {
		t.Fatalf("current event = %q", p.EventID)
	}
	if s := p.ReportedSnapshot; s == nil || s.EventID != reported.ID.String() || s.Title != "" || s.BodyText != "" ||
		s.Opportunity != nil || !s.TakenAt.Equal(at) {
		t.Fatalf("reported snapshot = %+v", s)
	}
}

// TestReportDTO_NoSnapshotBeforeBE160: a post report stored without a
// snapshot (filed before BE-160) shows the current post and no
// reported_snapshot.
func TestReportDTO_NoSnapshotBeforeBE160(t *testing.T) {
	t.Parallel()
	title := "Duyuru"
	p := feed.Post{ID: uuid.NewV7(), Kind: feed.KindText, Status: feed.StatusPublished, Title: &title}
	d := feed.ToReportDTO(feed.ReportView{
		Report: feed.Report{ID: uuid.NewV7(), TargetType: feed.TargetPost, TargetID: p.ID, Reason: feed.ReasonSpam,
			Status: feed.ReportOpen},
		Target: &feed.TargetSummary{Type: feed.TargetPost, Post: &p},
	})
	if d.Target.Post == nil || d.Target.Post.Title != title || d.Target.Post.ReportedSnapshot != nil {
		t.Fatalf("legacy report DTO = %+v", d.Target.Post)
	}
	b, _ := json.Marshal(d)
	if strings.Contains(string(b), "reported_snapshot") {
		t.Fatalf("legacy report JSON = %s", b)
	}
}
