//go:build integration

package migrations_test

import (
	"slices"
	"sync"
	"testing"
	"uuid"

	"koro/internal/platform/db"
	"koro/internal/platform/testutil"
)

// TestSchema_PostsCompanySegmentRequiresEntitlement (02 §12.2): a company
// post to a segment needs an audience estimate of at least 10 and the
// consumed entitlement (posts_company_segment_chk). The estimate must be
// present: in the 02 DDL a NULL estimate made the CHECK NULL, i.e. passed
// (fixed in 000066). Other authors and other audiences need neither.
func TestSchema_PostsCompanySegmentRequiresEntitlement(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	company, club, author := w.org("company"), w.org("club"), w.user()
	use := w.usage(company, "post")
	// $1 = org, $2 = kind, $3 = author, $4 = id.
	p := func(name string, org uuid.UUID, kind string, over map[string]string, state, constraint string) p1aCase {
		return p1aCase{name, insertSQL("posts", postDefaults, cols(companySegmentPost(use), over)), []any{org, kind, author, uuid.NewV7()}, state, constraint}
	}
	chk := func(name string, over map[string]string) p1aCase {
		return p(name, company, "company", over, db.CodeCheckViolation, "posts_company_segment_chk")
	}
	expectFail(t, w.d.Pool, []p1aCase{
		chk("no entitlement", map[string]string{"entitlement_usage_id": "-"}),
		chk("an estimate of 9", map[string]string{"audience_size_estimate": "9"}),
		chk("an estimate of 0", map[string]string{"audience_size_estimate": "0"}),
		chk("no estimate (NULL)", map[string]string{"audience_size_estimate": "-"}),
		chk("no estimate and no entitlement", map[string]string{"audience_size_estimate": "-", "entitlement_usage_id": "-"}),
		chk("a draft without an entitlement", map[string]string{"entitlement_usage_id": "-", "status": "'draft'", "published_at": "-"}),
		p("a company segment post without a snapshot", company, "company", map[string]string{"audience_snapshot": "-"}, db.CodeCheckViolation, "posts_audience_snapshot_chk"),
		p("a non-commercial company post", company, "company", map[string]string{"commercial_flag": "false"}, db.CodeCheckViolation, "posts_company_commercial_chk"),
		p("an unknown entitlement usage", company, "company", map[string]string{"entitlement_usage_id": lit(uuid.NewV7().String())}, db.CodeForeignKeyViolation, "posts_entitlement_usage_id_fkey"),
	})
	expectOK(t, w.d.Pool, []p1aCase{
		p("a company segment post with an estimate of 10 and an entitlement", company, "company", map[string]string{"audience_size_estimate": "10"}, "", ""),
		p("a club segment post needs neither (k >= 10 does not apply to its own members)", club, "club",
			map[string]string{"commercial_flag": "false", "audience_size_estimate": "-", "entitlement_usage_id": "-"}, "", ""),
		p("a public company post needs neither", company, "company",
			map[string]string{"audience_kind": "'public'", "audience_keys": "ARRAY['all']", "audience_dim_count": "1", "audience_snapshot": "-",
				"audience_size_estimate": "-", "entitlement_usage_id": "-"}, "", ""),
		p("a followers-only company post needs neither", company, "company",
			map[string]string{"audience_kind": "'followers'", "audience_keys": "ARRAY['all']", "audience_dim_count": "1", "audience_snapshot": "-",
				"audience_size_estimate": "-", "entitlement_usage_id": "-"}, "", ""),
	})
	// The rule holds on UPDATE too: a published company segment post cannot
	// drop its entitlement or its estimate afterwards.
	id := w.post(company, "company", author, companySegmentPost(use))
	expectFail(t, w.d.Pool, []p1aCase{
		{"dropping the entitlement", `UPDATE posts SET entitlement_usage_id = NULL WHERE id = $1`, []any{id}, db.CodeCheckViolation, "posts_company_segment_chk"},
		{"dropping the estimate", `UPDATE posts SET audience_size_estimate = NULL WHERE id = $1`, []any{id}, db.CodeCheckViolation, "posts_company_segment_chk"},
		{"switching a public company post to a segment", `UPDATE posts SET audience_kind = 'segment', audience_snapshot = '{}'
			WHERE id = $1`, []any{w.post(company, "company", author, map[string]string{"commercial_flag": "true"})}, db.CodeCheckViolation, "posts_company_segment_chk"},
	})
}

// TestSchema_PostConstraints (02 §12.2): kind ↔ payload, audience shape,
// status ↔ timestamps, author org kind (composite FK), counters, H-38 key.
func TestSchema_PostConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	club, author := w.org("club"), w.user()
	ev := w.event(club, author, nil)
	// $1 = org, $2 = kind, $3 = author, $4 = id.
	p := func(name string, over map[string]string, state, constraint string) p1aCase {
		return p1aCase{name, insertSQL("posts", postDefaults, over), []any{club, "club", author, uuid.NewV7()}, state, constraint}
	}
	chk := func(name string, over map[string]string, constraint string) p1aCase {
		return p(name, over, db.CodeCheckViolation, constraint)
	}
	event := lit(ev.String())
	expectFail(t, w.d.Pool, []p1aCase{
		chk("unknown kind", map[string]string{"kind": "'video'"}, "posts_kind_chk"),
		chk("unknown status", map[string]string{"status": "'archived'"}, "posts_status_chk"),
		chk("unknown audience", map[string]string{"audience_kind": "'friends'"}, "posts_audience_kind_chk"),
		chk("title of 141 characters", map[string]string{"title": lit(repeat("a", 141))}, "posts_title_chk"),
		chk("body of 5001 characters", map[string]string{"body_text": lit(repeat("a", 5001))}, "posts_body_text_chk"),
		chk("five audience dimensions", map[string]string{"audience_kind": "'segment'", "audience_snapshot": "'{}'", "audience_dim_count": "5"}, "posts_audience_dim_count_chk"),
		chk("an event share without the event", map[string]string{"kind": "'event_share'"}, "posts_event_share_chk"),
		chk("a text post pointing at an event", map[string]string{"event_id": event}, "posts_event_share_chk"),
		chk("an opportunity call without the opportunity", map[string]string{"kind": "'opportunity_call'"}, "posts_opportunity_chk"),
		chk("a text post with an opportunity", map[string]string{"opportunity": `'{"role":"intern"}'::jsonb`}, "posts_opportunity_chk"),
		chk("a text post without a body", map[string]string{"body_json": "-"}, "posts_body_chk"),
		chk("a public post with a targeting key", map[string]string{"audience_keys": "ARRAY['uni:x']"}, "posts_audience_all_chk"),
		chk("a public post with two dimensions", map[string]string{"audience_dim_count": "2"}, "posts_audience_all_chk"),
		chk("a segment post without a snapshot", map[string]string{"audience_kind": "'segment'", "audience_keys": "ARRAY['uni:x']"}, "posts_audience_snapshot_chk"),
		chk("published without a publication time", map[string]string{"published_at": "-"}, "posts_published_chk"),
		chk("hidden without a publication time", map[string]string{"status": "'hidden'", "published_at": "-"}, "posts_published_chk"),
		chk("removed without a removal time", map[string]string{"status": "'removed'"}, "posts_removed_chk"),
		chk("a removal time on a published post", map[string]string{"removed_at": "now()"}, "posts_removed_chk"),
		chk("negative likes", map[string]string{"like_count": "-1"}, "posts_like_count_chk"),
		chk("negative saves", map[string]string{"save_count": "-1"}, "posts_save_count_chk"),
		{"an author kind that is not the org's", insertSQL("posts", postDefaults, nil), []any{club, "university", author, uuid.NewV7()}, db.CodeForeignKeyViolation, "posts_org_id_author_org_kind_fkey"},
		{"unknown author", insertSQL("posts", postDefaults, nil), []any{club, "club", uuid.NewV7(), uuid.NewV7()}, db.CodeForeignKeyViolation, "posts_author_user_id_fkey"},
		p("unknown event", map[string]string{"kind": "'event_share'", "event_id": lit(uuid.NewV7().String())}, db.CodeForeignKeyViolation, "posts_event_id_fkey"),
		p("unknown reviewer", map[string]string{"reviewed_by": lit(uuid.NewV7().String())}, db.CodeForeignKeyViolation, "posts_reviewed_by_fkey"),
	})
	expectOK(t, w.d.Pool, []p1aCase{
		p("a draft", map[string]string{"status": "'draft'", "published_at": "-"}, "", ""),
		p("pending review", map[string]string{"status": "'pending_review'", "published_at": "-"}, "", ""),
		p("an image post without a body", map[string]string{"kind": "'image'", "body_json": "-"}, "", ""),
		p("an event share", map[string]string{"kind": "'event_share'", "body_json": "-", "event_id": event}, "", ""),
		p("an opportunity call", map[string]string{"kind": "'opportunity_call'", "opportunity": `'{"role":"intern"}'::jsonb`}, "", ""),
		p("hidden", map[string]string{"status": "'hidden'"}, "", ""),
		p("removed", map[string]string{"status": "'removed'", "removed_at": "now()"}, "", ""),
		p("followers only", map[string]string{"audience_kind": "'followers'"}, "", ""),
	})
	// Full-text search over the title and the body.
	w.post(club, "club", author, map[string]string{"title": "'Bahar Şenliği'", "body_text": "'Kampüste konserler'"})
	if n := w.count(`SELECT count(*) FROM posts WHERE search_vector @@ websearch_to_tsquery('koro_tr', 'konser') AND status = 'published'`); n != 1 {
		t.Errorf("full-text matches = %d, want 1", n)
	}
}

// TestSchema_PostChildrenGoWithThePost (02 §12.3–§12.6): media, likes, saves
// and hides are deleted with the post (the purge of removed posts); a file
// belongs to one post; at most 10 media positions; one like per user.
func TestSchema_PostChildrenGoWithThePost(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	club, author, reader := w.org("club"), w.user(), w.user()
	post := w.post(club, "club", author, nil)
	file := w.file("post_media")
	w.exec(`INSERT INTO post_media (post_id, file_id, position, alt_text, width, height) VALUES ($1, $2, 0, 'Afiş', 1080, 1350)`, post, file)
	w.exec(`INSERT INTO post_reactions (post_id, user_id) VALUES ($1, $2)`, post, reader)
	w.exec(`INSERT INTO post_saves (user_id, post_id) VALUES ($1, $2)`, reader, post)
	w.exec(`INSERT INTO post_hides (user_id, post_id) VALUES ($1, $2)`, author, post)
	media := `INSERT INTO post_media (post_id, file_id, position) VALUES ($1, $2, $3)`
	expectFail(t, w.d.Pool, []p1aCase{
		{"the same file on another post", media, []any{w.post(club, "club", author, nil), file, 0}, db.CodeUniqueViolation, "post_media_file_id_uq"},
		{"the same position twice", media, []any{post, w.file("post_media"), 0}, db.CodeUniqueViolation, "post_media_post_position_uq"},
		{"position 10", media, []any{post, w.file("post_media"), 10}, db.CodeCheckViolation, "post_media_position_chk"},
		{"alt text of 301 characters", `INSERT INTO post_media (post_id, file_id, position, alt_text) VALUES ($1, $2, 1, $3)`, []any{post, w.file("post_media"), repeat("a", 301)}, db.CodeCheckViolation, "post_media_alt_text_chk"},
		{"zero width", `INSERT INTO post_media (post_id, file_id, position, width) VALUES ($1, $2, 1, 0)`, []any{post, w.file("post_media")}, db.CodeCheckViolation, "post_media_width_chk"},
		{"zero height", `INSERT INTO post_media (post_id, file_id, position, height) VALUES ($1, $2, 1, 0)`, []any{post, w.file("post_media")}, db.CodeCheckViolation, "post_media_height_chk"},
		{"unknown file", media, []any{post, uuid.NewV7(), 1}, db.CodeForeignKeyViolation, "post_media_file_id_fkey"},
		{"a second like", `INSERT INTO post_reactions (post_id, user_id) VALUES ($1, $2)`, []any{post, reader}, db.CodeUniqueViolation, "post_reactions_pkey"},
		{"another reaction kind", `INSERT INTO post_reactions (post_id, user_id, kind) VALUES ($1, $2, 'love')`, []any{post, author}, db.CodeCheckViolation, "post_reactions_kind_chk"},
		{"a second save", `INSERT INTO post_saves (user_id, post_id) VALUES ($1, $2)`, []any{reader, post}, db.CodeUniqueViolation, "post_saves_pkey"},
		{"a like of an unknown post", `INSERT INTO post_reactions (post_id, user_id) VALUES ($1, $2)`, []any{uuid.NewV7(), reader}, db.CodeForeignKeyViolation, "post_reactions_post_id_fkey"},
		{"a save by an unknown user", `INSERT INTO post_saves (user_id, post_id) VALUES ($1, $2)`, []any{uuid.NewV7(), post}, db.CodeForeignKeyViolation, "post_saves_user_id_fkey"},
		{"a hide of an unknown post", `INSERT INTO post_hides (user_id, post_id) VALUES ($1, $2)`, []any{reader, uuid.NewV7()}, db.CodeForeignKeyViolation, "post_hides_post_id_fkey"},
	})
	w.exec(`DELETE FROM posts WHERE id = $1`, post)
	if n := w.count(`SELECT (SELECT count(*) FROM post_media WHERE post_id = $1) + (SELECT count(*) FROM post_reactions WHERE post_id = $1)
		+ (SELECT count(*) FROM post_saves WHERE post_id = $1) + (SELECT count(*) FROM post_hides WHERE post_id = $1)`, post); n != 0 {
		t.Errorf("children left after deleting the post = %d", n)
	}
	if n := w.count(`SELECT count(*) FROM files WHERE id = $1`, file); n != 1 {
		t.Errorf("the file row must stay for files.Delete: %d", n)
	}
}

// TestSchema_FeedSettingsFollowsMutes (02 §12.1, §12.7, §12.8).
func TestSchema_FeedSettingsFollowsMutes(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	club, user := w.org("club"), w.user()
	settings := `INSERT INTO feed_org_settings (org_id, trust_level, approved_posts_count, reason) VALUES ($1, $2, $3, $4)`
	expectFail(t, w.d.Pool, []p1aCase{
		{"unknown trust level", settings, []any{club, "vip", 0, nil}, db.CodeCheckViolation, "feed_org_settings_trust_level_chk"},
		{"restricted without a reason", settings, []any{club, "restricted", 0, nil}, db.CodeCheckViolation, "feed_org_settings_restriction_reason_chk"},
		{"suspended without a reason", settings, []any{club, "suspended", 0, nil}, db.CodeCheckViolation, "feed_org_settings_restriction_reason_chk"},
		{"negative approved count", settings, []any{club, "new", -1, nil}, db.CodeCheckViolation, "feed_org_settings_approved_posts_count_chk"},
		{"reason of 501 characters", settings, []any{club, "restricted", 0, repeat("a", 501)}, db.CodeCheckViolation, "feed_org_settings_reason_chk"},
		{"unknown org", settings, []any{uuid.NewV7(), "new", 0, nil}, db.CodeForeignKeyViolation, "feed_org_settings_org_id_fkey"},
	})
	w.exec(settings, club, "restricted", 2, "Spam şikâyetleri")
	expectFail(t, w.d.Pool, []p1aCase{
		{"a second settings row", settings, []any{club, "new", 0, nil}, db.CodeUniqueViolation, "feed_org_settings_pkey"},
		{"lifting the reason while restricted", `UPDATE feed_org_settings SET reason = NULL WHERE org_id = $1`, []any{club}, db.CodeCheckViolation, "feed_org_settings_restriction_reason_chk"},
		{"app_rw cannot delete the settings", `DELETE FROM feed_org_settings WHERE org_id = $1`, []any{club}, db.CodeInsufficientPriv, ""},
	})
	w.exec(`UPDATE feed_org_settings SET trust_level = 'trusted', reason = NULL WHERE org_id = $1`, club)

	follow := `INSERT INTO org_follows (user_id, org_id, source) VALUES ($1, $2, $3)`
	w.exec(follow, user, club, "manual")
	w.exec(`INSERT INTO org_mutes (user_id, org_id) VALUES ($1, $2)`, user, club)
	expectFail(t, w.d.Pool, []p1aCase{
		{"following twice", follow, []any{user, club, "auto_membership"}, db.CodeUniqueViolation, "org_follows_pkey"},
		{"unknown follow source", follow, []any{w.user(), club, "import"}, db.CodeCheckViolation, "org_follows_source_chk"},
		{"following an unknown org", follow, []any{user, uuid.NewV7(), "manual"}, db.CodeForeignKeyViolation, "org_follows_org_id_fkey"},
		{"muting twice", `INSERT INTO org_mutes (user_id, org_id) VALUES ($1, $2)`, []any{user, club}, db.CodeUniqueViolation, "org_mutes_pkey"},
		{"muting an unknown org", `INSERT INTO org_mutes (user_id, org_id) VALUES ($1, $2)`, []any{user, uuid.NewV7()}, db.CodeForeignKeyViolation, "org_mutes_org_id_fkey"},
	})
	if got := queryString(t, w.d, `SELECT notify::text FROM org_follows WHERE user_id = $1`, user); got != "false" {
		t.Errorf("org_follows.notify default = %s, want false", got)
	}
}

// TestSchema_ContentReportConstraints (02 §12.9): one open report per
// reporter and target, even under concurrent reports; resolution ↔ time.
func TestSchema_ContentReportConstraints(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	reporter, club := w.user(), w.org("club")
	post := w.post(club, "club", w.user(), nil)
	report := `INSERT INTO content_reports (reporter_user_id, target_type, target_id, reason, status, resolved_at) VALUES ($1, $2, $3, $4, $5, $6)`
	expectFail(t, w.d.Pool, []p1aCase{
		{"unknown target type", report, []any{reporter, "user", post, "spam", "open", nil}, db.CodeCheckViolation, "content_reports_target_type_chk"},
		{"unknown reason", report, []any{reporter, "post", post, "boring", "open", nil}, db.CodeCheckViolation, "content_reports_reason_chk"},
		{"unknown status", report, []any{reporter, "post", post, "spam", "closed", nil}, db.CodeCheckViolation, "content_reports_status_chk"},
		{"actioned without a resolution time", report, []any{reporter, "post", post, "spam", "actioned", nil}, db.CodeCheckViolation, "content_reports_resolved_chk"},
		{"dismissed without a resolution time", report, []any{reporter, "post", post, "spam", "dismissed", nil}, db.CodeCheckViolation, "content_reports_resolved_chk"},
		{"open with a resolution time", report, []any{reporter, "post", post, "spam", "open", "2026-10-01T00:00:00Z"}, db.CodeCheckViolation, "content_reports_resolved_chk"},
		{"details of 1001 characters", `INSERT INTO content_reports (reporter_user_id, target_type, target_id, reason, details) VALUES ($1, 'post', $2, 'other', $3)`, []any{reporter, post, repeat("a", 1001)}, db.CodeCheckViolation, "content_reports_details_chk"},
		{"unknown reporter", report, []any{uuid.NewV7(), "post", post, "spam", "open", nil}, db.CodeForeignKeyViolation, "content_reports_reporter_user_id_fkey"},
	})
	// Concurrent reports of one target by one reporter: exactly one is kept.
	var wg sync.WaitGroup
	won := make([]bool, 10)
	for i := range won {
		wg.Go(func() {
			_, err := w.d.Pool.Exec(t.Context(), report, reporter, "post", post, "spam", "open", nil)
			switch {
			case err == nil:
				won[i] = true
			case db.SQLState(err) != db.CodeUniqueViolation || db.ConstraintName(err) != "content_reports_open_uq":
				t.Errorf("concurrent report: %v", err)
			}
		})
	}
	wg.Wait()
	if n := len(slices.DeleteFunc(won, func(b bool) bool { return !b })); n != 1 {
		t.Errorf("concurrent reports accepted = %d, want 1", n)
	}
	expectFail(t, w.d.Pool, []p1aCase{
		{"a second open report while in review", report, []any{reporter, "post", post, "hate", "in_review", nil}, db.CodeUniqueViolation, "content_reports_open_uq"},
	})
	expectOK(t, w.d.Pool, []p1aCase{
		{"the same reporter on the org", report, []any{reporter, "org", club, "spam", "open", nil}, "", ""},
		{"another reporter on the post", report, []any{w.user(), "post", post, "spam", "open", nil}, "", ""},
	})
	w.exec(`UPDATE content_reports SET status = 'dismissed', resolved_at = now() WHERE reporter_user_id = $1 AND target_id = $2`, reporter, post)
	expectOK(t, w.d.Pool, []p1aCase{
		{"a new report after the old one was resolved", report, []any{reporter, "post", post, "spam", "open", nil}, "", ""},
	})
}

// TestSchema_ModerationActionsAppendOnly (02 §12.10, §0.5): the moderation
// log takes inserts only. app_rw has no UPDATE/DELETE/TRUNCATE grant and the
// trigger stops even the owner; the one allowed change is the
// ON DELETE SET NULL of report_id when the report is purged, and only that
// column may change in it.
func TestSchema_ModerationActionsAppendOnly(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	moderator, reporter, club := w.user(), w.user(), w.org("club")
	post := w.post(club, "club", w.user(), nil)
	reportID, actionID, other := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	w.exec(`INSERT INTO content_reports (id, reporter_user_id, target_type, target_id, reason) VALUES ($1, $2, 'post', $3, 'spam')`, reportID, reporter, post)
	ins := `INSERT INTO moderation_actions (id, report_id, target_type, target_id, action, reason_code, actor_user_id, note) VALUES ($1, $2, 'post', $3, $4, $5, $6, $7)`
	w.exec(ins, actionID, reportID, post, "hide", "spam_content", moderator, "Tekrarlayan reklam")
	w.exec(ins, other, nil, post, "restore", "appeal_accepted", moderator, nil)

	// Layer 1: no grant for app_rw.
	expectFail(t, w.d.Pool, []p1aCase{
		{"app_rw update", `UPDATE moderation_actions SET note = 'x' WHERE id = $1`, []any{actionID}, db.CodeInsufficientPriv, ""},
		{"app_rw clears the report itself", `UPDATE moderation_actions SET report_id = NULL WHERE id = $1`, []any{actionID}, db.CodeInsufficientPriv, ""},
		{"app_rw delete", `DELETE FROM moderation_actions WHERE id = $1`, []any{actionID}, db.CodeInsufficientPriv, ""},
		{"app_rw truncate", `TRUNCATE moderation_actions`, nil, db.CodeInsufficientPriv, ""},
	})
	// Layer 2: the trigger holds against the owner.
	owner := testutil.OwnerConn(t, w.d)
	expectFail(t, owner, []p1aCase{
		{"owner update", `UPDATE moderation_actions SET action = 'remove' WHERE id = $1`, []any{actionID}, db.CodeRestrictViolation, ""},
		{"owner update to the same value", `UPDATE moderation_actions SET note = note WHERE id = $1`, []any{actionID}, db.CodeRestrictViolation, ""},
		{"owner clears the report and edits the note", `UPDATE moderation_actions SET report_id = NULL, note = 'x' WHERE id = $1`, []any{actionID}, db.CodeRestrictViolation, ""},
		{"owner points the action at another report", `UPDATE moderation_actions SET report_id = $2 WHERE id = $1`, []any{other, reportID}, db.CodeRestrictViolation, ""},
		{"owner delete", `DELETE FROM moderation_actions WHERE id = $1`, []any{other}, db.CodeRestrictViolation, ""},
		{"owner truncate", `TRUNCATE moderation_actions`, nil, db.CodeRestrictViolation, ""},
	})
	if got := queryStrings(t, w, `SELECT tgname FROM pg_trigger WHERE tgrelid = 'moderation_actions'::regclass AND NOT tgisinternal ORDER BY tgname`); !slices.Equal(got, []string{"moderation_actions_immutable", "moderation_actions_no_truncate"}) {
		t.Errorf("triggers on moderation_actions = %v", got)
	}

	// The retention purge of the report (app_rw, hard delete) keeps the
	// action and only clears its report_id.
	w.exec(`DELETE FROM content_reports WHERE id = $1`, reportID)
	if got := queryString(t, w.d, `SELECT coalesce(report_id::text, 'null') || ' ' || action || ' ' || note FROM moderation_actions WHERE id = $1`, actionID); got != "null hide Tekrarlayan reklam" {
		t.Errorf("action after its report was purged = %q", got)
	}
	if n := w.count(`SELECT count(*) FROM moderation_actions`); n != 2 {
		t.Errorf("actions = %d, want 2: the log changed", n)
	}

	// The shape rules.
	expectFail(t, w.d.Pool, []p1aCase{
		{"unknown action", ins, []any{uuid.NewV7(), nil, post, "ban", "spam_content", moderator, nil}, db.CodeCheckViolation, "moderation_actions_action_chk"},
		{"reason code with a capital", ins, []any{uuid.NewV7(), nil, post, "hide", "Spam", moderator, nil}, db.CodeCheckViolation, "moderation_actions_reason_code_chk"},
		{"reason code of 1 character", ins, []any{uuid.NewV7(), nil, post, "hide", "s", moderator, nil}, db.CodeCheckViolation, "moderation_actions_reason_code_chk"},
		{"note of 1001 characters", ins, []any{uuid.NewV7(), nil, post, "hide", "spam", moderator, repeat("a", 1001)}, db.CodeCheckViolation, "moderation_actions_note_chk"},
		{"unknown moderator", ins, []any{uuid.NewV7(), nil, post, "hide", "spam", uuid.NewV7(), nil}, db.CodeForeignKeyViolation, "moderation_actions_actor_user_id_fkey"},
		{"unknown report", ins, []any{uuid.NewV7(), uuid.NewV7(), post, "hide", "spam", moderator, nil}, db.CodeForeignKeyViolation, "moderation_actions_report_id_fkey"},
		{"unknown target type", `INSERT INTO moderation_actions (target_type, target_id, action, reason_code, actor_user_id) VALUES ('user', $1, 'hide', 'spam', $2)`, []any{post, moderator}, db.CodeCheckViolation, "moderation_actions_target_type_chk"},
	})
}
