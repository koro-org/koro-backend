package feed_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"koro/internal/entitlements"
	"koro/internal/feed"
	"koro/internal/outreach"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/problem/problemtest"
)

// BE-035: the feed's side of the dispatch — "notify on publish" on a
// targeted post schedules the dispatch in the post's transaction, and a
// post that leaves before it was ever published withdraws it.

// grantDispatches gives org n targeted_dispatch units.
func (e *env) grantDispatches(t *testing.T, org string, n int) {
	t.Helper()
	by := orgID("platform_admin")
	if _, err := e.ents.Grant(t.Context(), entitlements.GrantInput{
		OrgID: orgID(org), Feature: entitlements.FeatureTargetedDispatch, Quantity: &n, ValidFrom: t0.Add(-time.Hour),
		ValidTo: t0.Add(30 * 24 * time.Hour), Status: entitlements.StatusActive, SourceType: entitlements.SourceAdminGrant,
		GrantedBy: &by,
	}); err != nil {
		t.Fatal(err)
	}
}

func (e *env) dispatchUnits(t *testing.T, org string) int {
	t.Helper()
	av, err := e.ents.Available(t.Context(), orgID(org), entitlements.FeatureTargetedDispatch)
	if err != nil {
		t.Fatal(err)
	}
	return av.Remaining
}

func withDispatch(in feed.TargetedInput, chans ...outreach.Channel) feed.TargetedInput {
	in.Dispatch = &outreach.DispatchRequest{Channels: chans}
	return in
}

// TestTargetedPost_NotifyOnPublish_SchedulesDispatch: a company's targeted
// post with "notify on publish" schedules a commercial dispatch in the
// same transaction (one targeted_dispatch unit, outreach.dispatch_scheduled)
// and answers its ID; a club's notice to its members spends nothing and is
// informational. A commercial in-app dispatch is refused.
func TestTargetedPost_NotifyOnPublish_SchedulesDispatch(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	e.audience.add(30, uniKey)
	e.grantPosts(t, "company_a", 2)
	e.grantDispatches(t, "company_a", 2)
	cm := persona(t, "company_member")

	v, err := e.svc.CreateTargetedPost(ctx, cm, orgID("company_a"), withDispatch(uniTargeted("Staj"), outreach.ChannelPush))
	if err != nil {
		t.Fatal(err)
	}
	if v.DispatchID == nil || feed.ToTargetedDTO(v).DispatchID != v.DispatchID.String() {
		t.Fatalf("dispatch id = %v", v.DispatchID)
	}
	d, err := e.dispatches.GetDispatch(ctx, orgID("company_a"), *v.DispatchID)
	switch {
	case err != nil:
		t.Fatal(err)
	case d.PostID != v.Post.ID || d.Status != outreach.DispatchScheduled || !d.Commercial || d.EntitlementUsageID == nil:
		t.Fatalf("dispatch = %+v", d)
	case !slices.Equal([]string(d.AudienceKeys), []string{uniKey}) || d.DeepLink != "/posts/"+v.Post.ID.String():
		t.Fatalf("dispatch targeting = %v %s", d.AudienceKeys, d.DeepLink)
	}
	if n := e.dispatchUnits(t, "company_a"); n != 1 {
		t.Fatalf("targeted_dispatch left = %d, want 1", n)
	}
	if !slices.Contains(e.out.Topics(), domainevent.TopicOutreachDispatchScheduled) {
		t.Fatalf("outbox = %v", e.out.Topics())
	}

	// A commercial dispatch never goes in-app (notify: no opt-out there).
	_, err = e.svc.CreateTargetedPost(ctx, cm, orgID("company_a"), withDispatch(uniTargeted("Staj 2"), outreach.ChannelInApp))
	problemtest.WantProblem(t, err, 422, "validation_failed")

	// A club's notice to its own members: informational, no unit.
	e.audience.add(3, "club:"+orgID("club_a").String())
	cv, err := e.svc.CreateTargetedPost(ctx, persona(t, "club_admin"), orgID("club_a"),
		withDispatch(clubTargeted("club_a", "Genel kurul"), outreach.ChannelInApp, outreach.ChannelPush))
	if err != nil {
		t.Fatal(err)
	}
	cd, err := e.dispatches.GetDispatch(ctx, orgID("club_a"), *cv.DispatchID)
	if err != nil || cd.Commercial || cd.EntitlementUsageID != nil || !slices.Equal([]string(cd.Channels), []string{"in_app", "push"}) {
		t.Fatalf("club dispatch = %+v, %v", cd, err)
	}
}

// TestDeletePost_DraftWithDispatch_DiscardsIt: deleting the draft gives
// both units back (targeted_post and targeted_dispatch) and removes the
// dispatch before the post (the composite FK); a moderator rejecting a
// post in review does the same.
func TestDeletePost_DraftWithDispatch_DiscardsIt(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	e.audience.add(30, uniKey)
	e.grantPosts(t, "company_a", 2)
	e.grantDispatches(t, "company_a", 2)
	cm := persona(t, "company_member")

	v, err := e.svc.CreateTargetedPost(ctx, cm, orgID("company_a"), withDispatch(uniTargeted("Staj"), outreach.ChannelPush))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.DeletePost(ctx, cm, v.Post.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.dispatches.GetDispatch(ctx, orgID("company_a"), *v.DispatchID); !errors.Is(err, outreach.ErrNotFound) {
		t.Fatalf("dispatch after the draft's deletion: %v", err)
	}
	if e.remaining(t, "company_a") != 2 || e.dispatchUnits(t, "company_a") != 2 {
		t.Fatalf("units = %d posts, %d dispatches; want both back", e.remaining(t, "company_a"), e.dispatchUnits(t, "company_a"))
	}

	// Rejected in review (company_a is new: its post goes to review).
	v, err = e.svc.CreateTargetedPost(ctx, cm, orgID("company_a"), withDispatch(uniTargeted("Staj 2"), outreach.ChannelPush))
	if err != nil {
		t.Fatal(err)
	}
	if p, err := e.svc.PublishPost(ctx, cm, v.Post.ID); err != nil || p.Post.Status != feed.StatusPendingReview {
		t.Fatalf("publish = %s, %v", p.Post.Status, err)
	}
	e.moderate(t, "moderator", v.Post.ID, feed.ModRemove, "spam")
	if _, err := e.dispatches.GetDispatch(ctx, orgID("company_a"), *v.DispatchID); !errors.Is(err, outreach.ErrNotFound) {
		t.Fatalf("dispatch after the rejection: %v", err)
	}
	if e.dispatchUnits(t, "company_a") != 2 {
		t.Fatalf("targeted_dispatch left = %d after the rejection", e.dispatchUnits(t, "company_a"))
	}
}

// TestPostStats_DispatchCounts (04 §2.7 getPostStats, 06 H-61): a targeted
// post with a dispatch shows its counts as published estimates only (null
// before the run and below 10, rounded down otherwise, even when the row
// holds an exact number); a post without a dispatch has no dispatch block.
func TestPostStats_DispatchCounts(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	e.audience.add(30, uniKey)
	e.grantPosts(t, "company_a", 2)
	e.grantDispatches(t, "company_a", 1)
	cm := persona(t, "company_member")

	v, err := e.svc.CreateTargetedPost(ctx, cm, orgID("company_a"), withDispatch(uniTargeted("Staj"), outreach.ChannelPush))
	if err != nil {
		t.Fatal(err)
	}
	st, err := e.svc.PostStats(ctx, cm, v.Post.ID)
	if err != nil || st.Dispatch == nil || st.Dispatch.Eligible != nil || st.Dispatch.Consented != nil || st.Dispatch.Sent != nil {
		t.Fatalf("stats before the run = %+v, %v", st.Dispatch, err)
	}
	d, err := e.dispatches.GetDispatch(ctx, orgID("company_a"), *v.DispatchID)
	if err != nil {
		t.Fatal(err)
	}
	n := func(v int) *int { return &v }
	d.EligibleCount, d.ConsentedCount, d.SentCount = n(137), n(9), n(12)
	if err := e.dispatches.PutDispatch(d); err != nil {
		t.Fatal(err)
	}
	st, err = e.svc.PostStats(ctx, cm, v.Post.ID)
	if err != nil {
		t.Fatal(err)
	}
	dto := feed.ToStatsDTO(st)
	switch c := dto.Dispatch; {
	case c == nil || c.EligibleCount == nil || *c.EligibleCount != 100:
		t.Fatalf("eligible = %+v (137 → 100)", c)
	case c.ConsentedCount != nil:
		t.Fatalf("consented = %d: below 10 is null", *c.ConsentedCount)
	case c.SentCount == nil || *c.SentCount != 10:
		t.Fatalf("sent = %v (12 → 10)", c.SentCount)
	}

	other, err := e.svc.CreateTargetedPost(ctx, cm, orgID("company_a"), uniTargeted("Staj 2"))
	if err != nil {
		t.Fatal(err)
	}
	if st, err := e.svc.PostStats(ctx, cm, other.Post.ID); err != nil || st.Dispatch != nil || feed.ToStatsDTO(st).Dispatch != nil {
		t.Fatalf("stats without a dispatch = %+v, %v", st.Dispatch, err)
	}
}

// TestGetPostManage_DispatchIDAndScope (BE-127): the management view (one
// post and the list) carries a targeted post's dispatch_id; a post without
// a dispatch has none; another org's post is 404 under this org's path; a
// reader who is not the org's writer is refused.
func TestGetPostManage_DispatchIDAndScope(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	e.audience.add(30, uniKey)
	e.grantPosts(t, "company_a", 2)
	e.grantDispatches(t, "company_a", 1)
	cm := persona(t, "company_member")
	with, err := e.svc.CreateTargetedPost(ctx, cm, orgID("company_a"), withDispatch(uniTargeted("Staj"), outreach.ChannelPush))
	if err != nil {
		t.Fatal(err)
	}
	without, err := e.svc.CreateTargetedPost(ctx, cm, orgID("company_a"), uniTargeted("Burs"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := e.svc.GetOrgPost(ctx, cm, orgID("company_a"), with.Post.ID)
	if err != nil || got.DispatchID == nil || *got.DispatchID != *with.DispatchID || got.ETag == "" || !got.Manage {
		t.Fatalf("GetOrgPost = %+v, %v", got, err)
	}
	if d := feed.ToManageDTO(got); d.DispatchID != with.DispatchID.String() || d.Status != "draft" {
		t.Fatalf("dto = %+v", d)
	}
	if got, err := e.svc.GetOrgPost(ctx, cm, orgID("company_a"), without.Post.ID); err != nil || got.DispatchID != nil {
		t.Fatalf("post without a dispatch = %v, %v", got.DispatchID, err)
	}
	p, err := e.svc.ListOrgPosts(ctx, cm, orgID("company_a"), feed.ManageQuery{})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, v := range p.Items {
		ids[v.Post.ID.String()] = feed.ToManageDTO(v).DispatchID
	}
	if ids[with.Post.ID.String()] != with.DispatchID.String() || ids[without.Post.ID.String()] != "" {
		t.Fatalf("list dispatch ids = %v", ids)
	}
	_, err = e.svc.GetOrgPost(ctx, persona(t, "club_admin"), orgID("club_a"), with.Post.ID)
	problemtest.WantErr(t, err, feed.ErrNotFound)
	_, err = e.svc.GetOrgPost(ctx, persona(t, "student"), orgID("company_a"), with.Post.ID)
	if err == nil {
		t.Fatal("a student read the management view")
	}
}
