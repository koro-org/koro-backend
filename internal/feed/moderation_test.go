package feed_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/feed"
	"koro/internal/notify"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
)

// report files a report of a published post by actor.
func (e *env) report(t *testing.T, actor string, target uuid.UUID) feed.ReportResult {
	t.Helper()
	r, err := e.svc.CreateReport(t.Context(), persona(t, actor), feed.ReportInput{
		TargetType: feed.TargetPost, TargetID: target, Reason: feed.ReasonScamOrMisleading, Details: "Ücret istiyorlar",
	})
	if err != nil {
		t.Fatalf("CreateReport(%s): %v", actor, err)
	}
	return r
}

// moderate takes a decision on a post as actor.
func (e *env) moderate(t *testing.T, actor string, post uuid.UUID, act feed.ModAction, reason string) feed.PostModeration {
	t.Helper()
	m, err := e.svc.ModeratePost(t.Context(), persona(t, actor), post, feed.ModerationInput{Action: act, ReasonCode: reason})
	if err != nil {
		t.Fatalf("ModeratePost(%s %s): %v", actor, act, err)
	}
	return m
}

// staffActor is a user with the given platform role and memberships
// (MFA-verified, as platform roles require).
func staffActor(role authz.PlatformRole, ms ...authz.Membership) authz.Actor {
	return authz.Actor{Kind: authz.KindUser, UserID: uuid.NewV7(), PlatformRoles: []authz.PlatformRole{role},
		Memberships: ms, MFAVerified: true}
}

// TestReport_DuplicateOpen_409: one user has one open report per target
// (content_reports_open_uq). The ticket names the duplicate "409"; 04 §2.7
// and §F.4 (the contract, 06 H-11) make it idempotent: reporting again
// returns the same report_id with 200 (Created=false), no second row, no
// second audit record or event. The database's guard against a second open
// row answers 409 (TestReport_DuplicateOpen_409_Gorm). After a decision a
// new report is a new row.
func TestReport_DuplicateOpen_409(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	post := e.published(t, "club_admin", "club_a", textInput("Duyuru")).Post.ID
	first := e.report(t, "student", post)
	again := e.report(t, "student", post)
	if !first.Created || again.Created || again.ID != first.ID {
		t.Fatalf("first %+v, again %+v", first, again)
	}
	if n := len(e.audit.of(authz.FeedReportCreate)); n != 1 {
		t.Fatalf("audit records = %d", n)
	}
	if n := len(slices.DeleteFunc(e.out.Topics(), func(s string) bool { return s != domainevent.TopicContentReportCreated })); n != 1 {
		t.Fatalf("content_report.created = %d", n)
	}
	// Another user, or another target, is another report.
	if other := e.report(t, "club_member", post); !other.Created || other.ID == first.ID {
		t.Fatalf("other reporter = %+v", other)
	}
	// Once dismissed, the same user may report again.
	if _, err := e.svc.ResolveReport(t.Context(), persona(t, "moderator"), first.ID,
		feed.ResolveInput{Status: feed.ReportDismissed}); err != nil {
		t.Fatal(err)
	}
	if next := e.report(t, "student", post); !next.Created || next.ID == first.ID {
		t.Fatalf("report after dismissal = %+v", next)
	}
	// A report on an org: the same rule.
	org := feed.ReportInput{TargetType: feed.TargetOrg, TargetID: orgID("company_a"), Reason: feed.ReasonSpam}
	a, err := e.svc.CreateReport(t.Context(), persona(t, "student"), org)
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.svc.CreateReport(t.Context(), persona(t, "student"), org)
	if err != nil || b.ID != a.ID || b.Created {
		t.Fatalf("org duplicate = %+v, %v", b, err)
	}
	// An Idempotency-Key replay answers the stored result.
	key := idempotency.WithKey(t.Context(), idempotency.Key{Scope: "student:POST /api/v1/reports", Value: "rep-1", Status: 201})
	in := feed.ReportInput{TargetType: feed.TargetOrg, TargetID: orgID("club_b"), Reason: feed.ReasonHate}
	x, err := e.svc.CreateReport(key, persona(t, "student"), in)
	if err != nil {
		t.Fatal(err)
	}
	y, err := e.svc.CreateReport(key, persona(t, "student"), in)
	if err != nil || y.ID != x.ID || !y.Created {
		t.Fatalf("replay = %+v, %v (first %+v)", y, err, x)
	}
}

// TestReport_TargetRules: the target must be a published post or an org
// the reporter sees; a draft its writer sees is 409, an invisible post
// 404; a reason outside the list and control characters are 422.
func TestReport_TargetRules(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	draft := e.create(t, "club_admin", "club_a", textInput("Taslak")).Post.ID
	_, err := e.svc.CreateReport(ctx, persona(t, "student"), feed.ReportInput{TargetType: feed.TargetPost, TargetID: draft,
		Reason: feed.ReasonSpam})
	wantNotFound(t, err)
	_, err = e.svc.CreateReport(ctx, persona(t, "club_admin"), feed.ReportInput{TargetType: feed.TargetPost, TargetID: draft,
		Reason: feed.ReasonSpam})
	wantCode(t, err, 409, "feed.invalid_state")
	_, err = e.svc.CreateReport(ctx, persona(t, "student"), feed.ReportInput{TargetType: feed.TargetOrg, TargetID: uuid.NewV7(),
		Reason: feed.ReasonSpam})
	wantNotFound(t, err)
	post := e.published(t, "club_admin", "club_a", textInput("Duyuru")).Post.ID
	_, err = e.svc.CreateReport(ctx, persona(t, "student"), feed.ReportInput{TargetType: feed.TargetPost, TargetID: post,
		Reason: "boring"})
	wantField(t, err, "body.reason")
	_, err = e.svc.CreateReport(ctx, persona(t, "student"), feed.ReportInput{TargetType: feed.TargetPost, TargetID: post,
		Reason: feed.ReasonOther, Details: "a\u202eb"})
	wantField(t, err, "body.details")
	// A commercial post is not a minor's to report (k15: not visible).
	co := e.published(t, "company_member", "company_a", textInput("Reklam")).Post.ID
	_, err = e.svc.CreateReport(ctx, persona(t, "student_minor"), feed.ReportInput{TargetType: feed.TargetPost, TargetID: co,
		Reason: feed.ReasonSpam})
	wantNotFound(t, err)
}

// TestReport_ReporterIdentityOnlyInAdmin (03 Y7, 02 §12.9): the reporter
// is in the admin report DTO only; the author org's views, the audit trail
// of the org and the events carry no reporter.
func TestReport_ReporterIdentityOnlyInAdmin(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	post := e.published(t, "club_admin", "club_a", textInput("Duyuru")).Post.ID
	reporter := persona(t, "student")
	r := e.report(t, "student", post)
	who := reporter.UserID.String()

	// Admin: the reporter is there.
	v, err := e.svc.GetReport(ctx, persona(t, "moderator"), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	d := feed.ToReportDTO(v)
	if d.ReporterUserID != who || d.Target == nil || d.Target.Post == nil || d.Target.OpenReports != 1 {
		t.Fatalf("admin report = %+v", d)
	}
	page, err := e.svc.ListReports(ctx, persona(t, "platform_admin"), feed.ReportQuery{
		Filter: feed.ReportFilter{Statuses: []feed.ReportStatus{feed.ReportOpen}}})
	if err != nil || len(page.Items) != 1 || feed.ToReportDTO(page.Items[0]).ReporterUserID != who {
		t.Fatalf("admin list = %+v, %v", page.Items, err)
	}
	// Nobody else reads the reports (admin surface: 403).
	for _, name := range []string{"club_admin", "student", "support", "dpo", "company_admin"} {
		_, err := e.svc.GetReport(ctx, persona(t, name), r.ID)
		wantForbidden(t, err)
		_, err = e.svc.ListReports(ctx, persona(t, name), feed.ReportQuery{})
		wantForbidden(t, err)
	}
	// The author org: its management views carry no reporter.
	manage, err := e.svc.ListOrgPosts(ctx, persona(t, "club_admin"), orgID("club_a"), feed.ManageQuery{})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(feed.ToManageDTO(manage.Items[0]))
	if strings.Contains(string(b), who) {
		t.Fatalf("management view leaks the reporter: %s", b)
	}
	// The audit record of a report has no org scope (never in the org's trail).
	for _, ev := range e.audit.of(authz.FeedReportCreate) {
		if ev.OrgID != nil {
			t.Fatalf("report audit with org scope: %+v", ev)
		}
	}
	// The event names the report only.
	for _, env := range e.out.Events() {
		if env.Topic == domainevent.TopicContentReportCreated && strings.Contains(string(env.Payload), who) {
			t.Fatalf("content_report.created carries the reporter: %s", env.Payload)
		}
	}
}

// TestReport_AutoHideAfterThreshold (R-003 §1.5.3): the third distinct
// reporter hides a published post until a moderator decides; the author
// org sees reported_by_users and is told; a moderator restores it.
func TestReport_AutoHideAfterThreshold(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	post := e.published(t, "club_admin", "club_a", textInput("Duyuru")).Post.ID
	e.report(t, "student", post)
	e.report(t, "student", post) // the same reporter twice counts once
	e.report(t, "club_member", post)
	if p, _ := e.mem.GetPost(ctx, post); p.Status != feed.StatusPublished {
		t.Fatalf("status after 2 reporters = %s", p.Status)
	}
	e.report(t, "student_minor", post)
	p, _ := e.mem.GetPost(ctx, post)
	if p.Status != feed.StatusHidden {
		t.Fatalf("status after 3 reporters = %s", p.Status)
	}
	if _, err := e.svc.GetPost(ctx, persona(t, "student"), post); err == nil {
		t.Fatal("a hidden post is visible to readers")
	}
	manage, err := e.svc.ListOrgPosts(ctx, persona(t, "club_admin"), orgID("club_a"), feed.ManageQuery{})
	if err != nil || manage.Items[0].ReasonCode != "reported_by_users" {
		t.Fatalf("manage = %+v, %v", manage.Items, err)
	}
	moderated := false
	for _, env := range e.out.Events() {
		if env.Topic == domainevent.TopicPostModerated {
			moderated = true
		}
	}
	if !moderated {
		t.Fatal("no post.moderated for the automatic hide")
	}
	if got := e.audit.of("feed.post.auto_hide"); len(got) != 1 || got[0].SystemJob == "" || got[0].OrgID != nil {
		t.Fatalf("auto-hide audit = %+v", got)
	}
	// The moderator restores it.
	m := e.moderate(t, "moderator", post, feed.ModRestore, "not_violating")
	if m.Post.Status != feed.StatusPublished {
		t.Fatalf("restored = %s", m.Post.Status)
	}
}

// TestReport_ResolveRules: in_review takes the report; actioned needs a
// moderation action with the report (409 otherwise); a resolved report
// takes no other change (409) but repeating its outcome is a no-op; the
// reporter does not resolve their own report; the reporter is told.
func TestReport_ResolveRules(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	post := e.published(t, "club_admin", "club_a", textInput("Duyuru")).Post.ID
	r := e.report(t, "student", post)
	mod := persona(t, "moderator")
	v, err := e.svc.ResolveReport(ctx, mod, r.ID, feed.ResolveInput{Status: feed.ReportInReview, AssignToMe: true})
	if err != nil || v.Report.Status != feed.ReportInReview || v.Report.AssignedTo == nil || *v.Report.AssignedTo != mod.UserID {
		t.Fatalf("in_review = %+v, %v", v.Report, err)
	}
	_, err = e.svc.ResolveReport(ctx, mod, r.ID, feed.ResolveInput{Status: feed.ReportActioned})
	wantCode(t, err, 409, "feed.moderation_action_required")
	if _, err := e.svc.ModeratePost(ctx, mod, post, feed.ModerationInput{Action: feed.ModHide, ReasonCode: "misleading_job",
		ReportID: &r.ID}); err != nil {
		t.Fatal(err)
	}
	v, err = e.svc.ResolveReport(ctx, mod, r.ID, feed.ResolveInput{Status: feed.ReportActioned, Note: "Gizlendi"})
	if err != nil || v.Report.Status != feed.ReportActioned || v.Report.ResolvedAt == nil {
		t.Fatalf("actioned = %+v, %v", v.Report, err)
	}
	if _, err := e.svc.ResolveReport(ctx, mod, r.ID, feed.ResolveInput{Status: feed.ReportActioned}); err != nil {
		t.Fatalf("repeated outcome: %v", err)
	}
	_, err = e.svc.ResolveReport(ctx, mod, r.ID, feed.ResolveInput{Status: feed.ReportDismissed})
	wantCode(t, err, 409, "feed.invalid_state")
	// A decision citing a resolved report is 409; citing another target's 422.
	_, err = e.svc.ModeratePost(ctx, mod, post, feed.ModerationInput{Action: feed.ModRestore, ReasonCode: "appeal",
		ReportID: &r.ID})
	wantCode(t, err, 409, "feed.invalid_state")
	other := e.published(t, "club_admin", "club_a", textInput("Diğer")).Post.ID
	r2 := e.report(t, "club_member", other)
	_, err = e.svc.ModeratePost(ctx, mod, post, feed.ModerationInput{Action: feed.ModRestore, ReasonCode: "appeal",
		ReportID: &r2.ID})
	wantField(t, err, "body.report_id")
	// A moderator does not resolve their own report.
	selfReport, err := e.svc.CreateReport(ctx, mod, feed.ReportInput{TargetType: feed.TargetPost, TargetID: other,
		Reason: feed.ReasonSpam})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.ResolveReport(ctx, mod, selfReport.ID, feed.ResolveInput{Status: feed.ReportDismissed})
	wantForbidden(t, err)
	if _, err := e.svc.ResolveReport(ctx, persona(t, "platform_admin"), selfReport.ID,
		feed.ResolveInput{Status: feed.ReportDismissed}); err != nil {
		t.Fatal(err)
	}
	// The reporters are told (a task per resolution).
	tasks := 0
	for _, task := range e.queue.Tasks() {
		if task.Type == feed.NotifyReportResolvedTaskName {
			tasks++
			if err := feed.HandleTask(e.mod, ctx, task.Type, task.Payload); err != nil {
				t.Fatal(err)
			}
		}
	}
	if tasks != 2 || len(e.sender.to("feed.report_resolved")) != 2 {
		t.Fatalf("resolution tasks = %d, notices = %v", tasks, e.sender.to("feed.report_resolved"))
	}
	if got := e.sender.to("feed.report_resolved"); !slices.Contains(got, persona(t, "student").UserID) {
		t.Fatalf("reporter not told: %v", got)
	}
}

// TestModeratePost_AppendOnlyAction: every decision appends one
// moderation_actions row (never updated) with an audit record that has no
// org scope; the history lists them newest first; the author org's view
// shows the reason code of the decision that set the status.
func TestModeratePost_AppendOnlyAction(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	post := e.published(t, "club_admin", "club_a", textInput("Duyuru")).Post.ID
	hide := e.moderate(t, "moderator", post, feed.ModHide, "spam")
	restore := e.moderate(t, "platform_admin", post, feed.ModRestore, "appeal_accepted")
	remove := e.moderate(t, "moderator", post, feed.ModRemove, "scam")
	if hide.Post.Status != feed.StatusHidden || restore.Post.Status != feed.StatusPublished ||
		remove.Post.Status != feed.StatusRemoved || remove.Post.RemovedAt == nil {
		t.Fatalf("statuses = %s %s %s", hide.Post.Status, restore.Post.Status, remove.Post.Status)
	}
	page, err := e.svc.ListModerationActions(ctx, persona(t, "moderator"), feed.ActionQuery{
		Filter: feed.ActionFilter{TargetType: feed.TargetPost, TargetID: &post}})
	if err != nil || len(page.Items) != 3 {
		t.Fatalf("history = %+v, %v", page.Items, err)
	}
	if page.Items[0].ID != remove.Action.ID || page.Items[2].ID != hide.Action.ID || page.Items[2].Action != feed.ModHide ||
		page.Items[2].ReasonCode != "spam" || page.Items[2].ActorUserID != persona(t, "moderator").UserID {
		t.Fatalf("history order/content = %+v", page.Items)
	}
	// The first decision is unchanged by the later ones (append-only).
	first, err := e.mem.GetModerationAction(ctx, hide.Action.ID)
	if err != nil || first != hide.Action {
		t.Fatalf("first action = %+v, %v (was %+v)", first, err, hide.Action)
	}
	for _, ev := range e.audit.of(authz.FeedPostModerate) {
		if ev.OrgID != nil || ev.Metadata["org_id"] != orgID("club_a").String() {
			t.Fatalf("moderation audit = %+v", ev)
		}
	}
	if n := len(e.audit.of(authz.FeedPostModerate)); n != 3 {
		t.Fatalf("audit records = %d", n)
	}
	manage, _ := e.svc.ListOrgPosts(ctx, persona(t, "club_admin"), orgID("club_a"), feed.ManageQuery{})
	if manage.Items[0].ReasonCode != "scam" || feed.ToManageDTO(manage.Items[0]).Body != nil {
		t.Fatalf("manage view = %+v", manage.Items[0])
	}
	// A removed post is restored only after a moderator's removal; the
	// author org's own removal stays.
	own := e.published(t, "club_admin", "club_a", textInput("Kendi")).Post.ID
	if err := e.svc.DeletePost(ctx, persona(t, "club_admin"), own); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.ModeratePost(ctx, persona(t, "moderator"), own, feed.ModerationInput{Action: feed.ModRestore, ReasonCode: "appeal"})
	wantCode(t, err, 409, "feed.invalid_state")
	// Outside the state machine: 409; outside the admin surface: 403.
	_, err = e.svc.ModeratePost(ctx, persona(t, "moderator"), post, feed.ModerationInput{Action: feed.ModApprove, ReasonCode: "ok"})
	wantCode(t, err, 409, "feed.invalid_state")
	_, err = e.svc.ModeratePost(ctx, persona(t, "club_admin"), post, feed.ModerationInput{Action: feed.ModHide, ReasonCode: "spam"})
	wantForbidden(t, err)
	_, err = e.svc.ModeratePost(ctx, persona(t, "moderator"), post, feed.ModerationInput{Action: feed.ModHide, ReasonCode: "Spam!"})
	wantField(t, err, "body.reason_code")
	_, err = e.svc.ModeratePost(ctx, persona(t, "moderator"), uuid.NewV7(), feed.ModerationInput{Action: feed.ModHide, ReasonCode: "spam"})
	wantNotFound(t, err)
}

// TestModeratePost_OwnWorkForbidden: a moderator does not decide on a post
// they wrote nor on a post of an org they write for (403).
func TestModeratePost_OwnWorkForbidden(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	post := e.published(t, "club_admin", "club_a", textInput("Duyuru")).Post.ID
	writer := staffActor(authz.PlatformModerator, authz.Membership{OrgID: orgID("club_a"), OrgKind: authz.OrgClub,
		Role: authz.MemberClubAdmin})
	_, err := e.svc.ModeratePost(ctx, writer, post, feed.ModerationInput{Action: feed.ModHide, ReasonCode: "spam"})
	wantForbidden(t, err)
	// The platform org's post, by a platform admin who wrote it.
	pa := staffActor(authz.PlatformAdmin, authz.Membership{OrgID: orgID("platform_org"), OrgKind: authz.OrgPlatform,
		Role: authz.MemberStaff})
	v, err := e.svc.CreatePost(ctx, pa, orgID("platform_org"), textInput("Koro"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.PublishPost(ctx, pa, v.Post.ID); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.ModeratePost(ctx, pa, v.Post.ID, feed.ModerationInput{Action: feed.ModRemove, ReasonCode: "spam"})
	wantForbidden(t, err)
	if _, err := e.svc.ModeratePost(ctx, persona(t, "moderator"), v.Post.ID,
		feed.ModerationInput{Action: feed.ModHide, ReasonCode: "spam"}); err != nil {
		t.Fatal(err)
	}
}

// TestModerateOrg_RestrictBlocksPosting: restrict_org sends every post of
// the org to review until restricted_until; suspend_org (a platform
// admin's, k17) stops new drafts and publication (403
// feed.posting_suspended); the org's writers are told.
func TestModerateOrg_RestrictBlocksPosting(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	e.trust("club_a")
	admin := persona(t, "club_admin")
	e.orgs.setMembers(admin.UserID, admin.Memberships...)
	until := t0.Add(7 * 24 * time.Hour)
	r, err := e.svc.ModerateOrg(ctx, persona(t, "moderator"), orgID("club_a"), feed.ModerationInput{
		Action: feed.ModRestrictOrg, ReasonCode: "repeated_spam", Note: "üç şikâyet", RestrictedUntil: &until})
	if err != nil {
		t.Fatal(err)
	}
	if r.Settings.TrustLevel != feed.TrustRestricted || r.Settings.RestrictedUntil == nil || !r.Settings.RestrictedUntil.Equal(until) ||
		r.Action.Action != feed.ModRestrictOrg || *r.Settings.Reason != "repeated_spam" {
		t.Fatalf("restrict = %+v", r)
	}
	v := e.create(t, "club_admin", "club_a", textInput("Kısıtlıyken"))
	p, err := e.svc.PublishPost(ctx, admin, v.Post.ID)
	if err != nil || p.Post.Status != feed.StatusPendingReview {
		t.Fatalf("publish while restricted = %s, %v", p.Post.Status, err)
	}
	// After the restriction the trusted org's approved posts decide.
	e.clk.Advance(8 * 24 * time.Hour)
	e.mem.PutSettings(feed.OrgSettings{OrgID: orgID("club_a"), TrustLevel: feed.TrustRestricted, ApprovedPostsCount: 5,
		RestrictedUntil: &until, Reason: r.Settings.Reason})
	v = e.create(t, "club_admin", "club_a", textInput("Sonra"))
	if p, err := e.svc.PublishPost(ctx, admin, v.Post.ID); err != nil || p.Post.Status != feed.StatusPublished {
		t.Fatalf("publish after the restriction = %s, %v", p.Post.Status, err)
	}
	// A moderator does not suspend (k17); a platform admin does.
	_, err = e.svc.ModerateOrg(ctx, persona(t, "moderator"), orgID("club_a"),
		feed.ModerationInput{Action: feed.ModSuspendOrg, ReasonCode: "abuse"})
	wantForbidden(t, err)
	draft := e.create(t, "club_admin", "club_a", textInput("Askıdan önce"))
	if _, err := e.svc.ModerateOrg(ctx, persona(t, "platform_admin"), orgID("club_a"),
		feed.ModerationInput{Action: feed.ModSuspendOrg, ReasonCode: "abuse"}); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.CreatePost(ctx, admin, orgID("club_a"), textInput("Askıda"))
	wantCode(t, err, 403, "feed.posting_suspended")
	_, err = e.svc.PublishPost(ctx, admin, draft.Post.ID)
	wantCode(t, err, 403, "feed.posting_suspended")
	// Only a platform admin changes a suspended org; a warning is anyone's.
	later := e.clk.Now().Add(24 * time.Hour)
	_, err = e.svc.ModerateOrg(ctx, persona(t, "moderator"), orgID("club_a"), feed.ModerationInput{
		Action: feed.ModRestrictOrg, ReasonCode: "lifted", RestrictedUntil: &later})
	wantForbidden(t, err)
	if _, err := e.svc.ModerateOrg(ctx, persona(t, "moderator"), orgID("club_a"),
		feed.ModerationInput{Action: feed.ModWarnOrg, ReasonCode: "reminder"}); err != nil {
		t.Fatal(err)
	}
	// The org's writers are told of each decision.
	for _, task := range e.queue.Tasks() {
		if err := feed.HandleTask(e.mod, ctx, task.Type, task.Payload); err != nil {
			t.Fatal(err)
		}
	}
	if got := e.sender.to("feed.org_moderated"); len(got) != 3 || got[0] != admin.UserID {
		t.Fatalf("org notices = %v", got)
	}
	// Input rules: restricted_until is required for restrict_org, refused
	// otherwise, in the future and at most a year ahead.
	pa := persona(t, "platform_admin")
	for _, in := range []feed.ModerationInput{
		{Action: feed.ModRestrictOrg, ReasonCode: "x_y"},
		{Action: feed.ModWarnOrg, ReasonCode: "x_y", RestrictedUntil: &later},
		{Action: feed.ModRestrictOrg, ReasonCode: "x_y", RestrictedUntil: new(t0.Add(-time.Hour))},
		{Action: feed.ModRestrictOrg, ReasonCode: "x_y", RestrictedUntil: new(e.clk.Now().Add(400 * 24 * time.Hour))},
	} {
		_, err := e.svc.ModerateOrg(ctx, pa, orgID("company_a"), in)
		wantField(t, err, "body.restricted_until")
	}
	// A platform admin who belongs to the org does not moderate it; the
	// platform org is not moderated.
	member := staffActor(authz.PlatformAdmin, authz.Membership{OrgID: orgID("company_a"), OrgKind: authz.OrgCompany,
		Role: authz.MemberCompanyAdmin})
	_, err = e.svc.ModerateOrg(ctx, member, orgID("company_a"), feed.ModerationInput{Action: feed.ModWarnOrg, ReasonCode: "x_y"})
	wantForbidden(t, err)
	_, err = e.svc.ModerateOrg(ctx, pa, orgID("platform_org"), feed.ModerationInput{Action: feed.ModWarnOrg, ReasonCode: "x_y"})
	wantCode(t, err, 409, "feed.invalid_state")
	_, err = e.svc.ModerateOrg(ctx, pa, uuid.NewV7(), feed.ModerationInput{Action: feed.ModWarnOrg, ReasonCode: "x_y"})
	wantNotFound(t, err)
}

// TestReviewQueue_ApprovePublishes: a new org's post waits in the review
// queue (moderators are told); approve publishes it (post.published), counts
// it towards the org's trust and tells the author org; remove rejects one
// in review and gives a targeted unit back.
func TestReviewQueue_ApprovePublishes(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	cm := persona(t, "company_member")
	e.orgs.setMembers(cm.UserID, cm.Memberships...)
	v := e.create(t, "company_member", "company_a", textInput("İlk ilan"))
	if p, err := e.svc.PublishPost(ctx, cm, v.Post.ID); err != nil || p.Post.Status != feed.StatusPendingReview {
		t.Fatalf("publish of a new org = %s, %v", p.Post.Status, err)
	}
	q, err := e.svc.ListReviewQueue(ctx, persona(t, "moderator"), feed.PageQuery{})
	if err != nil || len(q.Items) != 1 || q.Items[0].Post.ID != v.Post.ID || !q.Items[0].Manage {
		t.Fatalf("queue = %+v, %v", q.Items, err)
	}
	_, err = e.svc.ListReviewQueue(ctx, cm, feed.PageQuery{})
	wantForbidden(t, err)
	// Moderators are told of the submission.
	runEvents(t, e, domainevent.TopicPostSubmittedForReview)
	if got := e.sender.to("feed.review_submitted"); len(got) != 1 || got[0] != persona(t, "moderator").UserID {
		t.Fatalf("review notices = %v", got)
	}
	m := e.moderate(t, "moderator", v.Post.ID, feed.ModApprove, "approved")
	if m.Post.Status != feed.StatusPublished || m.Post.PublishedAt == nil || m.Post.ReviewedBy == nil {
		t.Fatalf("approved post = %+v", m.Post)
	}
	if _, err := e.svc.GetPost(ctx, persona(t, "student"), v.Post.ID); err != nil {
		t.Fatalf("approved post not visible: %v", err)
	}
	st, _, _ := e.mem.OrgSettings(ctx, orgID("company_a"))
	if st.ApprovedPostsCount != 1 || st.TrustLevel != feed.TrustNew {
		t.Fatalf("settings = %+v", st)
	}
	if q, _ := e.svc.ListReviewQueue(ctx, persona(t, "moderator"), feed.PageQuery{}); len(q.Items) != 0 {
		t.Fatalf("queue after approval = %d", len(q.Items))
	}
	runEvents(t, e, domainevent.TopicPostModerated)
	if got := e.sender.to("feed.post_approved"); len(got) != 1 || got[0] != cm.UserID {
		t.Fatalf("author notices = %v", got)
	}
	// A targeted post rejected in review gives its unit back.
	e.audience.add(20, uniKey)
	e.grantPosts(t, "company_a", 1)
	tp, err := e.svc.CreateTargetedPost(ctx, cm, orgID("company_a"), uniTargeted("Hedefli"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.PublishPost(ctx, cm, tp.Post.ID); err != nil {
		t.Fatal(err)
	}
	if e.remaining(t, "company_a") != 0 {
		t.Fatal("unit not consumed")
	}
	rej := e.moderate(t, "moderator", tp.Post.ID, feed.ModRemove, "misleading_job")
	if rej.Post.Status != feed.StatusRemoved || e.remaining(t, "company_a") != 1 {
		t.Fatalf("rejected = %s, remaining %d", rej.Post.Status, e.remaining(t, "company_a"))
	}
	// A rejected post is not restored (never published; a new post is a new unit).
	_, err = e.svc.ModeratePost(ctx, persona(t, "moderator"), tp.Post.ID, feed.ModerationInput{Action: feed.ModRestore, ReasonCode: "appeal"})
	wantCode(t, err, 409, "feed.invalid_state")
}

// TestAutoTrust_AfterThreshold: the third approved post of a new org makes
// it trusted (feed.org_settings.auto_trust, a system action audited as
// such); its next post is published without review.
func TestAutoTrust_AfterThreshold(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	cm := persona(t, "company_member")
	for i := range 3 {
		v := e.create(t, "company_member", "company_a", textInput("İlan "+string(rune('A'+i))))
		if p, err := e.svc.PublishPost(ctx, cm, v.Post.ID); err != nil || p.Post.Status != feed.StatusPendingReview {
			t.Fatalf("publish %d = %s, %v", i, p.Post.Status, err)
		}
		e.moderate(t, "moderator", v.Post.ID, feed.ModApprove, "approved")
		st, _, _ := e.mem.OrgSettings(ctx, orgID("company_a"))
		want := feed.TrustNew
		if i == 2 {
			want = feed.TrustTrusted
		}
		if st.ApprovedPostsCount != i+1 || st.TrustLevel != want {
			t.Fatalf("after approval %d: %+v", i+1, st)
		}
	}
	got := e.audit.of(authz.FeedOrgSettingsAutoTrust)
	if len(got) != 1 || got[0].SystemJob != "feed.auto_trust" || got[0].ActorUserID != nil {
		t.Fatalf("auto_trust audit = %+v", got)
	}
	v := e.create(t, "company_member", "company_a", textInput("Güvenilir"))
	if p, err := e.svc.PublishPost(ctx, cm, v.Post.ID); err != nil || p.Post.Status != feed.StatusPublished {
		t.Fatalf("publish of a trusted org = %s, %v", p.Post.Status, err)
	}
	// A restricted org's approvals count but do not make it trusted.
	reason := "spam"
	until := t0.Add(48 * time.Hour)
	e.mem.PutSettings(feed.OrgSettings{OrgID: orgID("club_b"), TrustLevel: feed.TrustRestricted, ApprovedPostsCount: 7,
		RestrictedUntil: &until, Reason: &reason})
	b := e.create(t, "other_club_admin", "club_b", textInput("Kısıtlı"))
	if _, err := e.svc.PublishPost(ctx, persona(t, "other_club_admin"), b.Post.ID); err != nil {
		t.Fatal(err)
	}
	e.moderate(t, "moderator", b.Post.ID, feed.ModApprove, "approved")
	if st, _, _ := e.mem.OrgSettings(ctx, orgID("club_b")); st.TrustLevel != feed.TrustRestricted || st.ApprovedPostsCount != 8 {
		t.Fatalf("restricted org = %+v", st)
	}
}

// TestModerationNotices_Recipients: the review queue and report notices go
// to the moderators in effect, or to the platform admins when there is
// none; a hidden or removed post's notice goes to the author org's
// writers only (not its plain members).
func TestModerationNotices_Recipients(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	e.staff.moderators = nil
	post := e.published(t, "club_admin", "club_a", textInput("Duyuru")).Post.ID
	admin, member := persona(t, "club_admin"), persona(t, "club_member")
	e.orgs.setMembers(admin.UserID, admin.Memberships...)
	e.orgs.setMembers(member.UserID, member.Memberships...)
	e.report(t, "student", post)
	runEvents(t, e, domainevent.TopicContentReportCreated)
	if got := e.sender.to("feed.report_created"); len(got) != 1 || got[0] != persona(t, "platform_admin").UserID {
		t.Fatalf("report notices without moderators = %v", got)
	}
	e.moderate(t, "platform_admin", post, feed.ModRemove, "scam")
	runEvents(t, e, domainevent.TopicPostModerated)
	runEvents(t, e, domainevent.TopicPostModerated) // a retried task notifies once
	if got := e.sender.to("feed.post_actioned"); len(got) != 1 || got[0] != admin.UserID {
		t.Fatalf("removal notices = %v", got)
	}
	// Without notify the task fails (retried), it never drops the notice.
	n := newEnvWith(t, func(d *feed.Deps) { d.Sender = nil })
	np := n.published(t, "club_admin", "club_a", textInput("x")).Post.ID
	n.orgs.setMembers(admin.UserID, admin.Memberships...)
	n.moderate(t, "moderator", np, feed.ModHide, "spam")
	for _, env := range n.out.Events() {
		if env.Topic == domainevent.TopicPostModerated {
			if err := feed.HandleEvent(n.mod, ctx, env); err == nil {
				t.Fatal("a notice without notify succeeded")
			}
		}
	}
}

// runEvents runs the feed consumers of every recorded event of topic.
func runEvents(t *testing.T, e *env, topic string) {
	t.Helper()
	for _, env := range e.out.Events() {
		if env.Topic != topic {
			continue
		}
		if err := feed.HandleEvent(e.mod, t.Context(), env); err != nil {
			t.Fatalf("%s: %v", topic, err)
		}
	}
}

// TestPrivacy_ReportsAndModeration: the KVKK export lists a user's reports
// and a moderator's decisions; erasure deletes the reports (the decisions
// stay, their report_id cleared) and retains the decisions.
func TestPrivacy_ReportsAndModeration(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	post := e.published(t, "club_admin", "club_a", textInput("Duyuru")).Post.ID
	r := e.report(t, "student", post)
	mod := persona(t, "moderator")
	if _, err := e.svc.ModeratePost(ctx, mod, post, feed.ModerationInput{Action: feed.ModHide, ReasonCode: "spam",
		ReportID: &r.ID}); err != nil {
		t.Fatal(err)
	}
	exp, err := e.mod.Privacy.Export(ctx, persona(t, "student").UserID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(exp.Items, func(m map[string]any) bool {
		return m["type"] == "content_report" && m["details"] == "Ücret istiyorlar"
	}) {
		t.Fatalf("export = %+v", exp.Items)
	}
	modExp, err := e.mod.Privacy.Export(ctx, mod.UserID)
	if err != nil || !slices.ContainsFunc(modExp.Items, func(m map[string]any) bool { return m["type"] == "moderation_action" }) {
		t.Fatalf("moderator export = %+v, %v", modExp.Items, err)
	}
	res, err := e.mod.Privacy.Erase(ctx, persona(t, "student").UserID)
	if err != nil || res.Deleted != 1 {
		t.Fatalf("erase = %+v, %v", res, err)
	}
	if _, err := e.mem.GetReport(ctx, r.ID); err == nil {
		t.Fatal("the report survived the erasure")
	}
	actions, _ := e.mem.ListModerationActions(ctx, feed.ActionFilter{}, nil, 10)
	if len(actions) != 1 || actions[0].ReportID != nil {
		t.Fatalf("actions after erasure = %+v", actions)
	}
	again, err := e.mod.Privacy.Erase(ctx, persona(t, "student").UserID)
	if err != nil || again.Deleted != 0 {
		t.Fatalf("second erase = %+v, %v", again, err)
	}
	modRes, err := e.mod.Privacy.Erase(ctx, mod.UserID)
	if err != nil || modRes.Retained != 1 || modRes.RetainedReason == "" {
		t.Fatalf("moderator erase = %+v, %v", modRes, err)
	}
}

// TestNoticeTemplates_Exist: every moderation notice has its notify
// template (internal/notify/templates/feed): in-app, transactional,
// preference moderation.
func TestNoticeTemplates_Exist(t *testing.T) {
	t.Parallel()
	tpl, err := notify.EmbeddedTemplates()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range feed.NoticeTemplates {
		m, ok := tpl.Meta(id)
		if !ok {
			t.Fatalf("template %s is missing", id)
		}
		if m.Category != notify.CategoryTransactional || m.Preference != notify.PrefModeration ||
			!slices.Contains(m.Channels, notify.ChannelInApp) {
			t.Fatalf("template %s = %+v", id, m)
		}
	}
}
