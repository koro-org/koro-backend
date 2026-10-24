package feed_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/feed"
	"koro/internal/files"
	"koro/internal/orgs"
	"koro/internal/platform/domainevent"
)

// stats reads the counters of a post as its author org does.
func (e *env) stats(t *testing.T, writer string, id uuid.UUID) feed.Stats {
	t.Helper()
	st, err := e.svc.PostStats(t.Context(), persona(t, writer), id)
	if err != nil {
		t.Fatalf("PostStats: %v", err)
	}
	return st
}

// envelope wraps a payload as the outbox delivers it.
func envelope(t *testing.T, p domainevent.Payload, aggID uuid.UUID) domainevent.Envelope {
	t.Helper()
	e, err := domainevent.New(p, "membership", aggID)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// TestLike_Twice_Idempotent_CountOnce: two PUTs are one like and one count
// (04 §F.3) and one post.liked; the ETag of the post does not move with
// its counters; the author org sees the count, never who liked (03 Y7).
func TestLike_Twice_Idempotent_CountOnce(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	v := e.published(t, "club_admin", "club_a", textInput("Beğenilecek"))
	before, _ := e.svc.GetPost(ctx, persona(t, "club_admin"), v.Post.ID)
	reader := persona(t, "student")
	for range 2 {
		if err := e.svc.LikePost(ctx, reader, v.Post.ID); err != nil {
			t.Fatal(err)
		}
	}
	if st := e.stats(t, "club_admin", v.Post.ID); st.LikeCount != 1 {
		t.Fatalf("like_count = %d", st.LikeCount)
	}
	if n := countTopic(e, domainevent.TopicPostLiked); n != 1 {
		t.Fatalf("post.liked events = %d", n)
	}
	got, err := e.svc.GetPost(ctx, reader, v.Post.ID)
	if err != nil || !got.Viewer.Liked || got.Post.LikeCount != 1 {
		t.Fatalf("GetPost = %+v, %v", got.Viewer, err)
	}
	if got.ETag != before.ETag {
		t.Fatalf("a like changed the ETag: %s → %s", before.ETag, got.ETag)
	}
	// A second reader adds one; saves count separately and twice is once.
	if err := e.svc.LikePost(ctx, persona(t, "club_member"), v.Post.ID); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := e.svc.SavePost(ctx, reader, v.Post.ID); err != nil {
			t.Fatal(err)
		}
	}
	if st := e.stats(t, "club_admin", v.Post.ID); st.LikeCount != 2 || st.SaveCount != 1 {
		t.Fatalf("stats = %+v", st)
	}
	if n := countTopic(e, domainevent.TopicPostSaved); n != 1 {
		t.Fatalf("post.saved events = %d", n)
	}
	// The event carries what talent needs; the readers' DTO and the stats
	// carry no liker.
	for _, ev := range e.out.Events() {
		if ev.Topic == domainevent.TopicPostLiked {
			p, err := domainevent.Decode[domainevent.PostLiked](ev)
			if err != nil || p.PostID != v.Post.ID || p.AuthorOrgID != orgID("club_a") {
				t.Fatalf("post.liked = %+v, %v", p, err)
			}
			break
		}
	}
	b, _ := json.Marshal(feed.ToManageDTO(got))
	if strings.Contains(string(b), reader.UserID.String()) {
		t.Fatalf("a post DTO names a liker: %s", b)
	}
	// Unlike twice: one decrement, one post.unliked.
	for range 2 {
		if err := e.svc.UnlikePost(ctx, reader, v.Post.ID); err != nil {
			t.Fatal(err)
		}
	}
	if st := e.stats(t, "club_admin", v.Post.ID); st.LikeCount != 1 {
		t.Fatalf("like_count after unlike = %d", st.LikeCount)
	}
	if n := countTopic(e, domainevent.TopicPostUnliked); n != 1 {
		t.Fatalf("post.unliked events = %d", n)
	}
}

func countTopic(e *env, topic string) int {
	n := 0
	for _, t := range e.out.Topics() {
		if t == topic {
			n++
		}
	}
	return n
}

// TestLike_InvisiblePost_404: a like, a save or a hide needs a post the
// reader may see (03 §3.6): another org's draft, a commercial post for a
// minor, a followers post for a stranger and a missing post are 404; a
// visible post that is not published (its writer's draft) is 409.
func TestLike_InvisiblePost_404(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	draft := e.create(t, "club_admin", "club_a", textInput("Taslak"))
	ad := e.published(t, "company_admin", "company_a", textInput("Staj"))
	in := textInput("Takipçilere")
	in.AudienceKind = feed.AudienceFollowers
	followers := e.published(t, "club_admin", "club_a", in)
	student, minor, stranger := persona(t, "student"), persona(t, "student_minor"), persona(t, "user_unverified")
	for _, c := range []struct {
		name string
		a    authz.Actor
		id   uuid.UUID
	}{
		{"draft", student, draft.Post.ID},
		{"commercial for a minor", minor, ad.Post.ID},
		{"followers post for a stranger", stranger, followers.Post.ID},
		{"missing", student, uuid.NewV7()},
	} {
		wantNotFound(t, e.svc.LikePost(ctx, c.a, c.id))
		wantNotFound(t, e.svc.SavePost(ctx, c.a, c.id))
		wantNotFound(t, e.svc.HidePost(ctx, c.a, c.id))
	}
	err := e.svc.LikePost(ctx, persona(t, "club_admin"), draft.Post.ID)
	wantCode(t, err, 409, "feed.invalid_state")
	// The org members see the followers post: they may like it.
	if err := e.svc.LikePost(ctx, persona(t, "club_member"), followers.Post.ID); err != nil {
		t.Fatal(err)
	}
	// A removed post takes no new like.
	if err := e.svc.DeletePost(ctx, persona(t, "club_admin"), followers.Post.ID); err != nil {
		t.Fatal(err)
	}
	wantNotFound(t, e.svc.LikePost(ctx, student, followers.Post.ID))
	if st, _ := e.mem.GetPost(ctx, ad.Post.ID); st.LikeCount != 0 || st.SaveCount != 0 {
		t.Fatalf("counters moved: %+v", st)
	}
}

// TestUnlike_Missing_204: unlike and unsave without a like or save are a
// no-op (204, no event, no decrement below zero). On a post the reader no
// longer sees, their own row still goes; a post they never touched stays
// 404 (no existence leak).
func TestUnlike_Missing_204(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	v := e.published(t, "club_admin", "club_a", textInput("Hiç beğenilmedi"))
	reader := persona(t, "student")
	if err := e.svc.UnlikePost(ctx, reader, v.Post.ID); err != nil {
		t.Fatalf("UnlikePost(no like) = %v", err)
	}
	if err := e.svc.UnsavePost(ctx, reader, v.Post.ID); err != nil {
		t.Fatalf("UnsavePost(no save) = %v", err)
	}
	if st := e.stats(t, "club_admin", v.Post.ID); st.LikeCount != 0 || st.SaveCount != 0 {
		t.Fatalf("stats = %+v", st)
	}
	if n := countTopic(e, domainevent.TopicPostUnliked); n != 0 {
		t.Fatalf("post.unliked for nothing: %d", n)
	}
	wantNotFound(t, e.svc.UnlikePost(ctx, reader, uuid.NewV7()))
	// A liked and saved post is removed: the reader may still clean up.
	if err := e.svc.LikePost(ctx, reader, v.Post.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SavePost(ctx, reader, v.Post.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.DeletePost(ctx, persona(t, "club_admin"), v.Post.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.UnlikePost(ctx, reader, v.Post.ID); err != nil {
		t.Fatalf("UnlikePost(removed, liked) = %v", err)
	}
	if err := e.svc.UnsavePost(ctx, reader, v.Post.ID); err != nil {
		t.Fatalf("UnsavePost(removed, saved) = %v", err)
	}
	wantNotFound(t, e.svc.UnlikePost(ctx, reader, v.Post.ID))
	wantNotFound(t, e.svc.UnlikePost(ctx, persona(t, "club_member"), v.Post.ID))
	if p, _ := e.mem.GetPost(ctx, v.Post.ID); p.LikeCount != 0 || p.SaveCount != 0 {
		t.Fatalf("counters = %d, %d", p.LikeCount, p.SaveCount)
	}
}

// TestHidePost_ExcludedFromFeed: hidePost takes a post out of the reader's
// feed only (not other readers', not the org page); twice is once.
func TestHidePost_ExcludedFromFeed(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	a := e.published(t, "club_admin", "club_a", textInput("gizlenecek"))
	b := e.published(t, "club_admin", "club_a", textInput("kalacak"))
	reader := persona(t, "student")
	for range 2 {
		if err := e.svc.HidePost(ctx, reader, a.Post.ID); err != nil {
			t.Fatal(err)
		}
	}
	ids := e.feedIDs(t, reader, feed.FeedQuery{})
	if contains(ids, a.Post.ID) || !contains(ids, b.Post.ID) {
		t.Fatalf("feed = %v", ids)
	}
	if other := e.feedIDs(t, persona(t, "club_member"), feed.FeedQuery{}); !contains(other, a.Post.ID) {
		t.Fatal("the hide leaked to another reader")
	}
	page, err := e.svc.ListPosts(ctx, reader, orgID("club_a"), feed.PageQuery{})
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("org page = %d, %v", len(page.Items), err)
	}
	in, _ := e.mem.InteractionsOf(ctx, reader.UserID)
	if len(in.Hides) != 1 {
		t.Fatalf("hides = %d", len(in.Hides))
	}
}

// TestMuteOrg_ExcludedFromFeed: muteOrg takes the org's posts out of the
// reader's feed (both tabs; 04 §F.5) but not off its page; unmute brings
// them back; both are idempotent and listed for their owner only.
func TestMuteOrg_ExcludedFromFeed(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	a := e.published(t, "club_admin", "club_a", textInput("A"))
	b := e.published(t, "other_club_admin", "club_b", textInput("B"))
	reader := persona(t, "student")
	if err := e.svc.FollowOrg(ctx, reader, orgID("club_a"), nil); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := e.svc.MuteOrg(ctx, reader, orgID("club_a")); err != nil {
			t.Fatal(err)
		}
	}
	for _, tab := range []string{feed.TabDiscover, feed.TabFollowing} {
		if ids := e.feedIDs(t, reader, feed.FeedQuery{Tab: tab}); contains(ids, a.Post.ID) {
			t.Fatalf("%s tab shows a muted org: %v", tab, ids)
		}
	}
	if ids := e.feedIDs(t, reader, feed.FeedQuery{}); !contains(ids, b.Post.ID) {
		t.Fatal("another org vanished")
	}
	if page, err := e.svc.ListPosts(ctx, reader, orgID("club_a"), feed.PageQuery{}); err != nil || len(page.Items) != 1 {
		t.Fatalf("org page of a muted org = %d, %v", len(page.Items), err)
	}
	mutes, err := e.svc.ListMutes(ctx, reader, feed.PageQuery{})
	if err != nil || len(mutes.Items) != 1 || mutes.Items[0].Org.ID != orgID("club_a") {
		t.Fatalf("ListMutes = %+v, %v", mutes, err)
	}
	if other, _ := e.svc.ListMutes(ctx, persona(t, "club_member"), feed.PageQuery{}); len(other.Items) != 0 {
		t.Fatal("another reader sees the mute")
	}
	for range 2 {
		if err := e.svc.UnmuteOrg(ctx, reader, orgID("club_a")); err != nil {
			t.Fatal(err)
		}
	}
	if ids := e.feedIDs(t, reader, feed.FeedQuery{}); !contains(ids, a.Post.ID) {
		t.Fatal("unmute did not bring the org back")
	}
	wantNotFound(t, e.svc.MuteOrg(ctx, reader, uuid.NewV7()))
	// A pending company's profile is its members' only: 404 for others.
	e.orgs.setStatus(orgID("company_b"), orgs.StatusPendingReview)
	wantNotFound(t, e.svc.MuteOrg(ctx, reader, orgID("company_b")))
	wantNotFound(t, e.svc.FollowOrg(ctx, reader, orgID("company_b"), nil))
	// A closed org takes no new follow or mute.
	e.orgs.setStatus(orgID("company_a"), orgs.StatusClosed)
	wantCode(t, e.svc.MuteOrg(ctx, persona(t, "company_admin"), orgID("company_a")), 409, "orgs.invalid_state")
}

// TestFollowOrg_ManualAndNotify: followOrg is idempotent, keeps notify
// unless given, turns an automatic follow into a manual one, and lists
// follows newest first with their source; a sensitive club may be
// followed by hand.
func TestFollowOrg_ManualAndNotify(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	reader := persona(t, "student")
	on := true
	if err := e.svc.FollowOrg(ctx, reader, orgID("club_a"), &on); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.FollowOrg(ctx, reader, orgID("club_a"), nil); err != nil {
		t.Fatal(err)
	}
	if f, ok := e.mem.FollowOf(reader.UserID, orgID("club_a")); !ok || !f.Notify || f.Source != feed.FollowManual {
		t.Fatalf("follow = %+v, %v", f, ok)
	}
	e.clk.Advance(time.Second)
	e.clubs.set(orgID("club_sensitive"), true)
	if err := e.svc.FollowOrg(ctx, reader, orgID("club_sensitive"), nil); err != nil {
		t.Fatal(err)
	}
	// An automatic follow the user confirms becomes theirs.
	e.clk.Advance(time.Second)
	if _, err := e.mem.AddAutoFollow(ctx, reader.UserID, orgID("club_b")); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.FollowOrg(ctx, reader, orgID("club_b"), nil); err != nil {
		t.Fatal(err)
	}
	if f, _ := e.mem.FollowOf(reader.UserID, orgID("club_b")); f.Source != feed.FollowManual {
		t.Fatalf("confirmed auto follow = %+v", f)
	}
	p1, err := e.svc.ListFollows(ctx, reader, feed.PageQuery{Limit: 2})
	if err != nil || len(p1.Items) != 2 || p1.Next == nil || p1.Items[0].Org.ID != orgID("club_b") {
		t.Fatalf("ListFollows page 1 = %+v, %v", p1, err)
	}
	p2, err := e.svc.ListFollows(ctx, reader, feed.PageQuery{Limit: 2, After: p1.Next})
	if err != nil || len(p2.Items) != 1 || p2.Next != nil || p2.Items[0].Org.ID != orgID("club_a") || !p2.Items[0].Follow.Notify {
		t.Fatalf("ListFollows page 2 = %+v, %v", p2, err)
	}
	// following_author follows the row.
	v := e.published(t, "club_admin", "club_a", textInput("takip"))
	got, err := e.svc.GetPost(ctx, reader, v.Post.ID)
	if err != nil || !got.Viewer.FollowingAuthor {
		t.Fatalf("viewer = %+v, %v", got.Viewer, err)
	}
	for range 2 {
		if err := e.svc.UnfollowOrg(ctx, reader, orgID("club_a")); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := e.mem.FollowOf(reader.UserID, orgID("club_a")); ok {
		t.Fatal("unfollow left the row")
	}
	wantNotFound(t, e.svc.UnfollowOrg(ctx, reader, uuid.NewV7()))
}

// TestMembershipStarted_AutoFollow_NotForSensitiveClub: membership.started
// makes the member follow the org (auto_membership), except in a sensitive
// club, whose follow would reveal the membership (02 §12.7), and except a
// club whose sensitivity directory cannot tell (fail closed). A club that
// becomes sensitive loses its automatic follows.
func TestMembershipStarted_AutoFollow_NotForSensitiveClub(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	user := uuid.NewV7()
	e.clubs.set(orgID("club_sensitive"), true)
	ms := []authz.Membership{
		{OrgID: orgID("club_a"), OrgKind: authz.OrgClub, Role: authz.MemberMember},
		{OrgID: orgID("club_sensitive"), OrgKind: authz.OrgClub, Role: authz.MemberMember},
		{OrgID: orgID("company_a"), OrgKind: authz.OrgCompany, Role: authz.MemberRecruiter},
	}
	e.orgs.setMembers(user, ms...)
	for _, m := range ms {
		ev := envelope(t, domainevent.MembershipStarted{MembershipID: uuid.NewV7(), OrgID: m.OrgID, OrgKind: string(m.OrgKind),
			UserID: user, Role: string(m.Role)}, m.OrgID)
		for range 2 { // redelivery is harmless
			if err := feed.HandleMembershipStarted(e.mod, ctx, ev); err != nil {
				t.Fatal(err)
			}
		}
	}
	if f, ok := e.mem.FollowOf(user, orgID("club_a")); !ok || f.Source != feed.FollowAutoMembership || f.Notify {
		t.Fatalf("club_a follow = %+v, %v", f, ok)
	}
	if _, ok := e.mem.FollowOf(user, orgID("company_a")); !ok {
		t.Fatal("no automatic follow of the company")
	}
	if _, ok := e.mem.FollowOf(user, orgID("club_sensitive")); ok {
		t.Fatal("a sensitive club membership created a follow")
	}
	a := authz.Actor{Kind: authz.KindUser, UserID: user, Memberships: ms}
	list, err := e.svc.ListFollows(ctx, a, feed.PageQuery{})
	if err != nil || len(list.Items) != 2 {
		t.Fatalf("ListFollows = %+v, %v", list, err)
	}
	for _, it := range list.Items {
		if it.Org.ID == orgID("club_sensitive") {
			t.Fatal("the follow list reveals the sensitive membership")
		}
	}
	// A member of the sensitive club still reads its followers posts (T-250).
	in := textInput("Üyelere")
	in.AudienceKind = feed.AudienceFollowers
	e.trust("club_sensitive")
	admin := authz.Actor{Kind: authz.KindUser, UserID: uuid.NewV7(), Memberships: []authz.Membership{
		{OrgID: orgID("club_sensitive"), OrgKind: authz.OrgClub, Role: authz.MemberClubAdmin}}}
	d, err := e.svc.CreatePost(ctx, admin, orgID("club_sensitive"), in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.PublishPost(ctx, admin, d.Post.ID); err != nil {
		t.Fatal(err)
	}
	if ids := e.feedIDs(t, a, feed.FeedQuery{Tab: feed.TabFollowing}); !contains(ids, d.Post.ID) {
		t.Fatal("a sensitive club member misses its followers post")
	}
	// Without directory, a club counts as sensitive: no automatic follow.
	blind := newEnvWith(t, func(d *feed.Deps) { d.Clubs = nil })
	other := uuid.NewV7()
	blind.orgs.setMembers(other, ms[0])
	ev := envelope(t, domainevent.MembershipStarted{MembershipID: uuid.NewV7(), OrgID: orgID("club_a"), OrgKind: "club",
		UserID: other, Role: "member"}, orgID("club_a"))
	if err := feed.HandleMembershipStarted(blind.mod, ctx, ev); err != nil {
		t.Fatal(err)
	}
	if _, ok := blind.mem.FollowOf(other, orgID("club_a")); ok {
		t.Fatal("an unknown club got an automatic follow")
	}
	// club_a becomes sensitive: its automatic follows go, manual ones stay.
	fan := persona(t, "student")
	if err := e.svc.FollowOrg(ctx, fan, orgID("club_a"), nil); err != nil {
		t.Fatal(err)
	}
	chg := envelope(t, domainevent.ClubSensitivityChanged{ClubOrgID: orgID("club_a"), Sensitive: true}, orgID("club_a"))
	if err := feed.HandleSensitivityChanged(e.mod, ctx, chg); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.mem.FollowOf(user, orgID("club_a")); ok {
		t.Fatal("the automatic follow survived the sensitivity change")
	}
	if _, ok := e.mem.FollowOf(fan.UserID, orgID("club_a")); !ok {
		t.Fatal("a manual follow was dropped")
	}
}

// TestMembershipEnded_RemovesOnlyAutoFollow: membership.ended removes the
// automatic follow, never a manual one, and keeps it while another active
// membership of the org remains (a role change); events arriving out of
// order end in the state of the memberships.
func TestMembershipEnded_RemovesOnlyAutoFollow(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	auto, manual := uuid.NewV7(), uuid.NewV7()
	m := authz.Membership{OrgID: orgID("club_a"), OrgKind: authz.OrgClub, Role: authz.MemberMember}
	started := func(u uuid.UUID) domainevent.Envelope {
		return envelope(t, domainevent.MembershipStarted{MembershipID: uuid.NewV7(), OrgID: m.OrgID, OrgKind: "club",
			UserID: u, Role: "member"}, m.OrgID)
	}
	ended := func(u uuid.UUID) domainevent.Envelope {
		return envelope(t, domainevent.MembershipEnded{MembershipID: uuid.NewV7(), OrgID: m.OrgID, OrgKind: "club",
			UserID: u, Role: "member", Reason: "left"}, m.OrgID)
	}
	for _, u := range []uuid.UUID{auto, manual} {
		e.orgs.setMembers(u, m)
		if err := feed.HandleMembershipStarted(e.mod, ctx, started(u)); err != nil {
			t.Fatal(err)
		}
	}
	// manual confirms the follow by hand.
	if err := e.svc.FollowOrg(ctx, authz.Actor{Kind: authz.KindUser, UserID: manual}, m.OrgID, nil); err != nil {
		t.Fatal(err)
	}
	// A role change: the old membership ends while the new one is active.
	if err := feed.HandleMembershipEnded(e.mod, ctx, ended(auto)); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.mem.FollowOf(auto, m.OrgID); !ok {
		t.Fatal("a role change dropped the automatic follow")
	}
	for _, u := range []uuid.UUID{auto, manual} {
		e.orgs.setMembers(u)
		for range 2 {
			if err := feed.HandleMembershipEnded(e.mod, ctx, ended(u)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, ok := e.mem.FollowOf(auto, m.OrgID); ok {
		t.Fatal("the automatic follow survived the end of the membership")
	}
	if f, ok := e.mem.FollowOf(manual, m.OrgID); !ok || f.Source != feed.FollowManual {
		t.Fatalf("the manual follow = %+v, %v", f, ok)
	}
	// A late membership.started of an ended membership creates nothing.
	if err := feed.HandleMembershipStarted(e.mod, ctx, started(auto)); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.mem.FollowOf(auto, m.OrgID); ok {
		t.Fatal("a stale membership.started created a follow")
	}
	// A malformed payload is permanent (no retry).
	bad := domainevent.Envelope{Topic: domainevent.TopicMembershipEnded, Payload: []byte(`{`)}
	if err := feed.HandleMembershipEnded(e.mod, ctx, bad); err == nil {
		t.Fatal("a malformed payload was accepted")
	}
}

// TestReconcileCounters_FixesDrift: feed.reconcile_counters sets the
// counters back to the number of rows (a seeded drift both ways) and
// leaves correct posts alone; the job is scheduled hourly at :35.
func TestReconcileCounters_FixesDrift(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	a := e.published(t, "club_admin", "club_a", textInput("kayık"))
	b := e.published(t, "club_admin", "club_a", textInput("doğru"))
	if err := e.svc.LikePost(ctx, persona(t, "student"), b.Post.ID); err != nil {
		t.Fatal(err)
	}
	// Drift: rows without counts on a, a count without rows on a's save.
	e.mem.PutLike(persona(t, "club_member").UserID, a.Post.ID)
	e.mem.PutLike(persona(t, "student").UserID, a.Post.ID)
	p, _ := e.mem.GetPost(ctx, a.Post.ID)
	p.SaveCount = 7
	if err := e.mem.PutPost(p); err != nil {
		t.Fatal(err)
	}
	n, err := e.mem.ReconcileCounters(ctx)
	if err != nil || n != 1 {
		t.Fatalf("ReconcileCounters = %d, %v", n, err)
	}
	if st := e.stats(t, "club_admin", a.Post.ID); st.LikeCount != 2 || st.SaveCount != 0 {
		t.Fatalf("a = %+v", st)
	}
	if st := e.stats(t, "club_admin", b.Post.ID); st.LikeCount != 1 {
		t.Fatalf("b = %+v", st)
	}
	if err := feed.ReconcileCounters(e.mod, ctx); err != nil {
		t.Fatal(err)
	}
	jobs := e.mod.Jobs()
	if len(jobs) != 1 || jobs[0].Name != feed.ReconcileCountersJobName || jobs[0].Cron != "35 * * * *" || jobs[0].Queue != "low" {
		t.Fatalf("jobs = %+v", jobs)
	}
}

// TestListSavedPosts_SkipsNoLongerVisible: the saved list keeps the posts
// the reader may still see, newest save first, page by page; a removed
// post and one that moved out of reach (followers post after an unfollow)
// are skipped without shortening a page; the list is the owner's only.
func TestListSavedPosts_SkipsNoLongerVisible(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	reader := persona(t, "student")
	if err := e.svc.FollowOrg(ctx, reader, orgID("club_b"), nil); err != nil {
		t.Fatal(err)
	}
	in := textInput("Takipçilere")
	in.AudienceKind = feed.AudienceFollowers
	followers := e.published(t, "other_club_admin", "club_b", in)
	var visible []uuid.UUID
	removed := e.published(t, "club_admin", "club_a", textInput("kaldırılacak"))
	for i := range 4 {
		v := e.published(t, "club_admin", "club_a", textInput("kayıt "+string(rune('a'+i))))
		visible = append(visible, v.Post.ID)
	}
	for _, id := range []uuid.UUID{followers.Post.ID, removed.Post.ID, visible[0], visible[1], visible[2], visible[3]} {
		if err := e.svc.SavePost(ctx, reader, id); err != nil {
			t.Fatal(err)
		}
		e.clk.Advance(time.Millisecond)
	}
	if err := e.svc.DeletePost(ctx, persona(t, "club_admin"), removed.Post.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.UnfollowOrg(ctx, reader, orgID("club_b")); err != nil {
		t.Fatal(err)
	}
	var got []uuid.UUID
	q := feed.PageQuery{Limit: 2}
	for range 10 {
		p, err := e.svc.ListSavedPosts(ctx, reader, q)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range p.Items {
			if v.Viewer == nil || !v.Viewer.Saved {
				t.Fatalf("viewer.saved missing on %s", v.Post.ID)
			}
			got = append(got, v.Post.ID)
		}
		if p.Next == nil {
			break
		}
		if len(p.Items) != 2 {
			t.Fatalf("a short page before the end: %d", len(p.Items))
		}
		q.After = p.Next
	}
	want := []uuid.UUID{visible[3], visible[2], visible[1], visible[0]}
	if len(got) != len(want) {
		t.Fatalf("saved = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("saved = %v, want %v", got, want)
		}
	}
	if other, _ := e.svc.ListSavedPosts(ctx, persona(t, "club_member"), feed.PageQuery{}); len(other.Items) != 0 {
		t.Fatal("another reader sees the saves")
	}
	// A moderator's saved list keeps every published post (03 §3.6).
	mod := persona(t, "moderator")
	if err := e.svc.SavePost(ctx, mod, followers.Post.ID); err != nil {
		t.Fatal(err)
	}
	if p, _ := e.svc.ListSavedPosts(ctx, mod, feed.PageQuery{}); len(p.Items) != 1 {
		t.Fatalf("moderator saved = %d", len(p.Items))
	}
}

// TestPrivacy_Interactions: the KVKK provider exports the reader's likes,
// saves, hides, follows and mutes and erases them, taking the likes and
// saves off the counters; a second erase changes nothing.
func TestPrivacy_Interactions(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	v := e.published(t, "club_admin", "club_a", textInput("kişisel"))
	reader := persona(t, "student")
	for _, err := range []error{
		e.svc.LikePost(ctx, reader, v.Post.ID), e.svc.SavePost(ctx, reader, v.Post.ID),
		e.svc.HidePost(ctx, reader, v.Post.ID), e.svc.FollowOrg(ctx, reader, orgID("club_b"), nil),
		e.svc.MuteOrg(ctx, reader, orgID("company_a")),
		e.svc.LikePost(ctx, persona(t, "club_member"), v.Post.ID),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	sec, err := e.mod.Privacy.Export(ctx, reader.UserID)
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]int{}
	for _, it := range sec.Items {
		types[it["type"].(string)]++
	}
	for _, typ := range []string{"post_like", "post_save", "post_hide", "org_follow", "org_mute"} {
		if types[typ] != 1 {
			t.Fatalf("export types = %v", types)
		}
	}
	res, err := e.mod.Privacy.Erase(ctx, reader.UserID)
	if err != nil || res.Deleted != 5 || res.Retained != 0 {
		t.Fatalf("Erase = %+v, %v", res, err)
	}
	if st := e.stats(t, "club_admin", v.Post.ID); st.LikeCount != 1 || st.SaveCount != 0 {
		t.Fatalf("stats after erase = %+v", st)
	}
	if res, err := e.mod.Privacy.Erase(ctx, reader.UserID); err != nil || res.Deleted != 0 {
		t.Fatalf("second Erase = %+v, %v", res, err)
	}
	if sec, _ := e.mod.Privacy.Export(ctx, reader.UserID); len(sec.Items) != 0 {
		t.Fatalf("export after erase = %v", sec.Items)
	}
}

// TestFollowMuteLists_LogoAndVerified (BE-098, T-251): the org cards of the
// follow and mute lists carry the verification badge and a signed logo URL
// by the rules of the post author; a page reads its orgs with one GetMany
// and signs each logo once; a logo the scan has not cleared is left out.
func TestFollowMuteLists_LogoAndVerified(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	reader := persona(t, "student")
	logo := e.files.upload(orgID("club_a"), files.ScanClean)
	a := e.orgs.orgs[orgID("club_a")]
	a.LogoFileID, a.Verified = &logo, true
	e.orgs.orgs[a.ID] = a
	pending := e.files.upload(orgID("club_b"), files.ScanPending)
	b := e.orgs.orgs[orgID("club_b")]
	b.LogoFileID = &pending
	e.orgs.orgs[b.ID] = b
	for _, id := range []uuid.UUID{a.ID, b.ID} {
		if err := e.svc.FollowOrg(ctx, reader, id, nil); err != nil {
			t.Fatal(err)
		}
		if err := e.svc.MuteOrg(ctx, reader, id); err != nil {
			t.Fatal(err)
		}
	}
	check := func(name string, items []feed.OrgItem) {
		t.Helper()
		if len(items) != 2 {
			t.Fatalf("%s = %d items, want 2", name, len(items))
		}
		for _, it := range items {
			d := feed.ToOrgDTO(it)
			switch it.Org.ID {
			case a.ID:
				if !d.Verified || d.LogoURL != "https://files.example.test/"+logo.String() {
					t.Fatalf("%s club_a = %+v", name, d)
				}
			case b.ID:
				if d.Verified || d.LogoURL != "" {
					t.Fatalf("%s club_b = %+v (unscanned logo, not verified)", name, d)
				}
			}
		}
	}
	before, signed := e.orgs.getMany, e.files.presigns(logo)
	follows, err := e.svc.ListFollows(ctx, reader, feed.PageQuery{})
	if err != nil {
		t.Fatal(err)
	}
	check("ListFollows", follows.Items)
	mutes, err := e.svc.ListMutes(ctx, reader, feed.PageQuery{})
	if err != nil {
		t.Fatal(err)
	}
	check("ListMutes", mutes.Items)
	if n := e.orgs.getMany - before; n != 2 {
		t.Fatalf("GetMany calls for two pages = %d, want 2", n)
	}
	if n := e.files.presigns(logo) - signed; n != 2 {
		t.Fatalf("logo signed %d times on two pages, want 2", n)
	}
	raw, err := json.Marshal(feed.ToOrgDTO(follows.Items[0]))
	if err != nil || !strings.Contains(string(raw), `"verified":`) {
		t.Fatalf("org card JSON = %s, %v", raw, err)
	}
}
