//go:build integration

package feed_test

import (
	"errors"
	"testing"
	"uuid"

	"koro/internal/audit/audittest"
	"koro/internal/feed"
	"koro/internal/files"
	"koro/internal/platform/clock"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
	"koro/internal/platform/storage"
	"koro/internal/platform/testutil"
)

// TestFeed_FileRejected_Gorm (T-176) runs feed.drop_rejected_media over
// PostgreSQL with the real files service: the post_media row goes before
// the files row in one transaction (post_media_file_id_fkey would refuse
// the delete otherwise), the other image keeps its place and the
// posts_touch_updated_at trigger moves the ETag.
func TestFeed_FileRejected_Gorm(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	w := pgWorld(d)
	ctx := t.Context()
	clk := clock.NewFake(t0)
	idem := idempotency.New(idempotency.Deps{Tx: d.Tx, Store: idempotency.NewMemStore(), Clock: clk, Log: obs.Discard()})
	fm, err := files.New(files.Deps{
		Store: files.NewGormStore(d.ORM), Objects: storage.NewMem(clk), Tx: d.Tx, Outbox: outbox.NewMem(clk),
		Queue: queue.NewMem(), Idempotency: idem, Clock: clk, Log: obs.Discard(),
		Buckets: files.Buckets{Files: "koro-files-dev", Quarantine: "quarantine"}, Audit: &audittest.Recorder{},
	})
	if err != nil {
		t.Fatal(err)
	}
	store := feed.NewGormStore(d.ORM)
	mod, err := feed.New(feed.Deps{
		Store: store, Tx: d.Tx, Outbox: outbox.NewMem(clk), Queue: queue.NewMem(), Audit: &audittest.Recorder{},
		Orgs: newFakeOrgs(), Idempotency: idem, Clock: clk, Log: obs.Discard(),
		Viewers: &fakeViewers{keys: map[uuid.UUID][]string{}}, Files: fm.Service,
	})
	if err != nil {
		t.Fatal(err)
	}
	org, by := w.org(t, feed.AuthorClub), w.user(t)
	bad, good := w.file(t, org), w.file(t, org)
	p, err := store.InsertPost(ctx, publishedAt(post(org, feed.AuthorClub, by), pub0))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceMedia(ctx, org, p.ID, []feed.Media{{ID: uuid.NewV7(), FileID: bad}, {ID: uuid.NewV7(), FileID: good}}); err != nil {
		t.Fatal(err)
	}
	r := outbox.NewRegistry()
	mod.Subscribe(r)
	env, err := domainevent.New(domainevent.FileRejected{FileID: bad, Purpose: string(files.PurposePostMedia), Reason: "infected",
		AttachedModule: "feed", AttachedEntityID: &p.ID}, "file", bad)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range r.For(domainevent.TopicFileRejected) {
		if s.Name != feed.DropRejectedMediaTaskName {
			continue
		}
		for range 2 { // at-least-once delivery
			if err := s.Handle(ctx, env); err != nil {
				t.Fatal(err)
			}
		}
	}
	media, err := store.MediaOf(ctx, []uuid.UUID{p.ID})
	if err != nil {
		t.Fatal(err)
	}
	if m := media[p.ID]; len(m) != 1 || m[0].FileID != good || m[0].Position != 0 {
		t.Fatalf("media after the rejection = %+v", m)
	}
	if _, err := fm.Service.Get(ctx, bad); !errors.Is(err, files.ErrNotFound) {
		t.Fatalf("rejected files row kept: %v", err)
	}
	if _, err := fm.Service.Get(ctx, good); err != nil {
		t.Fatalf("the other image's file: %v", err)
	}
	after, err := store.GetPost(ctx, p.ID)
	if err != nil || !after.UpdatedAt.After(p.UpdatedAt) || after.EditedAt != nil {
		t.Fatalf("post after the rejection = %+v, %v", after, err)
	}
}
