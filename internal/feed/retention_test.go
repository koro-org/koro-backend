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
	if got := e.mod.Sweeper.PolicyCodes(); !slices.Equal(got, []string{feed.PolicyContentReport, feed.PolicyModerationAction,
		feed.PolicyMediaDropped, feed.PolicyPostRemoved}) {
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

// TestPurgeDroppedMedia_WaitsForOpenReports (post_media_dropped, BE-158):
// an image an edit took off a published post goes with its file 12 months
// after the edit — not while a report on the post is open or in review,
// the moderators' evidence — and the post keeps what it shows; a removed
// post's purge takes its dropped images too. Unapproved, the sweep only
// counts.
func TestPurgeDroppedMedia_WaitsForOpenReports(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	admin := persona(t, "club_admin")
	up := func() uuid.UUID { return e.files.upload(orgID("club_a"), files.ScanClean) }
	a, b, c, d := up(), up(), up(), up()
	v := e.published(t, "club_admin", "club_a", feed.PostInput{Kind: feed.KindImage, Media: []feed.MediaInput{{FileID: a}, {FileID: b}}})
	r := e.report(t, "student", v.Post.ID)
	if _, err := e.svc.UpdatePost(ctx, admin, v.Post.ID, v.ETag, feed.PostPatch{Media: mediaList(b)}); err != nil {
		t.Fatal(err)
	}
	gone := e.published(t, "club_admin", "club_a", feed.PostInput{Kind: feed.KindImage, Media: []feed.MediaInput{{FileID: c}, {FileID: d}}})
	if _, err := e.svc.UpdatePost(ctx, admin, gone.Post.ID, gone.ETag, feed.PostPatch{Media: mediaList(d)}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.DeletePost(ctx, admin, gone.Post.ID); err != nil { // published → removed
		t.Fatal(err)
	}
	e.clk.Advance(13 * 30 * 24 * time.Hour)
	now := e.clk.Now()
	year := now.Sub(now.AddDate(0, -12, 0))
	p := privacy.RetentionPolicy{Code: feed.PolicyMediaDropped, Retention: year, LegalApproved: true}

	// The report on v is open: its dropped image waits; gone's is due.
	if res, err := e.mod.Sweeper.Sweep(ctx, privacy.RetentionPolicy{Code: feed.PolicyMediaDropped, Retention: year}, now); err != nil || res.Count != 1 {
		t.Fatalf("dry run = %+v, %v", res, err)
	}
	if len(e.files.deleted) != 0 {
		t.Fatalf("a dry run deleted %v", e.files.deleted)
	}
	if _, err := e.svc.ResolveReport(ctx, persona(t, "moderator"), r.ID, feed.ResolveInput{Status: feed.ReportInReview}); err != nil {
		t.Fatal(err)
	}
	res, err := e.mod.Sweeper.Sweep(ctx, p, now)
	if err != nil || res.Count != 1 || res.Table != "post_dropped_media" || !slices.Equal(e.files.deleted, []uuid.UUID{c}) {
		t.Fatalf("sweep with a report in review = %+v, %v; deleted %v", res, err, e.files.deleted)
	}

	// Dismissed, the report no longer holds the evidence.
	if _, err := e.svc.ResolveReport(ctx, persona(t, "moderator"), r.ID, feed.ResolveInput{Status: feed.ReportDismissed}); err != nil {
		t.Fatal(err)
	}
	if res, err := e.mod.Sweeper.Sweep(ctx, p, now); err != nil || res.Count != 1 || !slices.Equal(e.files.deleted, []uuid.UUID{c, a}) {
		t.Fatalf("sweep = %+v, %v; deleted %v", res, err, e.files.deleted)
	}
	if dropped, _ := e.mem.DroppedMediaOf(ctx, []uuid.UUID{v.Post.ID, gone.Post.ID}); len(dropped) != 0 {
		t.Fatalf("dropped rows left: %+v", dropped)
	}
	if m, _ := e.mem.MediaOf(ctx, []uuid.UUID{v.Post.ID}); len(m[v.Post.ID]) != 1 || m[v.Post.ID][0].FileID != b {
		t.Fatalf("the post lost what it shows: %+v", m[v.Post.ID])
	}
	if res, err := e.mod.Sweeper.Sweep(ctx, p, now); err != nil || res.Count != 0 {
		t.Fatalf("rerun = %+v, %v", res, err)
	}

	// The removed post's purge takes what it showed and what it dropped.
	e.files.deleted = nil
	purged, err := e.mod.Sweeper.Sweep(ctx, privacy.RetentionPolicy{Code: feed.PolicyPostRemoved, Retention: year, LegalApproved: true}, now)
	if err != nil || purged.Count != 1 || !slices.Equal(e.files.deleted, []uuid.UUID{d}) {
		t.Fatalf("post_removed = %+v, %v; deleted %v, want the shown image (the dropped one went first)", purged, err, e.files.deleted)
	}
}

// TestPurgeRemovedPosts_TakesDroppedMedia (BE-158): a removed post purged
// before its dropped images are due takes them along.
func TestPurgeRemovedPosts_TakesDroppedMedia(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	admin := persona(t, "club_admin")
	up := func() uuid.UUID { return e.files.upload(orgID("club_a"), files.ScanClean) }
	a, b := up(), up()
	v := e.published(t, "club_admin", "club_a", feed.PostInput{Kind: feed.KindImage, Media: []feed.MediaInput{{FileID: a}, {FileID: b}}})
	if err := e.svc.DeletePost(ctx, admin, v.Post.ID); err != nil { // published → removed
		t.Fatal(err)
	}
	// A dropped row of the removed post (an edit just before the removal).
	if err := e.mem.ReplaceMedia(ctx, orgID("club_a"), v.Post.ID, []feed.Media{{ID: uuid.NewV7(), FileID: b}}); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(400 * 24 * time.Hour)
	if err := e.mem.DropMedia(ctx, orgID("club_a"), v.Post.ID, []feed.DroppedMedia{{FileID: a, DroppedAt: e.clk.Now()}}); err != nil {
		t.Fatal(err)
	}
	now := e.clk.Now()
	p := privacy.RetentionPolicy{Code: feed.PolicyPostRemoved, Retention: now.Sub(now.AddDate(0, -12, 0)), LegalApproved: true}
	if res, err := e.mod.Sweeper.Sweep(ctx, p, now); err != nil || res.Count != 1 {
		t.Fatalf("sweep = %+v, %v", res, err)
	}
	if !slices.Equal(e.files.deleted, []uuid.UUID{b, a}) {
		t.Fatalf("deleted files = %v, want the shown then the dropped image", e.files.deleted)
	}
	if dropped, _ := e.mem.DroppedMediaOf(ctx, []uuid.UUID{v.Post.ID}); len(dropped) != 0 {
		t.Fatalf("dropped rows of the purged post: %+v", dropped)
	}
}
