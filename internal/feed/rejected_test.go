package feed_test

import (
	"slices"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/feed"
	"koro/internal/files"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
)

// rejectFile delivers file.rejected of file (attached to module's entity)
// to feed's subscription, as the worker would.
func (e *env) rejectFile(t *testing.T, file uuid.UUID, purpose, module string, entity uuid.UUID) {
	t.Helper()
	r := outbox.NewRegistry()
	e.mod.Subscribe(r)
	env, err := domainevent.New(domainevent.FileRejected{FileID: file, Purpose: purpose, Reason: "infected",
		AttachedModule: module, AttachedEntityID: &entity}, "file", file)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range r.For(domainevent.TopicFileRejected) {
		if s.Name == feed.DropRejectedMediaTaskName {
			if err := s.Handle(t.Context(), env); err != nil {
				t.Fatalf("%s: %v", s.Name, err)
			}
			return
		}
	}
	t.Fatalf("file.rejected has no %s subscription", feed.DropRejectedMediaTaskName)
}

// TestFeed_FileRejected_DropsReference (T-176): file.rejected for a post
// image attached to feed drops its post_media row (the other images keep
// their order and alt text) and deletes the file in one transaction; the
// post's ETag moves but edited_at stays, and no post.changed is written
// (webcache's own file.rejected subscription revalidates the public page,
// BE-150). A second delivery changes nothing more; another module's file
// and a file the post no longer shows are not feed's reference. A draft
// that loses its only image asks for a new one before publication.
func TestFeed_FileRejected_DropsReference(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	org := orgID("club_a")
	bad, good := e.files.upload(org, files.ScanClean), e.files.upload(org, files.ScanClean)
	p := e.published(t, "club_admin", "club_a", feed.PostInput{Kind: feed.KindImage, Title: "Afiş",
		Media: []feed.MediaInput{{FileID: bad, AltText: "eski afiş"}, {FileID: good, AltText: "salon"}}})
	before, err := e.mem.GetPost(ctx, p.Post.ID)
	if err != nil {
		t.Fatal(err)
	}
	changes := len(e.changes(t, p.Post.ID))

	for range 2 { // at-least-once delivery
		e.rejectFile(t, bad, string(files.PurposePostMedia), "feed", p.Post.ID)
	}
	media, err := e.mem.MediaOf(ctx, []uuid.UUID{p.Post.ID})
	if err != nil {
		t.Fatal(err)
	}
	if m := media[p.Post.ID]; len(m) != 1 || m[0].FileID != good || m[0].Position != 0 || *m[0].AltText != "salon" {
		t.Fatalf("media after the rejection = %+v", m)
	}
	if !slices.Contains(e.files.deleted, bad) {
		t.Fatal("the rejected image was not deleted")
	}
	after, err := e.mem.GetPost(ctx, p.Post.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.UpdatedAt.Equal(before.UpdatedAt) || after.EditedAt != nil || after.Status != feed.StatusPublished {
		t.Fatalf("post after the rejection: updated %v → %v, edited %v, status %s",
			before.UpdatedAt, after.UpdatedAt, after.EditedAt, after.Status)
	}
	if got := len(e.changes(t, p.Post.ID)); got != changes {
		t.Fatalf("post.changed written by the rejection: %d → %d (webcache revalidates on file.rejected)", changes, got)
	}
	au := e.audit.Of(authz.Action("feed.post.drop_rejected_media"))
	if len(au) != 1 || au[0].SystemJob != feed.DropRejectedMediaTaskName || au[0].OrgID == nil || *au[0].OrgID != org ||
		au[0].Metadata["file_id"] != bad.String() || au[0].Metadata["reason"] != "infected" || au[0].Metadata["media_left"] != 1 {
		t.Fatalf("system audit = %+v", au)
	}
	v, err := e.svc.GetPost(ctx, authz.Anonymous(), p.Post.ID)
	if err != nil || len(v.Media) != 1 || v.Media[0].FileID != good || v.Media[0].URL == "" {
		t.Fatalf("public post = %+v, %v", v.Media, err)
	}

	// Not feed's reference: another module's file, an image the post no
	// longer shows (only the file goes).
	e.rejectFile(t, good, string(files.PurposePostMedia), "events", p.Post.ID)
	stray := e.files.upload(org, files.ScanClean)
	e.rejectFile(t, stray, string(files.PurposePostMedia), "feed", p.Post.ID)
	if media, _ := e.mem.MediaOf(ctx, []uuid.UUID{p.Post.ID}); len(media[p.Post.ID]) != 1 ||
		slices.Contains(e.files.deleted, good) || !slices.Contains(e.files.deleted, stray) {
		t.Fatalf("media %+v, deleted %v", media[p.Post.ID], e.files.deleted)
	}
	if len(e.audit.Of(authz.Action("feed.post.drop_rejected_media"))) != 1 {
		t.Fatal("a rejection that dropped nothing was audited")
	}

	// A draft whose only image is rejected cannot be published until a
	// new image is added (the uploader got files' notice).
	img := e.files.upload(org, files.ScanPending)
	d := e.create(t, "club_admin", "club_a", feed.PostInput{Kind: feed.KindImage, Media: []feed.MediaInput{{FileID: img}}})
	e.rejectFile(t, img, string(files.PurposePostMedia), "feed", d.Post.ID)
	if media, _ := e.mem.MediaOf(ctx, []uuid.UUID{d.Post.ID}); len(media[d.Post.ID]) != 0 {
		t.Fatalf("draft media = %+v", media[d.Post.ID])
	}
	_, err = e.svc.PublishPost(ctx, persona(t, "club_admin"), d.Post.ID)
	wantField(t, err, "body.media")
}
