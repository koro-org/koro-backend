package feed_test

import (
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/feed"
	"koro/internal/files"
	"koro/internal/platform/privacy"
)

// TestPurgeRemovedPosts_CascadesMedia (post_removed, 12 months; T-152,
// T-247; BE-057): a removed post goes a year after its removal with its
// media rows, its likes and its image files; a published post and a post
// removed recently stay. Unapproved, the sweep only counts.
func TestPurgeRemovedPosts_CascadesMedia(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	admin := persona(t, "club_admin")
	in := textInput("görselli duyuru")
	img := e.files.upload(orgID("club_a"), files.ScanClean)
	in.Kind, in.Media = feed.KindImage, []feed.MediaInput{{FileID: img}}
	old := e.published(t, "club_admin", "club_a", in)
	if err := e.svc.LikePost(ctx, persona(t, "student"), old.Post.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.DeletePost(ctx, admin, old.Post.ID); err != nil { // published → removed
		t.Fatal(err)
	}
	kept := e.published(t, "club_admin", "club_a", textInput("yayında kalan"))
	e.clk.Advance(366 * 24 * time.Hour)
	recent := e.published(t, "club_admin", "club_a", textInput("yeni kaldırılan"))
	if err := e.svc.DeletePost(ctx, admin, recent.Post.ID); err != nil {
		t.Fatal(err)
	}

	now := e.clk.Now()
	p := privacy.RetentionPolicy{Code: feed.PolicyPostRemoved, Retention: now.Sub(now.AddDate(0, -12, 0))}
	if got := e.mod.Sweeper.PolicyCodes(); !slices.Equal(got, []string{feed.PolicyContentReport, feed.PolicyModerationAction, feed.PolicyPostRemoved}) {
		t.Fatalf("PolicyCodes() = %v", got)
	}
	if res, err := e.mod.Sweeper.Sweep(ctx, p, now); err != nil || res.Count != 1 {
		t.Fatalf("dry run = %+v, %v", res, err)
	}
	if _, err := e.mem.GetPost(ctx, old.Post.ID); err != nil || len(e.files.deleted) != 0 {
		t.Fatalf("a dry run deleted: %v, files %v", err, e.files.deleted)
	}
	p.LegalApproved = true
	res, err := e.mod.Sweeper.Sweep(ctx, p, now)
	if err != nil || res.Count != 1 || res.Table != "posts" {
		t.Fatalf("sweep = %+v, %v", res, err)
	}
	if _, err := e.mem.GetPost(ctx, old.Post.ID); err == nil {
		t.Fatal("the removed post survived its year")
	}
	if media, _ := e.mem.MediaOf(ctx, []uuid.UUID{old.Post.ID}); len(media[old.Post.ID]) != 0 {
		t.Fatalf("media rows of the purged post: %+v", media)
	}
	if !slices.Equal(e.files.deleted, []uuid.UUID{img}) {
		t.Fatalf("deleted files = %v, want the post's image", e.files.deleted)
	}
	if in, _ := e.mem.InteractionsOf(ctx, persona(t, "student").UserID); len(in.Likes) != 0 {
		t.Fatalf("likes of the purged post: %+v", in.Likes)
	}
	for _, id := range []uuid.UUID{kept.Post.ID, recent.Post.ID} {
		if _, err := e.mem.GetPost(ctx, id); err != nil {
			t.Fatalf("post %s was purged: %v", id, err)
		}
	}
	if res, err := e.mod.Sweeper.Sweep(ctx, p, now); err != nil || res.Count != 0 {
		t.Fatalf("rerun = %+v, %v", res, err)
	}
}
