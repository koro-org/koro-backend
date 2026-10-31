//go:build integration

package orgs_test

import (
	"errors"
	"testing"
	"uuid"

	"koro/internal/audit/audittest"
	"koro/internal/files"
	"koro/internal/orgs"
	"koro/internal/platform/clock"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
	"koro/internal/platform/storage"
	"koro/internal/platform/testutil"
)

// TestOrgs_FileRejected_Gorm (T-176) runs orgs.drop_rejected_logo over
// PostgreSQL with the real files service: logo_file_id is cleared before
// the files row goes, in one transaction (organizations_logo_file_id_fkey
// would refuse the delete otherwise), and the touch trigger moves the ETag.
func TestOrgs_FileRejected_Gorm(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
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
	store := orgs.NewGormStore(d.ORM)
	mod, err := orgs.New(orgs.Deps{
		Store: store, Tx: d.Tx, Outbox: outbox.NewMem(clk), Audit: &audittest.Recorder{}, Clock: clk, Log: obs.Discard(),
		Idempotency: idem, Files: fm.Service,
	})
	if err != nil {
		t.Fatal(err)
	}
	id, err := mod.Service.Create(ctx, orgs.CreateOrg{Kind: orgs.KindClub, Name: "Logolu Kulüp"})
	if err != nil {
		t.Fatal(err)
	}
	logo := uuid.NewV7()
	if _, err := d.Pool.Exec(ctx, `INSERT INTO files (id, purpose, org_id, attached_module, attached_entity_id, attached_at,
		bucket, storage_key, declared_mime, size_bytes, sha256, status, scan_status)
		VALUES ($1, 'org_logo', $2, 'orgs', $2, now(), 'koro-files-dev', $3, 'image/png', 10, decode(repeat('ab', 32), 'hex'),
		'active', 'infected')`, logo, id, "org_logo/2026/10/"+logo.String()); err != nil {
		t.Fatal(err)
	}
	o, err := store.GetOrg(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	o.LogoFileID = &logo
	if o, err = store.UpdateOrg(ctx, o); err != nil {
		t.Fatal(err)
	}
	r := outbox.NewRegistry()
	mod.Subscribe(r)
	env, err := domainevent.New(domainevent.FileRejected{FileID: logo, Purpose: string(files.PurposeOrgLogo), Reason: "infected",
		AttachedModule: "orgs", AttachedEntityID: &id}, "file", logo)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range r.For(domainevent.TopicFileRejected) {
		if s.Name != orgs.DropRejectedLogoTaskName {
			continue
		}
		for range 2 { // at-least-once delivery
			if err := s.Handle(ctx, env); err != nil {
				t.Fatal(err)
			}
		}
	}
	after, err := store.GetOrg(ctx, id)
	if err != nil || after.LogoFileID != nil || !after.UpdatedAt.After(o.UpdatedAt) {
		t.Fatalf("org after the rejection = %+v, %v", after, err)
	}
	if _, err := fm.Service.Get(ctx, logo); !errors.Is(err, files.ErrNotFound) {
		t.Fatalf("rejected files row kept: %v", err)
	}
}
