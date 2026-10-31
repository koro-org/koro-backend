//go:build integration

package events_test

import (
	"errors"
	"testing"
	"uuid"

	"koro/internal/audit/audittest"
	"koro/internal/authz"
	"koro/internal/events"
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

// TestEvents_ReplacedCover_Gorm (BE-155, T-376) runs updateEvent and
// deleteEventDraft over PostgreSQL with the real files service: the
// replaced cover's files row goes in the update's transaction after
// cover_file_id moved on (events_cover_file_id_fkey would refuse it
// otherwise), and a deleted draft's cover goes after the row.
func TestEvents_ReplacedCover_Gorm(t *testing.T) {
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
	org := w.org(t)
	dir := newFakeOrgs()
	dir.orgs[org] = orgs.OrgSummary{ID: org, Kind: orgs.KindClub, Name: "Kulüp", Slug: "kulup", Status: orgs.StatusActive}
	by := w.user(t)
	admin := authz.Actor{Kind: authz.KindUser, UserID: by,
		Memberships: []authz.Membership{{OrgID: org, OrgKind: authz.OrgClub, Role: authz.MemberClubAdmin}}}
	store := events.NewGormStore(d.ORM)
	mod, err := events.New(events.Deps{
		Store: store, Tx: d.Tx, Outbox: out, Audit: &audittest.Recorder{}, Orgs: dir, Catalog: fakeCatalog{},
		Idempotency: idem, Clock: clk, Log: obs.Discard(), Consents: &spyRecorder{}, Sealer: testEnvelope(),
		Tickets: testDeriver(), Sender: &spySender{}, Files: fm.Service, Reauth: reauth{}, Queue: queue.NewMem(),
		Destructions: &spyDestructions{},
	})
	if err != nil {
		t.Fatal(err)
	}
	// cover inserts a cleared event_cover upload of the org, attached to
	// event when it is not nil.
	cover := func(event *uuid.UUID) uuid.UUID {
		t.Helper()
		f := uuid.NewV7()
		if _, err := d.Pool.Exec(ctx, `INSERT INTO files (id, purpose, org_id, attached_module, attached_entity_id, attached_at,
			bucket, storage_key, declared_mime, size_bytes, sha256, status, scan_status)
			VALUES ($1, 'event_cover', $2, CASE WHEN $3::uuid IS NULL THEN NULL ELSE 'events' END, $3,
				CASE WHEN $3::uuid IS NULL THEN NULL ELSE now() END,
				'files', $4, 'image/png', 10, decode(repeat('ab', 32), 'hex'), 'active', 'clean')`,
			f, org, event, "event_cover/2026/10/"+f.String()); err != nil {
			t.Fatal(err)
		}
		return f
	}
	gone := func(f uuid.UUID) bool {
		t.Helper()
		_, err := fm.Service.Get(ctx, f)
		return errors.Is(err, files.ErrNotFound)
	}
	evID := uuid.NewV7()
	c1 := cover(&evID)
	ev := row(org, by, "Kapaklı Etkinlik", start)
	ev.ID, ev.CoverFileID = evID, &c1
	stored, ok, err := store.InsertEvent(ctx, ev)
	if err != nil || !ok {
		t.Fatalf("InsertEvent: %v %v", ok, err)
	}
	c2 := cover(nil)
	v, err := mod.Service.UpdateEvent(ctx, admin, evID, httpx.ETag(stored.UpdatedAt), events.EventPatch{CoverFileID: &c2})
	if err != nil {
		t.Fatal(err)
	}
	if !gone(c1) || gone(c2) {
		t.Fatalf("after the replacement: old gone %v, new gone %v", gone(c1), gone(c2))
	}
	if _, err := mod.Service.UpdateEvent(ctx, admin, evID, v.ETag, events.EventPatch{Clear: []string{"cover_file_id"}}); err != nil {
		t.Fatal(err)
	}
	if !gone(c2) {
		t.Fatal("the removed cover was kept")
	}

	draftID := uuid.NewV7()
	c3 := cover(&draftID)
	draft := row(org, by, "Taslak", start)
	draft.ID, draft.Status, draft.PublishedAt, draft.CoverFileID = draftID, events.StatusDraft, nil, &c3
	if _, ok, err := store.InsertEvent(ctx, draft); err != nil || !ok {
		t.Fatalf("InsertEvent: %v %v", ok, err)
	}
	if err := mod.Service.DeleteEventDraft(ctx, admin, draftID); err != nil {
		t.Fatal(err)
	}
	if !gone(c3) {
		t.Fatal("the deleted draft's cover was kept")
	}
}
