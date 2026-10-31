package feed_test

import (
	"errors"
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/feed"
	"koro/internal/files"
)

// mediaList is the media patch of the files ids, in order.
func mediaList(ids ...uuid.UUID) *[]feed.MediaInput {
	m := make([]feed.MediaInput, len(ids))
	for i, id := range ids {
		m[i] = feed.MediaInput{FileID: id}
	}
	return &m
}

// TestFeed_ReplacedMedia_DeletesFiles (BE-155, T-247; BE-158): updatePost
// on a draft deletes the images its new media list no longer names, in its
// transaction after the media rows, and keeps the ones it still shows
// (reordered too); a patch without media deletes nothing. A deleted draft
// takes its image files with it. A published post keeps what an edit drops
// (TestFeed_PublishedEdit_KeepsDroppedMedia) and a removed one keeps its
// images until the purge (BE-057).
func TestFeed_ReplacedMedia_DeletesFiles(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	admin := persona(t, "club_admin")
	org := orgID("club_a")
	up := func() uuid.UUID { return e.files.upload(org, files.ScanClean) }
	a, b, c := up(), up(), up()
	v := e.create(t, "club_admin", "club_a", feed.PostInput{Kind: feed.KindImage, Title: "Afiş",
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
	title := "Yeni afiş"
	update(feed.PostPatch{Title: &title})
	update(feed.PostPatch{Media: mediaList(b, a)}) // reordered only
	if len(e.files.deleted) != 0 {
		t.Fatalf("deleted without a dropped image: %v", e.files.deleted)
	}
	update(feed.PostPatch{Media: mediaList(c, b)})
	if !slices.Equal(e.files.deleted, []uuid.UUID{a}) {
		t.Fatalf("replaced image of a draft: deleted %v, want %v", e.files.deleted, a)
	}
	if m, _ := e.mem.MediaOf(ctx, []uuid.UUID{v.Post.ID}); len(m[v.Post.ID]) != 2 || m[v.Post.ID][0].FileID != c {
		t.Fatalf("media = %+v", m[v.Post.ID])
	}
	if d, _ := e.mem.DroppedMediaOf(ctx, []uuid.UUID{v.Post.ID}); len(d[v.Post.ID]) != 0 {
		t.Fatalf("a draft kept a dropped image: %+v", d[v.Post.ID])
	}
	if meta := e.audit.Last().Metadata; meta["dropped_kept"] != false || !slices.Equal(stringsOf(meta["dropped_file_ids"]), []string{a.String()}) {
		t.Fatalf("audit metadata = %v", meta)
	}

	// A deleted draft takes its images with it.
	if err := e.svc.DeletePost(ctx, admin, v.Post.ID); err != nil {
		t.Fatal(err)
	}
	if len(e.files.deleted) != 3 || !slices.Contains(e.files.deleted, b) || !slices.Contains(e.files.deleted, c) {
		t.Fatalf("deleted draft: deleted %v", e.files.deleted)
	}
}

// TestFeed_PublishedEdit_KeepsDroppedMedia (BE-158, the security review's
// G-1): an edit of a published, reported post that drops its images
// deletes no file. The images leave the post at once (readers, the author
// org and post.changed see only the new list) and stay attached to it in
// post_dropped_media with their alt text and the edit's time; the
// moderator's report view shows them next to the current ones, with URLs,
// and the edit's audit record names them. An image listed again comes back
// from there; a dropped image the scan rejects later goes with its row. A
// removed post still keeps its images until the purge.
func TestFeed_PublishedEdit_KeepsDroppedMedia(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	admin := persona(t, "club_admin")
	org := orgID("club_a")
	up := func() uuid.UUID { return e.files.upload(org, files.ScanClean) }
	a, b, c := up(), up(), up()
	alt := "Uygunsuz afiş"
	v := e.published(t, "club_admin", "club_a", feed.PostInput{Kind: feed.KindImage, Title: "Afiş",
		Media: []feed.MediaInput{{FileID: a, AltText: alt}, {FileID: b}}})
	r := e.report(t, "student", v.Post.ID)
	changes := len(e.changes(t, v.Post.ID))

	edited := e.clk.Now()
	got, err := e.svc.UpdatePost(ctx, admin, v.Post.ID, v.ETag, feed.PostPatch{Media: mediaList(c)})
	if err != nil {
		t.Fatal(err)
	}
	if len(e.files.deleted) != 0 {
		t.Fatalf("an edit of a published post deleted %v", e.files.deleted)
	}
	if len(got.Media) != 1 || got.Media[0].FileID != c {
		t.Fatalf("author view media = %+v", got.Media)
	}
	if n := len(e.changes(t, v.Post.ID)); n != changes+1 {
		t.Fatalf("post.changed after the edit: %d → %d", changes, n)
	}
	read, err := e.svc.GetPost(ctx, persona(t, "student"), v.Post.ID)
	if err != nil || len(read.Media) != 1 || read.Media[0].FileID != c {
		t.Fatalf("reader view = %+v, %v", read.Media, err)
	}
	dropped, _ := e.mem.DroppedMediaOf(ctx, []uuid.UUID{v.Post.ID})
	if d := dropped[v.Post.ID]; len(d) != 2 || !slices.Equal([]uuid.UUID{d[0].FileID, d[1].FileID}, sortedIDs(a, b)) ||
		!d[0].DroppedAt.Equal(edited) {
		t.Fatalf("dropped = %+v", d)
	}
	for _, d := range dropped[v.Post.ID] {
		if d.FileID == a && (d.AltText == nil || *d.AltText != alt) {
			t.Fatalf("the dropped image lost its alt text: %+v", d)
		}
	}
	meta := e.audit.Last().Metadata
	if meta["dropped_kept"] != true || !slices.Equal(stringsOf(meta["dropped_file_ids"]), []string{a.String(), b.String()}) {
		t.Fatalf("audit metadata = %v", meta)
	}

	// The moderator sees what was reported.
	view, err := e.svc.GetReport(ctx, persona(t, "moderator"), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	tg := view.Target
	if tg == nil || len(tg.Media) != 1 || tg.Media[0].FileID != c || tg.Media[0].URL == "" || len(tg.DroppedMedia) != 2 {
		t.Fatalf("report target = %+v", tg)
	}
	for _, d := range tg.DroppedMedia {
		if d.URL == "" || !d.DroppedAt.Equal(edited) {
			t.Fatalf("dropped image of the report view = %+v", d)
		}
	}
	dto := feed.ToReportDTO(view)
	if p := dto.Target.Post; p == nil || len(p.Media) != 1 || len(p.DroppedMedia) != 2 || p.DroppedMedia[0].URL == "" {
		t.Fatalf("report DTO post = %+v", p)
	}

	// Listed again, a comes back from the dropped images.
	e.clk.Advance(time.Minute)
	if got, err = e.svc.UpdatePost(ctx, admin, v.Post.ID, got.ETag, feed.PostPatch{Media: mediaList(c, a)}); err != nil {
		t.Fatal(err)
	}
	if d, _ := e.mem.DroppedMediaOf(ctx, []uuid.UUID{v.Post.ID}); len(d[v.Post.ID]) != 1 || d[v.Post.ID][0].FileID != b {
		t.Fatalf("dropped after a was listed again = %+v", d[v.Post.ID])
	}

	// The scan rejects the dropped b later: its row and its file go.
	e.rejectFile(t, b, string(files.PurposePostMedia), "feed", v.Post.ID)
	if !slices.Equal(e.files.deleted, []uuid.UUID{b}) {
		t.Fatalf("rejected dropped image: deleted %v", e.files.deleted)
	}
	if d, _ := e.mem.DroppedMediaOf(ctx, []uuid.UUID{v.Post.ID}); len(d[v.Post.ID]) != 0 {
		t.Fatalf("the rejected image kept its dropped row: %+v", d[v.Post.ID])
	}

	// A removed published post keeps its images (purged later, BE-057).
	if _, err := e.svc.UpdatePost(ctx, admin, v.Post.ID, got.ETag, feed.PostPatch{Media: mediaList(c)}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.DeletePost(ctx, admin, v.Post.ID); err != nil {
		t.Fatal(err)
	}
	if len(e.files.deleted) != 1 {
		t.Fatalf("a removed post lost its images at once: %v", e.files.deleted)
	}
	if d, _ := e.mem.DroppedMediaOf(ctx, []uuid.UUID{v.Post.ID}); len(d[v.Post.ID]) != 1 || d[v.Post.ID][0].FileID != a {
		t.Fatalf("dropped of the removed post = %+v", d[v.Post.ID])
	}
}

// TestFeed_DropMedia_OrgScoped (BE-158, 06 H-38): the dropped rows are
// written only through a posts row of the given org.
func TestFeed_DropMedia_OrgScoped(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	v := e.published(t, "club_admin", "club_a", textInput("duyuru"))
	err := e.mem.DropMedia(t.Context(), orgID("club_b"), v.Post.ID, []feed.DroppedMedia{{FileID: uuid.NewV7(), DroppedAt: e.clk.Now()}})
	if !errors.Is(err, feed.ErrNotFound) {
		t.Fatalf("DropMedia through another org's post = %v, want ErrNotFound", err)
	}
}

// sortedIDs returns ids in the dropped rows' order (dropped_at ties: file
// id).
func sortedIDs(ids ...uuid.UUID) []uuid.UUID {
	out := slices.Clone(ids)
	slices.SortFunc(out, func(x, y uuid.UUID) int { return x.Compare(y) })
	return out
}

// stringsOf is v as a string list (nil when it is not one).
func stringsOf(v any) []string {
	s, _ := v.([]string)
	return s
}
