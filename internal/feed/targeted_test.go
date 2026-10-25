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
	"koro/internal/entitlements"
	"koro/internal/feed"
	"koro/internal/outreach"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/problem"
	"koro/internal/platform/problem/problemtest"
)

// uniKey is students' audience key of uni_a's university.
var uniKey = "uni:" + uniA.String()

// grantPosts gives org n targeted_post units (an admin grant valid for a
// month).
func (e *env) grantPosts(t *testing.T, org string, n int) {
	t.Helper()
	by := orgID("platform_admin")
	if _, err := e.ents.Grant(t.Context(), entitlements.GrantInput{
		OrgID: orgID(org), Feature: entitlements.FeatureTargetedPost, Quantity: &n, ValidFrom: t0.Add(-time.Hour),
		ValidTo: t0.Add(30 * 24 * time.Hour), Status: entitlements.StatusActive, SourceType: entitlements.SourceAdminGrant,
		GrantedBy: &by,
	}); err != nil {
		t.Fatal(err)
	}
}

// remaining is what org has left of targeted_post.
func (e *env) remaining(t *testing.T, org string) int {
	t.Helper()
	av, err := e.ents.Available(t.Context(), orgID(org), entitlements.FeatureTargetedPost)
	if err != nil {
		t.Fatal(err)
	}
	return av.Remaining
}

// uniTargeted is a text post for the students of uni_a.
func uniTargeted(text string) feed.TargetedInput {
	return feed.TargetedInput{Post: textInput(text), Definition: &outreach.Definition{Uni: []uuid.UUID{uniA}}}
}

// clubTargeted is a text post for the members of club.
func clubTargeted(club, text string) feed.TargetedInput {
	return feed.TargetedInput{Post: textInput(text), Definition: &outreach.Definition{Club: []uuid.UUID{orgID(club)}}}
}

// TestTargetedPost_CompanyWithoutEntitlement_403NotEntitled: a company
// without a targeted_post unit gets 403 entitlements.not_entitled (T-58);
// nothing is written. With a unit the draft is created, consumes it and
// carries the usage, the compiled keys, the snapshot and the estimate.
func TestTargetedPost_CompanyWithoutEntitlement_403NotEntitled(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	e.audience.add(42, uniKey, "year:3")
	cm := persona(t, "company_member")
	_, err := e.svc.CreateTargetedPost(ctx, cm, orgID("company_a"), uniTargeted("Staj"))
	problemtest.WantProblem(t, err, 403, "entitlements.not_entitled")
	if rows, _ := e.mem.PostsOfAuthor(ctx, cm.UserID); len(rows) != 0 {
		t.Fatalf("posts = %d after a refused targeted post", len(rows))
	}
	e.grantPosts(t, "company_a", 2)
	v, err := e.svc.CreateTargetedPost(ctx, cm, orgID("company_a"), uniTargeted("Staj"))
	if err != nil {
		t.Fatal(err)
	}
	p := v.Post
	switch {
	case p.AudienceKind != feed.AudienceSegment || !slices.Equal(p.AudienceKeys, []string{uniKey}) || p.AudienceDimCount != 1:
		t.Fatalf("audience = %s %v %d", p.AudienceKind, p.AudienceKeys, p.AudienceDimCount)
	case p.EntitlementUsageID == nil || p.AudienceSizeEstimate == nil || *p.AudienceSizeEstimate != 40:
		t.Fatalf("usage %v, estimate %v (42 students → bucket 40)", p.EntitlementUsageID, p.AudienceSizeEstimate)
	case !p.CommercialFlag || p.Status != feed.StatusDraft:
		t.Fatalf("commercial %v, status %s", p.CommercialFlag, p.Status)
	case v.Remaining == nil || *v.Remaining != 1 || v.Estimate.Count == nil || *v.Estimate.Count != 40:
		t.Fatalf("remaining %v, estimate %+v", v.Remaining, v.Estimate)
	}
	var snap map[string]any
	if err := json.Unmarshal(p.AudienceSnapshot, &snap); err != nil || snap["uni"] == nil {
		t.Fatalf("snapshot = %s", p.AudienceSnapshot)
	}
	if got := e.audit.Of(authz.FeedPostCreateTargeted); len(got) != 1 || got[0].Metadata["quota_consumed"] != true {
		t.Fatalf("audit = %+v", got)
	}
	// The DTO of the author org shows the estimate and the units left.
	d := feed.ToTargetedDTO(v)
	if d.Entitlement == nil || d.Entitlement.Remaining != 1 || d.AudienceEstimate.Count == nil || d.AudienceKind != "segment" {
		t.Fatalf("dto = %+v", d)
	}
}

// TestTargetedPost_IdempotencyReplay_NoDoubleConsume: the same
// Idempotency-Key returns the same draft without compiling or consuming
// again; another key is another post and another unit.
func TestTargetedPost_IdempotencyReplay_NoDoubleConsume(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.audience.add(30, uniKey)
	e.grantPosts(t, "company_a", 5)
	cm := persona(t, "company_member")
	key := func(v string) idempotency.Key {
		return idempotency.Key{Scope: cm.UserID.String() + ":POST /api/v1/orgs/{org_id}/targeted-posts", Value: v, Status: 201}
	}
	ctx := idempotency.WithKey(t.Context(), key("targeted-1"))
	first, err := e.svc.CreateTargetedPost(ctx, cm, orgID("company_a"), uniTargeted("Staj"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := e.svc.CreateTargetedPost(ctx, cm, orgID("company_a"), uniTargeted("Staj"))
	if err != nil {
		t.Fatal(err)
	}
	if again.Post.ID != first.Post.ID || e.remaining(t, "company_a") != 4 {
		t.Fatalf("replay = %s (first %s), remaining %d", again.Post.ID, first.Post.ID, e.remaining(t, "company_a"))
	}
	if n := len(e.audit.Of(authz.FeedPostCreateTargeted)); n != 1 {
		t.Fatalf("audit records = %d", n)
	}
	// The same key with another body is 422 idempotency.key_reused.
	_, err = e.svc.CreateTargetedPost(ctx, cm, orgID("company_a"), uniTargeted("Başka"))
	problemtest.WantProblem(t, err, 422, "idempotency.key_reused")
	// A new key is a new post and spends a new unit.
	other, err := e.svc.CreateTargetedPost(idempotency.WithKey(t.Context(), key("targeted-2")), cm, orgID("company_a"),
		uniTargeted("Staj"))
	if err != nil || other.Post.ID == first.Post.ID || e.remaining(t, "company_a") != 3 {
		t.Fatalf("second key = %s, %v, remaining %d", other.Post.ID, err, e.remaining(t, "company_a"))
	}
}

// TestTargetedPost_Below10_422NoCount: an audience of fewer than 10
// students is 422 outreach.audience_too_small, whose body carries no number;
// no post is written and no unit spent. A sensitive club and a scope
// outside the org's are refused the same way (03 k16, Y2).
func TestTargetedPost_Below10_422NoCount(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	e.audience.add(9, uniKey)
	e.grantPosts(t, "company_a", 3)
	cm := persona(t, "company_member")
	_, err := e.svc.CreateTargetedPost(ctx, cm, orgID("company_a"), uniTargeted("Az"))
	problemtest.WantProblem(t, err, 422, "outreach.audience_too_small")
	p, _ := errors.AsType[*problem.Error](err)
	for _, f := range p.Fields() {
		if strings.ContainsAny(f.Message, "0123456789") {
			t.Fatalf("a field error carries a number: %+v", f)
		}
	}
	if strings.ContainsAny(p.Title()+p.Error(), "0123456789") {
		t.Fatalf("the error carries a number: %q", p.Error())
	}
	if e.remaining(t, "company_a") != 3 {
		t.Fatal("a refused post spent a unit")
	}
	if rows, _ := e.mem.PostsOfAuthor(ctx, cm.UserID); len(rows) != 0 {
		t.Fatal("a refused post was written")
	}
	// A company never targets a sensitive club.
	e.clubs.set(orgID("club_sensitive"), true)
	_, err = e.svc.CreateTargetedPost(ctx, cm, orgID("company_a"), clubTargeted("club_sensitive", "Hassas"))
	problemtest.WantProblem(t, err, 422, "outreach.sensitive_target")
	// A club targets its own members only.
	_, err = e.svc.CreateTargetedPost(ctx, persona(t, "club_admin"), orgID("club_a"), clubTargeted("club_b", "Başkası"))
	problemtest.WantProblem(t, err, 422, "outreach.target_out_of_scope")
	// Neither a segment nor a definition, or both: 422.
	in := uniTargeted("İkisi")
	seg := uuid.NewV7()
	in.SegmentID = &seg
	_, err = e.svc.CreateTargetedPost(ctx, cm, orgID("company_a"), in)
	wantField(t, err, "body.audience")
	if e.remaining(t, "company_a") != 3 {
		t.Fatal("a refused post spent a unit")
	}
}

// TestTargetedPost_ClubOwnMembersSensitive_Allowed: a sensitive club's
// admin addresses its members: no k ≥ 10, no unit, no number (a sensitive
// membership is never counted: audience_size_estimate NULL).
func TestTargetedPost_ClubOwnMembersSensitive_Allowed(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	e.clubs.set(orgID("club_sensitive"), true)
	e.audience.sensitive["club:"+orgID("club_sensitive").String()] = true
	e.audience.add(3, "club:"+orgID("club_sensitive").String())
	admin := persona(t, "club_admin_sensitive")
	v, err := e.svc.CreateTargetedPost(ctx, admin, orgID("club_sensitive"), clubTargeted("club_sensitive", "Genel kurul"))
	if err != nil {
		t.Fatal(err)
	}
	p := v.Post
	switch {
	case !slices.Equal(p.AudienceKeys, []string{"club:" + orgID("club_sensitive").String()}),
		p.AudienceSizeEstimate != nil, p.EntitlementUsageID != nil, p.CommercialFlag:
		t.Fatalf("post = %+v", p)
	case v.Remaining != nil || v.Estimate.Count != nil || !v.Estimate.BelowThreshold:
		t.Fatalf("view = remaining %v, estimate %+v", v.Remaining, v.Estimate)
	}
	// A small non-sensitive club needs no k ≥ 10 either.
	e.audience.add(4, "club:"+orgID("club_a").String())
	if _, err := e.svc.CreateTargetedPost(ctx, persona(t, "club_admin"), orgID("club_a"), clubTargeted("club_a", "Kurul")); err != nil {
		t.Fatal(err)
	}
	// A university addresses its own students (k ≥ 10, no unit).
	e.audience.add(12, uniKey)
	u, err := e.svc.CreateTargetedPost(ctx, persona(t, "university_rep"), orgID("uni_a"), uniTargeted("Kariyer günü"))
	if err != nil || u.Post.EntitlementUsageID != nil || u.Remaining != nil {
		t.Fatalf("university post = %+v, %v", u.Post, err)
	}
}

// TestTargetedPost_SensitiveClubMemberPost_VisibleOnlyToMembers: once
// published, a sensitive club's post for its members is seen by its
// members (their club key, sensitive clubs included) and its staff only:
// not in another student's feed, not on the club page for outsiders, 404 by
// ID.
func TestTargetedPost_SensitiveClubMemberPost_VisibleOnlyToMembers(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	club := orgID("club_sensitive")
	clubKey := "club:" + club.String()
	e.clubs.set(club, true)
	e.trust("club_sensitive")
	admin := persona(t, "club_admin_sensitive")
	v, err := e.svc.CreateTargetedPost(ctx, admin, club, clubTargeted("club_sensitive", "Üyelere"))
	if err != nil {
		t.Fatal(err)
	}
	if p, err := e.svc.PublishPost(ctx, admin, v.Post.ID); err != nil || p.Post.Status != feed.StatusPublished {
		t.Fatalf("PublishPost = %+v, %v", p.Post, err)
	}
	member, outsider := persona(t, "club_member"), persona(t, "student")
	e.viewers.keys[member.UserID] = []string{uniKey, clubKey}
	e.viewers.keys[outsider.UserID] = []string{uniKey}
	if !contains(e.feedIDs(t, member, feed.FeedQuery{}), v.Post.ID) {
		t.Fatal("a member does not see the club's post")
	}
	if contains(e.feedIDs(t, outsider, feed.FeedQuery{}), v.Post.ID) {
		t.Fatal("an outsider sees a sensitive club's member post")
	}
	if _, err := e.svc.GetPost(ctx, outsider, v.Post.ID); !errors.Is(err, feed.ErrNotFound) {
		wantNotFound(t, err)
	}
	if _, err := e.svc.GetPost(ctx, member, v.Post.ID); err != nil {
		t.Fatal(err)
	}
	page, err := e.svc.ListPosts(ctx, outsider, club, feed.PageQuery{})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("club page of an outsider = %d, %v", len(page.Items), err)
	}
	if _, err := e.svc.GetPost(ctx, authz.Anonymous(), v.Post.ID); err == nil {
		t.Fatal("an anonymous reader sees a member post")
	}
	if _, err := e.svc.GetPost(ctx, persona(t, "moderator"), v.Post.ID); err != nil {
		t.Fatalf("moderator: %v", err)
	}
}

// TestDeleteTargetedDraft_ReleasesEntitlement: deleting a company's
// targeted draft gives its unit back in the same transaction (04 §2.7
// "+ hak iadesi"); a published one becomes removed and keeps it (used).
func TestDeleteTargetedDraft_ReleasesEntitlement(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	e.audience.add(25, uniKey)
	e.grantPosts(t, "company_a", 2)
	e.trust("company_a")
	cm := persona(t, "company_member")
	draft, err := e.svc.CreateTargetedPost(ctx, cm, orgID("company_a"), uniTargeted("Taslak"))
	if err != nil {
		t.Fatal(err)
	}
	if e.remaining(t, "company_a") != 1 {
		t.Fatalf("remaining after create = %d", e.remaining(t, "company_a"))
	}
	if err := e.svc.DeletePost(ctx, cm, draft.Post.ID); err != nil {
		t.Fatal(err)
	}
	if e.remaining(t, "company_a") != 2 {
		t.Fatalf("remaining after delete = %d", e.remaining(t, "company_a"))
	}
	if got := e.audit.Of(authz.FeedPostDelete); len(got) != 1 || got[0].Metadata["quota_released"] != true {
		t.Fatalf("audit = %+v", got)
	}
	// Published, then removed: the unit was used.
	pub, err := e.svc.CreateTargetedPost(ctx, cm, orgID("company_a"), uniTargeted("Yayın"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.PublishPost(ctx, cm, pub.Post.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.DeletePost(ctx, cm, pub.Post.ID); err != nil {
		t.Fatal(err)
	}
	if e.remaining(t, "company_a") != 1 {
		t.Fatalf("remaining after removing a published post = %d", e.remaining(t, "company_a"))
	}
}

// TestTargetedPost_AudienceIsFixed: a targeted post keeps the audience it
// was compiled with: updatePost cannot make it public (422), nor make a
// public post a segment one; its keys survive an edit and publication.
func TestTargetedPost_AudienceIsFixed(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	e.audience.add(15, uniKey)
	rep := persona(t, "university_rep")
	v, err := e.svc.CreateTargetedPost(ctx, rep, orgID("uni_a"), uniTargeted("Kariyer"))
	if err != nil {
		t.Fatal(err)
	}
	public := feed.AudiencePublic
	_, err = e.svc.UpdatePost(ctx, rep, v.Post.ID, v.ETag, feed.PostPatch{AudienceKind: &public})
	wantField(t, err, "body.audience_kind")
	title := "Kariyer günü"
	u, err := e.svc.UpdatePost(ctx, rep, v.Post.ID, v.ETag, feed.PostPatch{Title: &title})
	if err != nil || !slices.Equal(u.Post.AudienceKeys, []string{uniKey}) || u.Post.AudienceKind != feed.AudienceSegment {
		t.Fatalf("UpdatePost = %+v, %v", u.Post, err)
	}
	e.trust("uni_a")
	p, err := e.svc.PublishPost(ctx, rep, v.Post.ID)
	if err != nil || !slices.Equal(p.Post.AudienceKeys, []string{uniKey}) {
		t.Fatalf("PublishPost = %+v, %v", p.Post, err)
	}
	// A student of uni_a sees it, one of another university does not.
	in, out := persona(t, "student"), persona(t, "other_club_admin")
	e.viewers.keys[in.UserID] = []string{uniKey}
	e.viewers.keys[out.UserID] = []string{"uni:" + uuid.NewV7().String()}
	if _, err := e.svc.GetPost(ctx, in, v.Post.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.GetPost(ctx, out, v.Post.ID); err == nil {
		t.Fatal("a student outside the audience sees the post")
	}
	// createPost never takes a segment audience.
	seg := feed.AudienceSegment
	_, err = e.svc.CreatePost(ctx, rep, orgID("uni_a"), feed.PostInput{Kind: feed.KindText, Body: doc("x"), AudienceKind: seg})
	wantField(t, err, "body.audience_kind")
}

// TestTargetedPost_SuspendedOrgAndMissingWiring: a suspended org takes no
// targeted post (403 feed.posting_suspended); without outreach or (for a
// company) entitlements the endpoint answers 503 rather than skipping a
// rule.
func TestTargetedPost_SuspendedOrgAndMissingWiring(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	reason := "spam"
	e.mem.PutSettings(feed.OrgSettings{OrgID: orgID("company_a"), TrustLevel: feed.TrustSuspended, Reason: &reason})
	_, err := e.svc.CreateTargetedPost(ctx, persona(t, "company_member"), orgID("company_a"), uniTargeted("x"))
	problemtest.WantProblem(t, err, 403, "feed.posting_suspended")

	n := newEnvWith(t, func(d *feed.Deps) { d.Entitlements = nil })
	_, err = n.svc.CreateTargetedPost(ctx, persona(t, "company_member"), orgID("company_a"), uniTargeted("x"))
	problemtest.WantProblem(t, err, 503, "unavailable")
	m := newEnvWith(t, func(d *feed.Deps) { d.Targeting = nil })
	_, err = m.svc.CreateTargetedPost(ctx, persona(t, "club_admin"), orgID("club_a"), clubTargeted("club_a", "x"))
	problemtest.WantProblem(t, err, 503, "unavailable")
	// Another org's member: 404 (org management surface).
	_, err = e.svc.CreateTargetedPost(ctx, persona(t, "other_club_admin"), orgID("club_a"), clubTargeted("club_a", "x"))
	wantNotFound(t, err)
}

// TestPublishTargeted_SensitiveSinceDraft_422: a company's draft whose club
// became sensitive after compilation is not published (03 Y2).
func TestPublishTargeted_SensitiveSinceDraft_422(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	clubKey := "club:" + orgID("club_a").String()
	e.audience.add(20, clubKey)
	e.grantPosts(t, "company_a", 1)
	e.trust("company_a")
	cm := persona(t, "company_member")
	v, err := e.svc.CreateTargetedPost(ctx, cm, orgID("company_a"), clubTargeted("club_a", "Kulübe"))
	if err != nil {
		t.Fatal(err)
	}
	e.clubs.set(orgID("club_a"), true)
	_, err = e.svc.PublishPost(ctx, cm, v.Post.ID)
	problemtest.WantProblem(t, err, 422, "outreach.sensitive_target")
}
