package feed_test

import (
	"slices"
	"testing"
	"uuid"

	"koro/internal/feed"
	"koro/internal/files"
)

// TestFeed_ReplacedMedia_DeletesFiles (BE-155, T-247): updatePost deletes
// the images its new media list no longer names, in its transaction after
// the media rows, and keeps the ones it still shows (reordered too); a
// patch without media deletes nothing. A deleted draft takes its image
// files with it; a removed (published) post keeps them until the purge
// (BE-057). The public page follows post.changed (edited; BE-149).
func TestFeed_ReplacedMedia_DeletesFiles(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	admin := persona(t, "club_admin")
	org := orgID("club_a")
	up := func() uuid.UUID { return e.files.upload(org, files.ScanClean) }
	a, b, c := up(), up(), up()
	v := e.published(t, "club_admin", "club_a", feed.PostInput{Kind: feed.KindImage, Title: "Afiş",
		Media: []feed.MediaInput{{FileID: a}, {FileID: b}}})
	etag := v.ETag
	update := func(p feed.PostPatch) {
		t.Helper()
		got, err := e.svc.UpdatePost(ctx, admin, v.Post.ID, etag, p)
		if err != nil {
			t.Fatal(err)
		}
		etag = got.ETag
	}
	media := func(ids ...uuid.UUID) *[]feed.MediaInput {
		m := make([]feed.MediaInput, len(ids))
		for i, id := range ids {
			m[i] = feed.MediaInput{FileID: id}
		}
		return &m
	}
	title := "Yeni afiş"
	update(feed.PostPatch{Title: &title})
	update(feed.PostPatch{Media: media(b, a)}) // reordered only
	if len(e.files.deleted) != 0 {
		t.Fatalf("deleted without a dropped image: %v", e.files.deleted)
	}
	changes := len(e.changes(t, v.Post.ID))
	update(feed.PostPatch{Media: media(c, b)})
	if !slices.Contains(e.files.deleted, a) || slices.Contains(e.files.deleted, b) || slices.Contains(e.files.deleted, c) {
		t.Fatalf("replaced image: deleted %v", e.files.deleted)
	}
	if got := len(e.changes(t, v.Post.ID)); got != changes+1 {
		t.Fatalf("post.changed after the replacement: %d → %d", changes, got)
	}
	if m, _ := e.mem.MediaOf(ctx, []uuid.UUID{v.Post.ID}); len(m[v.Post.ID]) != 2 || m[v.Post.ID][0].FileID != c {
		t.Fatalf("media = %+v", m[v.Post.ID])
	}

	// A removed published post keeps its images (purged later, BE-057).
	if err := e.svc.DeletePost(ctx, admin, v.Post.ID); err != nil {
		t.Fatal(err)
	}
	if len(e.files.deleted) != 1 {
		t.Fatalf("a removed post lost its images at once: %v", e.files.deleted)
	}

	// A deleted draft takes its images with it.
	d1, d2 := up(), up()
	d := e.create(t, "club_admin", "club_a", feed.PostInput{Kind: feed.KindImage, Media: []feed.MediaInput{{FileID: d1}, {FileID: d2}}})
	if err := e.svc.DeletePost(ctx, admin, d.Post.ID); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(e.files.deleted, d1) || !slices.Contains(e.files.deleted, d2) || len(e.files.deleted) != 3 {
		t.Fatalf("deleted draft: deleted %v", e.files.deleted)
	}
}
