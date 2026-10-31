//go:build integration

package events_test

import (
	"errors"
	"testing"
	"uuid"

	"koro/internal/audit/audittest"
	"koro/internal/events"
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

// TestEvents_FileRejected_Gorm (T-176) runs events.drop_rejected_cover over
// PostgreSQL with the real files service: cover_file_id is cleared before
// the files row goes, in one transaction (events_cover_file_id_fkey would
// refuse the delete otherwise), and the touch trigger moves the ETag.
func TestEvents_FileRejected_Gorm(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	w := pgWorld(d)
	ctx := t.Context()
	clk := clock.NewFake(t0)
	idem := idempotency.New(idempotency.Deps{Tx: d.Tx, Store: idempotency.NewMemStore(), Clock: clk, Log: obs.Discard()})
	out := outbox.NewMem(clk)
	fm, err := files.New(files.Deps{
		Store: files.NewGormStore(d.ORM), Objects: storage.NewMem(clk), Tx: d.Tx, Outbox: out, Queue: queue.NewMem(),
		Idempotency: idem, Clock: clk, Log: obs.Discard(), Buckets: files.Buckets{Files: "files", Quarantine: "quarantine"},
		Audit: &audittest.Recorder{},
	})
	if err != nil {
		t.Fatal(err)
	}
	store := events.NewGormStore(d.ORM)
	mod, err := events.New(events.Deps{
		Store: store, Tx: d.Tx, Outbox: out, Audit: &audittest.Recorder{}, Orgs: newFakeOrgs(), Catalog: fakeCatalog{},
		Idempotency: idem, Clock: clk, Log: obs.Discard(), Consents: &spyRecorder{}, Sealer: testEnvelope(),
		Tickets: testDeriver(), Sender: &spySender{}, Files: fm.Service, Reauth: reauth{}, Queue: queue.NewMem(),
		Destructions: &spyDestructions{},
	})
	if err != nil {
		t.Fatal(err)
	}
	org := w.org(t)
	cover := uuid.NewV7()
	ev := row(org, w.user(t), "Kapaklı Etkinlik", start)
	if _, err := d.Pool.Exec(ctx, `INSERT INTO files (id, purpose, org_id, attached_module, attached_entity_id, attached_at,
		bucket, storage_key, declared_mime, size_bytes, sha256, status, scan_status)
		VALUES ($1, 'event_cover', $2, 'events', $3, now(), 'files', $4, 'image/png', 10, decode(repeat('ab', 32), 'hex'),
		'active', 'infected')`, cover, org, ev.ID, "event_cover/2026/10/"+cover.String()); err != nil {
		t.Fatal(err)
	}
	ev.CoverFileID = &cover
	stored, ok, err := store.InsertEvent(ctx, ev)
	if err != nil || !ok {
		t.Fatalf("InsertEvent: %v %v", ok, err)
	}
	r := outbox.NewRegistry()
	mod.Subscribe(r)
	env, err := domainevent.New(domainevent.FileRejected{FileID: cover, Purpose: string(files.PurposeEventCover),
		Reason: "infected", AttachedModule: "events", AttachedEntityID: &ev.ID}, "file", cover)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range r.For(domainevent.TopicFileRejected) {
		if s.Name != events.DropRejectedCoverTaskName {
			continue
		}
		for range 2 { // at-least-once delivery
			if err := s.Handle(ctx, env); err != nil {
				t.Fatal(err)
			}
		}
	}
	after, err := store.GetEvent(ctx, ev.ID)
	if err != nil || after.CoverFileID != nil || !after.UpdatedAt.After(stored.UpdatedAt) || after.Status != events.StatusPublished {
		t.Fatalf("event after the rejection = %+v, %v", after, err)
	}
	if _, err := fm.Service.Get(ctx, cover); !errors.Is(err, files.ErrNotFound) {
		t.Fatalf("rejected files row kept: %v", err)
	}
}
