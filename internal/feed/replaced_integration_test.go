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
	"koro/internal/platform/privacy"
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

// TestFeed_PublishedEdit_KeepsDroppedMedia_Gorm (BE-158, G-1) runs an edit
// of a published post over PostgreSQL with the real files service: the
// dropped image keeps its files row, attached to the post, behind a
// post_dropped_media row (post_dropped_media_file_id_fkey); listed again it
// comes back; post_media_dropped deletes the row and then the file once
// due, and a removed post's purge deletes its dropped images too.
func TestFeed_PublishedEdit_KeepsDroppedMedia_Gorm(t *testing.T) {
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
	w.trust(t, org, feed.TrustTrusted, 0)
	dir := newFakeOrgs()
	dir.orgs[org] = orgs.OrgSummary{ID: org, Kind: orgs.KindClub, Name: "Kulüp", Slug: "kulup", Status: orgs.StatusActive}
	admin := authz.Actor{Kind: authz.KindUser, UserID: w.user(t),
		Memberships: []authz.Membership{{OrgID: org, OrgKind: authz.OrgClub, Role: authz.MemberClubAdmin}}}
	store := feed.NewGormStore(d.ORM)
	mod, err := feed.New(feed.Deps{
		Store: store, Tx: d.Tx, Outbox: outbox.NewMem(clk), Queue: queue.NewMem(),
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
	a, b, c := w.file(t, org), w.file(t, org), w.file(t, org)
	v, err := mod.Service.CreatePost(ctx, admin, org, feed.PostInput{Kind: feed.KindImage, Title: "Afiş",
		Media: []feed.MediaInput{{FileID: a}, {FileID: b}}})
	if err != nil {
		t.Fatal(err)
	}
	if v, err = mod.Service.PublishPost(ctx, admin, v.Post.ID); err != nil || v.Post.Status != feed.StatusPublished {
		t.Fatalf("PublishPost = %+v, %v", v.Post.Status, err)
	}
	if v, err = mod.Service.UpdatePost(ctx, admin, v.Post.ID, v.ETag, feed.PostPatch{Media: &[]feed.MediaInput{{FileID: c}}}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []uuid.UUID{a, b} {
		if m, err := fm.Service.Get(ctx, f); err != nil || m.AttachedModule != "feed" || m.AttachedEntityID != v.Post.ID {
			t.Fatalf("dropped image %s = %+v, %v; want kept, attached to the post", f, m, err)
		}
	}
	dropped, err := store.DroppedMediaOf(ctx, []uuid.UUID{v.Post.ID})
	if err != nil || len(dropped[v.Post.ID]) != 2 {
		t.Fatalf("DroppedMediaOf = %+v, %v", dropped, err)
	}
	// Listed again, b comes back.
	if v, err = mod.Service.UpdatePost(ctx, admin, v.Post.ID, v.ETag, feed.PostPatch{Media: &[]feed.MediaInput{{FileID: c}, {FileID: b}}}); err != nil {
		t.Fatal(err)
	}
	if dropped, _ = store.DroppedMediaOf(ctx, []uuid.UUID{v.Post.ID}); len(dropped[v.Post.ID]) != 1 || dropped[v.Post.ID][0].FileID != a {
		t.Fatalf("dropped after b came back = %+v", dropped)
	}

	// 13 months later the dropped image goes, row then file.
	now := clk.Now().AddDate(0, 13, 0)
	year := now.Sub(now.AddDate(0, -12, 0))
	res, err := mod.Sweeper.Sweep(ctx, privacy.RetentionPolicy{Code: feed.PolicyMediaDropped, Retention: year, LegalApproved: true}, now)
	if err != nil || res.Count != 1 || !gone(a) || gone(b) || gone(c) {
		t.Fatalf("post_media_dropped = %+v, %v; a gone %v, b gone %v, c gone %v", res, err, gone(a), gone(b), gone(c))
	}

	// A removed post's purge takes its dropped images with it.
	if v, err = mod.Service.UpdatePost(ctx, admin, v.Post.ID, v.ETag, feed.PostPatch{Media: &[]feed.MediaInput{{FileID: c}}}); err != nil {
		t.Fatal(err)
	}
	if err := mod.Service.DeletePost(ctx, admin, v.Post.ID); err != nil { // published → removed
		t.Fatal(err)
	}
	if gone(b) || gone(c) {
		t.Fatal("the removal deleted images before the purge")
	}
	res, err = mod.Sweeper.Sweep(ctx, privacy.RetentionPolicy{Code: feed.PolicyPostRemoved, Retention: year, LegalApproved: true}, now)
	if err != nil || res.Count != 1 || !gone(b) || !gone(c) {
		t.Fatalf("post_removed = %+v, %v; b gone %v, c gone %v", res, err, gone(b), gone(c))
	}
}
