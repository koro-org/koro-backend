//go:build integration

package orgs_test

import (
	"errors"
	"strings"
	"testing"
	"uuid"

	"koro/internal/audit/audittest"
	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/orgs"
	"koro/internal/platform/clock"
	"koro/internal/platform/httpx"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
	"koro/internal/platform/storage"
	"koro/internal/platform/testutil"
)

// TestOrgs_ReplacedLogo_Gorm (BE-155, T-376) runs updateOrg over PostgreSQL
// with the real files service: the replaced logo's files row goes in the
// update's transaction after logo_file_id moved on
// (organizations_logo_file_id_fkey would refuse it otherwise), and its
// public media copy is deleted after the commit; removing the logo deletes
// the new one the same way.
func TestOrgs_ReplacedLogo_Gorm(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	ctx := t.Context()
	clk := clock.NewFake(t0)
	objects := storage.NewMem(clk)
	public := files.PublicMedia{Bucket: "public-test", BaseURL: "https://media.koro.example/public"}
	idem := idempotency.New(idempotency.Deps{Tx: d.Tx, Store: idempotency.NewMemStore(), Clock: clk, Log: obs.Discard()})
	fm, err := files.New(files.Deps{
		Store: files.NewGormStore(d.ORM), Objects: objects, Tx: d.Tx, Outbox: outbox.NewMem(clk),
		Queue: queue.NewMem(), Idempotency: idem, Clock: clk, Log: obs.Discard(), Public: public,
		Buckets: files.Buckets{Files: "koro-files-dev", Quarantine: "quarantine"}, Audit: &audittest.Recorder{},
	})
	if err != nil {
		t.Fatal(err)
	}
	store := orgs.NewGormStore(d.ORM)
	mod, err := orgs.New(orgs.Deps{
		Store: store, Tx: d.Tx, Outbox: outbox.NewMem(clk), Audit: &audittest.Recorder{}, Clock: clk, Log: obs.Discard(),
		Idempotency: idem, Files: fm.Service,
	})
	if err != nil {
		t.Fatal(err)
	}
	id, err := mod.Service.Create(ctx, orgs.CreateOrg{Kind: orgs.KindClub, Name: "Logosu Değişen Kulüp"})
	if err != nil {
		t.Fatal(err)
	}
	admin := as(user(), id, orgs.KindClub, authz.MemberClubAdmin)
	// logo inserts a cleared org_logo upload of the org with a public copy.
	logo := func() (uuid.UUID, string) {
		t.Helper()
		f := uuid.NewV7()
		key := "org_logo/" + f.String() + "-" + strings.Repeat("ab", 16) + ".png"
		if _, err := d.Pool.Exec(ctx, `INSERT INTO files (id, purpose, org_id, bucket, storage_key, declared_mime, size_bytes,
			sha256, status, scan_status, public_key)
			VALUES ($1, 'org_logo', $2, 'koro-files-dev', $3, 'image/png', 10, decode(repeat('ab', 32), 'hex'), 'active', 'clean', $4)`,
			f, id, "org_logo/2026/10/"+f.String(), key); err != nil {
			t.Fatal(err)
		}
		objects.Upload(public.Bucket, key, []byte("png"), "image/png")
		return f, key
	}
	l1, k1 := logo()
	l2, k2 := logo()
	o, err := store.GetOrg(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	etag := httpx.ETag(o.UpdatedAt)
	update := func(p orgs.OrgPatch) {
		t.Helper()
		v, err := mod.Service.UpdateOrg(ctx, admin, id, etag, p)
		if err != nil {
			t.Fatal(err)
		}
		etag = v.ETag
	}
	gone := func(f uuid.UUID, key string) bool {
		t.Helper()
		_, err := fm.Service.Get(ctx, f)
		_, statErr := objects.Stat(ctx, public.Bucket, key)
		return errors.Is(err, files.ErrNotFound) && statErr != nil
	}

	update(orgs.OrgPatch{Logo: &l1})
	update(orgs.OrgPatch{Logo: &l2})
	if !gone(l1, k1) || gone(l2, k2) {
		t.Fatalf("after the replacement: old gone %v, new gone %v", gone(l1, k1), gone(l2, k2))
	}
	if m, err := fm.Service.Get(ctx, l2); err != nil || m.AttachedModule != "orgs" || m.AttachedEntityID != id {
		t.Fatalf("new logo = %+v, %v", m, err)
	}
	update(orgs.OrgPatch{ClearLogo: true})
	if after, err := store.GetOrg(ctx, id); err != nil || after.LogoFileID != nil || !gone(l2, k2) {
		t.Fatalf("after the removal: logo %v, %v, file gone %v", after.LogoFileID, err, gone(l2, k2))
	}
}
