package feed_test

import (
	"testing"
	"time"
	"uuid"

	"koro/internal/feed"
)

// The reader's hidden posts (BE-145, Q-35): listMyHiddenPosts and
// unhidePost.

// TestUnhidePost_BackInFeed_Idempotent: unhidePost brings the post back to
// the reader's feed; twice, or without a hide, is still 204; another
// reader's hide stays; the KVKK export no longer holds the row.
func TestUnhidePost_BackInFeed_Idempotent(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	v := e.published(t, "club_admin", "club_a", textInput("geri gelecek"))
	reader, other := persona(t, "student"), persona(t, "club_member")
	for _, a := range []string{"student", "club_member"} {
		if err := e.svc.HidePost(ctx, persona(t, a), v.Post.ID); err != nil {
			t.Fatal(err)
		}
	}
	if contains(e.feedIDs(t, reader, feed.FeedQuery{}), v.Post.ID) {
		t.Fatal("the hidden post is in the feed")
	}
	for range 2 {
		if err := e.svc.UnhidePost(ctx, reader, v.Post.ID); err != nil {
			t.Fatalf("UnhidePost = %v", err)
		}
	}
	if !contains(e.feedIDs(t, reader, feed.FeedQuery{}), v.Post.ID) {
		t.Fatal("the unhidden post is not back in the feed")
	}
	if contains(e.feedIDs(t, other, feed.FeedQuery{}), v.Post.ID) {
		t.Fatal("unhide undid another reader's hide")
	}
	// Never hidden: 204 on a visible post, 404 on a missing one.
	if err := e.svc.UnhidePost(ctx, persona(t, "student_minor"), v.Post.ID); err != nil {
		t.Fatalf("UnhidePost(no hide) = %v", err)
	}
	wantNotFound(t, e.svc.UnhidePost(ctx, reader, uuid.NewV7()))
	sec, err := e.mod.Privacy.Export(ctx, reader.UserID)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range sec.Items {
		if it["type"] == "post_hide" {
			t.Fatalf("the export still holds the hide: %v", it)
		}
	}
	if p, _ := e.svc.ListHiddenPosts(ctx, reader, feed.PageQuery{}); len(p.Items) != 0 {
		t.Fatalf("hidden after unhide = %d", len(p.Items))
	}
}

// TestUnhidePost_InvisiblePost_OwnRowOnly: on a post the reader may no
// longer see (removed) the reader's own hide still goes; without one it is
// 404 like a missing post, so nobody learns that a post they never hid
// exists.
func TestUnhidePost_InvisiblePost_OwnRowOnly(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	v := e.published(t, "club_admin", "club_a", textInput("kaldırılacak"))
	reader := persona(t, "student")
	if err := e.svc.HidePost(ctx, reader, v.Post.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.DeletePost(ctx, persona(t, "club_admin"), v.Post.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.UnhidePost(ctx, reader, v.Post.ID); err != nil {
		t.Fatalf("UnhidePost(removed, hidden) = %v", err)
	}
	wantNotFound(t, e.svc.UnhidePost(ctx, reader, v.Post.ID))
	wantNotFound(t, e.svc.UnhidePost(ctx, persona(t, "club_member"), v.Post.ID))
	if in, _ := e.mem.InteractionsOf(ctx, reader.UserID); len(in.Hides) != 0 {
		t.Fatalf("hides = %d", len(in.Hides))
	}
}

// TestListMyHiddenPosts_OwnOnly_SkipsInvisible: the hidden list holds the
// reader's own hides of posts they may still see, newest hide first, page
// by page with full pages; a removed post and a followers post after an
// unfollow are skipped (their rows stay), a muted org's post is listed;
// another reader sees none of it; a moderator's list keeps every
// published post (03 §3.6).
func TestListMyHiddenPosts_OwnOnly_SkipsInvisible(t *testing.T) {
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
	removed := e.published(t, "club_admin", "club_a", textInput("kaldırılacak"))
	muted := e.published(t, "company_admin", "company_a", textInput("sessiz"))
	var visible []uuid.UUID
	for i := range 3 {
		v := e.published(t, "club_admin", "club_a", textInput("gizli "+string(rune('a'+i))))
		visible = append(visible, v.Post.ID)
	}
	for _, id := range []uuid.UUID{followers.Post.ID, removed.Post.ID, muted.Post.ID, visible[0], visible[1], visible[2]} {
		if err := e.svc.HidePost(ctx, reader, id); err != nil {
			t.Fatal(err)
		}
		e.clk.Advance(time.Millisecond)
	}
	if err := e.svc.HidePost(ctx, persona(t, "club_member"), visible[0]); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.DeletePost(ctx, persona(t, "club_admin"), removed.Post.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.UnfollowOrg(ctx, reader, orgID("club_b")); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.MuteOrg(ctx, reader, orgID("company_a")); err != nil {
		t.Fatal(err)
	}
	var got []uuid.UUID
	q := feed.PageQuery{Limit: 2}
	for range 10 {
		p, err := e.svc.ListHiddenPosts(ctx, reader, q)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range p.Items {
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
	want := []uuid.UUID{visible[2], visible[1], visible[0], muted.Post.ID}
	if len(got) != len(want) {
		t.Fatalf("hidden = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("hidden = %v, want %v", got, want)
		}
	}
	// The skipped rows stay (the reader's data until unhide, the post's
	// deletion or the KVKK erase).
	if in, _ := e.mem.InteractionsOf(ctx, reader.UserID); len(in.Hides) != 6 {
		t.Fatalf("hide rows = %d", len(in.Hides))
	}
	if other, _ := e.svc.ListHiddenPosts(ctx, persona(t, "student_minor"), feed.PageQuery{}); len(other.Items) != 0 {
		t.Fatal("another reader sees the hides")
	}
	mod := persona(t, "moderator")
	if err := e.svc.HidePost(ctx, mod, followers.Post.ID); err != nil {
		t.Fatal(err)
	}
	if p, _ := e.svc.ListHiddenPosts(ctx, mod, feed.PageQuery{}); len(p.Items) != 1 {
		t.Fatalf("moderator hidden = %d", len(p.Items))
	}
	// The lists are apart: a hide saves nothing.
	if p, _ := e.svc.ListSavedPosts(ctx, reader, feed.PageQuery{}); len(p.Items) != 0 {
		t.Fatalf("saved = %d", len(p.Items))
	}
}

// TestListMyHiddenPosts_Anonymous_Refused: the list is the caller's own
// (feed.hide.list_own, O): an anonymous caller is refused before any read.
func TestListMyHiddenPosts_Anonymous_Refused(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	_, err := e.svc.ListHiddenPosts(t.Context(), persona(t, "anonymous"), feed.PageQuery{})
	wantForbidden(t, err)
}
