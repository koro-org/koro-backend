package feed_test

import (
	"slices"
	"testing"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/feed"
)

// joined is persona name with one more membership.
func joined(t *testing.T, name, org string, kind authz.OrgKind, role authz.MemberRole) authz.Actor {
	t.Helper()
	a := persona(t, name)
	a.Memberships = append(slices.Clone(a.Memberships), authz.Membership{OrgID: orgID(org), OrgKind: kind, Role: role})
	return a
}

// wantStakeRefused checks that a neither reads nor resolves report id (403
// with a denied record each, without org scope) and does not see it in the
// queue, and that another moderator then works it.
func wantStakeRefused(t *testing.T, e *env, a authz.Actor, id uuid.UUID) {
	t.Helper()
	ctx := t.Context()
	_, err := e.svc.GetReport(ctx, a, id)
	wantForbidden(t, err)
	_, err = e.svc.ResolveReport(ctx, a, id, feed.ResolveInput{Status: feed.ReportDismissed})
	wantForbidden(t, err)
	for _, act := range []authz.Action{authz.FeedReportRead, authz.FeedReportResolve} {
		ev := e.audit.Of(act)
		if len(ev) != 1 || ev[0].Outcome != audit.Denied || ev[0].OrgID != nil || *ev[0].ResourceID != id {
			t.Fatalf("%s audit = %+v", act, ev)
		}
	}
	page, err := e.svc.ListReports(ctx, a, feed.ReportQuery{})
	if err != nil || slices.ContainsFunc(page.Items, func(v feed.ReportView) bool { return v.Report.ID == id }) {
		t.Fatalf("the queue shows the report: %+v, %v", page.Items, err)
	}
	mod := persona(t, "moderator")
	v, err := e.svc.GetReport(ctx, mod, id)
	if err != nil || v.Report.Status != feed.ReportOpen || v.Target == nil {
		t.Fatalf("another moderator reads = %+v, %v", v, err)
	}
	if _, err := e.svc.ResolveReport(ctx, mod, id, feed.ResolveInput{Status: feed.ReportDismissed}); err != nil {
		t.Fatalf("another moderator resolves: %v", err)
	}
}

// TestReport_PostStakeForbidden (BE-194): a member of the reported post's
// author org (any role), its author and its reporter neither read nor
// resolve the report, whatever their platform role; another moderator
// works it.
func TestReport_PostStakeForbidden(t *testing.T) {
	t.Parallel()
	author := persona(t, "moderator")
	author.UserID = persona(t, "club_admin").UserID
	filer := staffActor(authz.PlatformModerator)
	for name, c := range map[string]struct{ a, reporter authz.Actor }{
		"member of the author org":         {a: joined(t, "moderator", "club_a", authz.OrgClub, authz.MemberMember)},
		"platform admin of the author org": {a: joined(t, "platform_admin", "club_a", authz.OrgClub, authz.MemberClubAdmin)},
		"the author":                       {a: author},
		"the reporter":                     {a: filer, reporter: filer},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			post := e.published(t, "club_admin", "club_a", textInput("Duyuru")).Post.ID
			reporter := c.reporter
			if !reporter.IsUser() {
				reporter = persona(t, "student")
			}
			r, err := e.svc.CreateReport(t.Context(), reporter, feed.ReportInput{TargetType: feed.TargetPost, TargetID: post,
				Reason: feed.ReasonSpam})
			if err != nil {
				t.Fatal(err)
			}
			wantStakeRefused(t, e, c.a, r.ID)
		})
	}
}

// TestReport_OrgStakeForbidden (BE-194): a member of the reported org (any
// role) and the reporter neither read nor resolve an org report, whatever
// their platform role; a member of another org works it.
func TestReport_OrgStakeForbidden(t *testing.T) {
	t.Parallel()
	filer := staffActor(authz.PlatformModerator)
	for name, c := range map[string]struct{ a, reporter authz.Actor }{
		"member of the org":         {a: joined(t, "moderator", "company_a", authz.OrgCompany, authz.MemberRecruiter)},
		"platform admin of the org": {a: joined(t, "platform_admin", "company_a", authz.OrgCompany, authz.MemberCompanyAdmin)},
		"the reporter":              {a: filer, reporter: filer},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			reporter := c.reporter
			if !reporter.IsUser() {
				reporter = persona(t, "student")
			}
			r, err := e.svc.CreateReport(t.Context(), reporter, feed.ReportInput{TargetType: feed.TargetOrg,
				TargetID: orgID("company_a"), Reason: feed.ReasonScamOrMisleading})
			if err != nil {
				t.Fatal(err)
			}
			wantStakeRefused(t, e, c.a, r.ID)
		})
	}

	e := newEnv(t)
	ctx := t.Context()
	r, err := e.svc.CreateReport(ctx, persona(t, "student"), feed.ReportInput{TargetType: feed.TargetOrg,
		TargetID: orgID("company_a"), Reason: feed.ReasonSpam})
	if err != nil {
		t.Fatal(err)
	}
	other := joined(t, "moderator", "company_b", authz.OrgCompany, authz.MemberCompanyAdmin)
	if _, err := e.svc.GetReport(ctx, other, r.ID); err != nil {
		t.Fatal(err)
	}
	if v, err := e.svc.ResolveReport(ctx, other, r.ID, feed.ResolveInput{Status: feed.ReportInReview}); err != nil ||
		v.Report.Status != feed.ReportInReview {
		t.Fatalf("in_review = %+v, %v", v.Report, err)
	}
}

// TestReport_StakeLeftOutOfTheQueue (BE-194, BE-183's listing): a report the
// caller has a stake in leaves the page, which keeps its position: a page
// may hold none and still have a next one. Another moderator's queue holds
// both.
func TestReport_StakeLeftOutOfTheQueue(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	post := e.published(t, "club_admin", "club_a", textInput("Duyuru")).Post.ID
	first := e.report(t, "student", post)
	second, err := e.svc.CreateReport(ctx, persona(t, "student"), feed.ReportInput{TargetType: feed.TargetOrg,
		TargetID: orgID("company_b"), Reason: feed.ReasonSpam})
	if err != nil {
		t.Fatal(err)
	}
	a := joined(t, "moderator", "club_a", authz.OrgClub, authz.MemberMember)
	page, err := e.svc.ListReports(ctx, a, feed.ReportQuery{Limit: 1})
	if err != nil || len(page.Items) != 0 || page.Next == nil {
		t.Fatalf("first page = %+v, %v", page, err)
	}
	page, err = e.svc.ListReports(ctx, a, feed.ReportQuery{Limit: 1, After: page.Next})
	if err != nil || len(page.Items) != 1 || page.Items[0].Report.ID != second.ID {
		t.Fatalf("second page = %+v, %v", page, err)
	}
	b := joined(t, "moderator", "company_b", authz.OrgCompany, authz.MemberRecruiter)
	page, err = e.svc.ListReports(ctx, b, feed.ReportQuery{})
	if err != nil || len(page.Items) != 1 || page.Items[0].Report.ID != first.ID {
		t.Fatalf("company_b's member's queue = %+v, %v", page, err)
	}
	page, err = e.svc.ListReports(ctx, persona(t, "moderator"), feed.ReportQuery{})
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("another moderator's queue = %+v, %v", page, err)
	}
}

// TestReport_PostGone_ReporterRuleOnly (BE-194): a post report whose post is
// gone names no author org nor author (BE-183's rule for a message without a
// kept context): its reporter is still refused, any other moderator reads it.
func TestReport_PostGone_ReporterRuleOnly(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	filer := staffActor(authz.PlatformModerator)
	id := uuid.NewV7()
	if err := e.mem.PutReport(feed.Report{ID: id, ReporterUserID: filer.UserID, TargetType: feed.TargetPost,
		TargetID: uuid.NewV7(), Reason: feed.ReasonSpam, Status: feed.ReportOpen, CreatedAt: t0, UpdatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.GetReport(ctx, filer, id)
	wantForbidden(t, err)
	member := joined(t, "moderator", "club_a", authz.OrgClub, authz.MemberClubAdmin)
	if v, err := e.svc.GetReport(ctx, member, id); err != nil || v.Target != nil {
		t.Fatalf("GetReport = %+v, %v", v, err)
	}
}
