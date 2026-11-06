package feed_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/directory"
	"koro/internal/feed"
	"koro/internal/files"
	"koro/internal/orgs"
	"koro/internal/platform/privacy"
)

// orgReport files a's report on org.
func (e *env) orgReport(t *testing.T, actor string, org uuid.UUID) feed.ReportResult {
	t.Helper()
	r, err := e.svc.CreateReport(t.Context(), persona(t, actor), feed.ReportInput{TargetType: feed.TargetOrg, TargetID: org,
		Reason: feed.ReasonScamOrMisleading})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestReport_OrgSnapshotKeepsReportedProfile (BE-180, 00 T-401): an org
// report keeps the org's name, description, website, social links and logo
// file as they stood when it was filed; an update of the org changes what the
// moderator sees as current, not the snapshot; the reporter's open report
// returned again keeps its first snapshot; another user's report after the
// update holds the new profile; resolving keeps it. It is shown on the
// moderators' report view only: not in the queue list, the reporter's KVKK
// export or a post report's author org. The reporter's erasure takes it.
func TestReport_OrgSnapshotKeepsReportedProfile(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	mod := persona(t, "moderator")
	org := orgID("club_a")
	oldLogo, newLogo := e.files.upload(org, files.ScanClean), e.files.upload(org, files.ScanClean)
	oldDesc, oldSite := "Başvuru ücreti 500 TL", "https://odeme.koro.test"
	e.orgs.setProfile(org, orgs.Profile{Kind: orgs.KindClub, Name: "Sahte Kulüp", Description: &oldDesc, WebsiteURL: &oldSite, LogoFileID: &oldLogo})
	e.clubs.setLinks(org, directory.SocialLinks{"instagram": "https://i.koro.test/sahte"})

	reportedAt := e.clk.Now()
	first := e.orgReport(t, "student", org)
	if !first.Created {
		t.Fatalf("first = %+v", first)
	}

	// The org changes everything.
	e.clk.Advance(time.Minute)
	newDesc := "Kulübümüz"
	e.orgs.setProfile(org, orgs.Profile{Kind: orgs.KindClub, Name: "Masum Kulüp", Description: &newDesc, LogoFileID: &newLogo})
	e.clubs.setLinks(org, directory.SocialLinks{})

	// The same reporter again: the open report, its first snapshot.
	e.clk.Advance(time.Minute)
	if again := e.orgReport(t, "student", org); again.Created || again.ID != first.ID {
		t.Fatalf("again = %+v", again)
	}
	other := e.orgReport(t, "club_member", org)

	view, err := e.svc.GetReport(ctx, mod, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	o := feed.ToReportDTO(view).Target.Org
	if o.Description != newDesc || o.WebsiteURL != "" || len(o.SocialLinks) != 0 || o.LogoFileID != newLogo.String() || o.LogoURL == "" {
		t.Fatalf("current org = %+v", o)
	}
	s := o.ReportedSnapshot
	if s == nil || s.Name != "Sahte Kulüp" || s.Description != oldDesc || s.WebsiteURL != oldSite ||
		s.SocialLinks["instagram"] != "https://i.koro.test/sahte" || s.LogoFileID != oldLogo.String() ||
		!strings.Contains(s.LogoURL, oldLogo.String()) || !s.TakenAt.Equal(reportedAt) {
		t.Fatalf("reported snapshot = %+v", s)
	}
	ov, err := e.svc.GetReport(ctx, mod, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if s := feed.ToReportDTO(ov).Target.Org.ReportedSnapshot; s == nil || s.Name != "Masum Kulüp" || s.Description != newDesc ||
		s.WebsiteURL != "" || len(s.SocialLinks) != 0 || s.LogoFileID != newLogo.String() {
		t.Fatalf("snapshot of the report after the update = %+v", s)
	}

	// Resolving keeps it (adminResolveReport answers the same view). A file
	// gone since leaves its ID without a URL.
	if err := e.files.Delete(ctx, mod, oldLogo); err != nil {
		t.Fatal(err)
	}
	res, err := e.svc.ResolveReport(ctx, mod, first.ID, feed.ResolveInput{Status: feed.ReportDismissed})
	if err != nil {
		t.Fatal(err)
	}
	if s := feed.ToReportDTO(res).Target.Org.ReportedSnapshot; s == nil || s.Name != "Sahte Kulüp" || s.LogoFileID != oldLogo.String() || s.LogoURL != "" {
		t.Fatalf("snapshot after the resolution = %+v", s)
	}

	// The queue list has no target, so no snapshot.
	page, err := e.svc.ListReports(ctx, mod, feed.ReportQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range page.Items {
		d, _ := json.Marshal(feed.ToReportDTO(it))
		if strings.Contains(string(d), "reported_snapshot") || strings.Contains(string(d), "Sahte") {
			t.Fatalf("the queue list shows a snapshot: %s", d)
		}
	}

	// The reporter's export lists the report, not the org's profile.
	student := persona(t, "student").UserID
	exp, err := e.mod.Privacy.Export(ctx, student)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(exp.Items)
	if !strings.Contains(string(out), first.ID.String()) || strings.Contains(string(out), "Sahte") ||
		strings.Contains(string(out), "odeme.koro.test") || strings.Contains(string(out), oldLogo.String()) {
		t.Fatalf("export = %s", out)
	}

	// A post report's author org carries no profile, kept logo or snapshot.
	v := e.published(t, "club_admin", "club_a", textInput("duyuru"))
	pr := e.report(t, "student", v.Post.ID)
	pv, err := e.svc.GetReport(ctx, mod, pr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if o := feed.ToReportDTO(pv).Target.Org; o.ReportedSnapshot != nil || o.Description != "" || o.LogoURL != "" || len(o.DroppedLogos) != 0 {
		t.Fatalf("author org of a post report = %+v", o)
	}

	// The reporter's erasure takes the report with its snapshot.
	if _, err := e.mod.Privacy.Erase(ctx, student); err != nil {
		t.Fatal(err)
	}
	if _, err := e.mem.GetReport(ctx, first.ID); !errors.Is(err, feed.ErrNotFound) {
		t.Fatalf("report after the reporter's erasure: %v", err)
	}
}

// TestReport_OrgSnapshotOfOtherKinds: only a club has social links, and a
// club directory does not know has none: the snapshot does not fail for it.
func TestReport_OrgSnapshotOfOtherKinds(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, org := range []string{"company_a", "club_b"} {
		if org == "company_a" {
			e.clubs.setLinks(orgID(org), directory.SocialLinks{"x": "https://x.koro.test/a"}) // never read for a company
		}
		r := e.orgReport(t, "student", orgID(org))
		got, err := e.mem.GetReport(t.Context(), r.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.SnapshotAt == nil || got.SnapshotOrgName == nil || *got.SnapshotOrgName != "Org "+org ||
			got.SnapshotOrgSocialLinks != nil || got.SnapshotOrgLogoFileID != nil {
			t.Fatalf("%s snapshot = %+v at %v", org, got.OrgSnapshot, got.SnapshotAt)
		}
	}
}

// TestReportDTO_NoOrgSnapshotBeforeBE180: an org report stored without a
// snapshot (filed before BE-180) shows the current org and no
// reported_snapshot.
func TestReportDTO_NoOrgSnapshotBeforeBE180(t *testing.T) {
	t.Parallel()
	id := uuid.NewV7()
	d := feed.ToReportDTO(feed.ReportView{
		Report: feed.Report{ID: uuid.NewV7(), TargetType: feed.TargetOrg, TargetID: id, Reason: feed.ReasonSpam, Status: feed.ReportOpen},
		Target: &feed.TargetSummary{Type: feed.TargetOrg, Org: orgs.OrgSummary{ID: id, Name: "Kulüp"}},
	})
	if d.Target.Org.Name != "Kulüp" || d.Target.Org.ReportedSnapshot != nil {
		t.Fatalf("legacy report DTO = %+v", d.Target.Org)
	}
	b, _ := json.Marshal(d)
	if strings.Contains(string(b), "reported_snapshot") || strings.Contains(string(b), "dropped_logos") {
		t.Fatalf("legacy report JSON = %s", b)
	}
}

// TestKeepReplacedLogo (BE-180, T-401): a logo the org replaces is kept for
// the moderators only while a report on the org is open or in review: the
// file is retired, its row shown on the report view; without one, or once
// resolved, or with a report on one of its posts only, the caller deletes it.
// The audit record has no org scope: the org's trail must not say it was
// reported.
func TestKeepReplacedLogo(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	admin, mod := persona(t, "club_admin"), persona(t, "moderator")
	org := orgID("club_a")
	up := func() uuid.UUID { return e.files.upload(org, files.ScanClean) }
	keep := func(file uuid.UUID, want bool) {
		t.Helper()
		kept, err := e.svc.KeepReplacedLogo(ctx, admin, org, file)
		if err != nil || kept != want {
			t.Fatalf("KeepReplacedLogo = %v, %v; want %v", kept, err, want)
		}
	}
	// No report: nothing kept.
	noReport := up()
	keep(noReport, false)
	// A report on a post of the org is not a report on the org.
	v := e.published(t, "club_admin", "club_a", textInput("duyuru"))
	e.report(t, "student", v.Post.ID)
	keep(noReport, false)
	if len(e.files.retired) != 0 {
		t.Fatalf("retired without an org report: %v", e.files.retired)
	}
	if got, _ := e.mem.DroppedLogosOf(ctx, org); len(got) != 0 {
		t.Fatalf("kept without an org report: %+v", got)
	}

	// An open report on the org: kept and retired.
	r := e.orgReport(t, "student", org)
	logo := up()
	at := e.clk.Now()
	keep(logo, true)
	if !slices.Equal(e.files.retired, []uuid.UUID{logo}) {
		t.Fatalf("retired = %v, want the logo", e.files.retired)
	}
	if got, _ := e.mem.DroppedLogosOf(ctx, org); len(got) != 1 || got[0].FileID != logo || got[0].OrgID != org || !got[0].DroppedAt.Equal(at) {
		t.Fatalf("kept logos = %+v", got)
	}
	last := e.audit.Last()
	if last.Action != "feed.org.keep_replaced_logo" || last.OrgID != nil || last.ResourceID == nil || *last.ResourceID != logo ||
		last.Metadata["org_id"] != org.String() {
		t.Fatalf("audit = %+v", last)
	}
	if b, _ := json.Marshal(last.Metadata); strings.Contains(string(b), persona(t, "student").UserID.String()) {
		t.Fatalf("the audit names the reporter: %s", b)
	}
	// Twice is a conflict of the key, never a silent second row.
	if kept, err := e.svc.KeepReplacedLogo(ctx, admin, org, logo); err == nil || kept {
		t.Fatalf("kept twice = %v, %v", kept, err)
	}

	// The moderators see it, with a URL while the scan clears it.
	view, err := e.svc.GetReport(ctx, mod, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	dl := feed.ToReportDTO(view).Target.Org.DroppedLogos
	if len(dl) != 1 || dl[0].FileID != logo.String() || !strings.Contains(dl[0].URL, logo.String()) || !dl[0].DroppedAt.Equal(at) {
		t.Fatalf("dropped_logos = %+v", dl)
	}
	// Nobody else reads the report: the org's admin, its reporter.
	for _, who := range []string{"club_admin", "student"} {
		if _, err := e.svc.GetReport(ctx, persona(t, who), r.ID); !errors.Is(err, authz.ErrForbidden) && !errors.Is(err, authz.ErrNotFound) {
			t.Fatalf("GetReport as %s = %v", who, err)
		}
	}

	// In review it still counts; resolved it no longer does.
	if _, err := e.svc.ResolveReport(ctx, mod, r.ID, feed.ResolveInput{Status: feed.ReportInReview}); err != nil {
		t.Fatal(err)
	}
	keep(up(), true)
	if _, err := e.svc.ResolveReport(ctx, mod, r.ID, feed.ResolveInput{Status: feed.ReportDismissed}); err != nil {
		t.Fatal(err)
	}
	keep(up(), false)
	if len(e.files.deleted) != 0 {
		t.Fatalf("the keeper deleted files: %v (the caller does)", e.files.deleted)
	}
}

// TestPurgeDroppedLogos_WaitsForHoldingReports (org_logo_dropped, BE-180): a
// kept logo goes with its file 12 months after it left, unless a report filed
// before that is open, in review or resolved within the period: it lasts as
// long as the report it may be evidence for. A report filed after it left
// does not hold it. Unapproved, the sweep only counts.
func TestPurgeDroppedLogos_WaitsForHoldingReports(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	admin, mod := persona(t, "club_admin"), persona(t, "moderator")
	held, gone := orgID("club_a"), orgID("club_b")
	keep := func(org uuid.UUID) uuid.UUID {
		t.Helper()
		f := e.files.upload(org, files.ScanClean)
		if kept, err := e.svc.KeepReplacedLogo(ctx, admin, org, f); err != nil || !kept {
			t.Fatalf("KeepReplacedLogo = %v, %v", kept, err)
		}
		return f
	}
	r := e.orgReport(t, "student", held)
	heldLogo := keep(held)
	r2 := e.orgReport(t, "student", gone)
	goneLogo := keep(gone)
	if _, err := e.svc.ResolveReport(ctx, mod, r2.ID, feed.ResolveInput{Status: feed.ReportDismissed}); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(13 * 30 * 24 * time.Hour)
	// A report after the logo left cannot be about it.
	e.orgReport(t, "student", gone)

	sweep := func(approved bool, want int) privacy.SweepResult {
		t.Helper()
		now := e.clk.Now()
		res, err := e.mod.Sweeper.Sweep(ctx, privacy.RetentionPolicy{Code: feed.PolicyLogoDropped, Retention: now.Sub(now.AddDate(0, -12, 0)),
			LegalApproved: approved}, now)
		if err != nil || res.Count != want || res.Table != "org_dropped_logos" || res.Method != privacy.MethodHardDelete {
			t.Fatalf("sweep = %+v, %v; want %d", res, err, want)
		}
		return res
	}
	sweep(false, 1)
	if len(e.files.deleted) != 0 {
		t.Fatalf("a dry run deleted %v", e.files.deleted)
	}
	sweep(true, 1)
	if !slices.Equal(e.files.deleted, []uuid.UUID{goneLogo}) {
		t.Fatalf("deleted = %v, want the unheld logo", e.files.deleted)
	}
	if got, _ := e.mem.DroppedLogosOf(ctx, gone); len(got) != 0 {
		t.Fatalf("rows of the purged logo: %+v", got)
	}

	// Resolved now, the report still holds the logo for its own period.
	if _, err := e.svc.ResolveReport(ctx, mod, r.ID, feed.ResolveInput{Status: feed.ReportDismissed}); err != nil {
		t.Fatal(err)
	}
	sweep(true, 0)
	e.clk.Advance(13 * 30 * 24 * time.Hour)
	sweep(true, 1)
	if !slices.Contains(e.files.deleted, heldLogo) {
		t.Fatalf("deleted = %v, want the held logo too", e.files.deleted)
	}
	if res := sweep(true, 0); res.PolicyCode != feed.PolicyLogoDropped {
		t.Fatalf("rerun = %+v", res)
	}
}
