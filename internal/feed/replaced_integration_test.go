//go:build integration

package feed_test

import (
	"errors"
	"testing"
	"uuid"

	"koro/internal/audit/audittest"
	"koro/internal/authz"
	"koro/internal/feed"
	"koro/internal/files"
	"koro/internal/orgs"
	"koro/internal/platform/clock"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
	"koro/internal/platform/storage"
	"koro/internal/platform/testutil"
)

// TestFeed_ReplacedMedia_Gorm (BE-155, T-247) runs updatePost and
// deletePost over PostgreSQL with the real files service: a dropped
// image's files row goes in the write's transaction after its post_media
// row (post_media_file_id_fkey would refuse it otherwise), the kept image
// stays attached, and a deleted draft's images go after the cascade.
func TestFeed_ReplacedMedia_Gorm(t *testing.T) {
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
	org := w.org(t, feed.AuthorClub)
	dir := newFakeOrgs()
	dir.orgs[org] = orgs.OrgSummary{ID: org, Kind: orgs.KindClub, Name: "Kulüp", Slug: "kulup", Status: orgs.StatusActive}
	admin := authz.Actor{Kind: authz.KindUser, UserID: w.user(t),
		Memberships: []authz.Membership{{OrgID: org, OrgKind: authz.OrgClub, Role: authz.MemberClubAdmin}}}
	mod, err := feed.New(feed.Deps{
		Store: feed.NewGormStore(d.ORM), Tx: d.Tx, Outbox: outbox.NewMem(clk), Queue: queue.NewMem(),
		Audit: &audittest.Recorder{}, Orgs: dir, Idempotency: idem, Clock: clk, Log: obs.Discard(),
		Viewers: &fakeViewers{keys: map[uuid.UUID][]string{}}, Files: fm.Service,
	})
	if err != nil {
		t.Fatal(err)
	}
	gone := func(f uuid.UUID) bool {
		t.Helper()
		_, err := fm.Service.Get(ctx, f)
		return errors.Is(err, files.ErrNotFound)
	}
	a, b := w.file(t, org), w.file(t, org)
	v, err := mod.Service.CreatePost(ctx, admin, org, feed.PostInput{Kind: feed.KindImage, Title: "Afiş",
		Media: []feed.MediaInput{{FileID: a}, {FileID: b}}})
	if err != nil {
		t.Fatal(err)
	}
	kept := []feed.MediaInput{{FileID: b}}
	if _, err := mod.Service.UpdatePost(ctx, admin, v.Post.ID, v.ETag, feed.PostPatch{Media: &kept}); err != nil {
		t.Fatal(err)
	}
	if !gone(a) || gone(b) {
		t.Fatalf("after the replacement: dropped gone %v, kept gone %v", gone(a), gone(b))
	}
	if m, err := fm.Service.Get(ctx, b); err != nil || m.AttachedModule != "feed" || m.AttachedEntityID != v.Post.ID {
		t.Fatalf("kept image = %+v, %v", m, err)
	}
	if err := mod.Service.DeletePost(ctx, admin, v.Post.ID); err != nil {
		t.Fatal(err)
	}
	if !gone(b) {
		t.Fatal("the deleted draft's image was kept")
	}
}
