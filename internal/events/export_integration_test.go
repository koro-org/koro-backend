//go:build integration

package events_test

import (
	"context"
	"errors"
	"testing"
	"time"
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

func TestStore_ExportContract_Gorm(t *testing.T) {
	t.Parallel()
	exportContract(t, func(t *testing.T) (events.Store, world) {
		d := testutil.NewDB(t)
		return events.NewGormStore(d.ORM), pgWorld(d)
	})
}

// TestExportFlow_Postgres runs BE-050 against PostgreSQL with the real
// files service: the export row takes its scope from the event, the
// worker stores and attaches the file (the row ready, files.attached_*
// set), a download is counted in the database, and the 7-day purge clears
// file_id and deletes the files row in one transaction (the FK
// event_data_exports_file_id_fkey would refuse the delete otherwise) with
// its destruction record (T-318, BE-083): a failing record rolls the
// deletion back, the file and its reference stay for the next run.
func TestExportFlow_Postgres(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	clk := clock.NewFake(t0)
	w := pgWorld(d)
	e := &pgEnv{d: d, w: w, clk: clk, out: outbox.NewMem(clk), store: events.NewGormStore(d.ORM), org: w.org(t), audit: &audittest.Recorder{}}
	fo := newFakeOrgs()
	fo.orgs[e.org] = orgs.OrgSummary{ID: e.org, Kind: orgs.KindClub, Name: "PG Kulübü", Slug: "pg-kulubu", Status: orgs.StatusActive}
	idem := idempotency.New(idempotency.Deps{Tx: d.Tx, Store: idempotency.NewGormStore(d.ORM), Clock: clk, Log: obs.Discard()})
	q := queue.NewMem()
	fm, err := files.New(files.Deps{
		Store: files.NewGormStore(d.ORM), Objects: storage.NewMem(clk), Tx: d.Tx, Outbox: e.out, Queue: queue.NewMem(),
		Idempotency: idem, Clock: clk, Log: obs.Discard(), Buckets: files.Buckets{Files: "files", Quarantine: "quarantine"},
		Audit: e.audit,
	})
	if err != nil {
		t.Fatal(err)
	}
	destr := &spyDestructions{}
	mod, err := events.New(events.Deps{
		Store: e.store, Tx: d.Tx, Outbox: e.out, Audit: e.audit, Orgs: fo, Catalog: fakeCatalog{}, Idempotency: idem,
		Clock: clk, Log: obs.Discard(), Consents: &spyRecorder{}, Sealer: testEnvelope(), Tickets: testDeriver(),
		Sender: &spySender{}, Files: fm.Service, Reauth: reauth{}, Queue: q, Destructions: destr,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.mod, e.svc = mod, mod.Service
	ctx := t.Context()
	ev := e.event(t, 10, false)
	for range 2 {
		if _, err := e.svc.CreateRegistration(ctx, e.student(t), ev.ID, events.RegistrationInput{Answers: motivation}); err != nil {
			t.Fatal(err)
		}
	}
	admin := authz.Actor{Kind: authz.KindUser, UserID: w.user(t), SessionID: uuid.NewV7(),
		Memberships: []authz.Membership{{OrgID: e.org, OrgKind: authz.OrgClub, Role: authz.MemberClubAdmin}}}
	x, err := e.svc.CreateExport(httpx.WithClientIP(ctx, "203.0.113.5"), admin, ev.ID, events.ExportInput{Reason: exportWhy,
		Fields: []string{"registration_id", "status", "answers.motivation"}, Format: events.FormatCSV}, good)
	if err != nil {
		t.Fatalf("CreateExport: %v %v", err, fieldsOf(err))
	}
	if n := e.count(t, `SELECT count(*) FROM event_data_exports WHERE id = $1 AND org_id = $2 AND host(ip) = '203.0.113.5'
		AND format = 'csv' AND status = 'queued' AND fields = ARRAY['registration_id','status','answers.motivation']`, x.ID, e.org); n != 1 {
		t.Fatalf("stored export rows = %d", n)
	}
	var build func(context.Context, []byte) error
	for _, tk := range mod.Tasks() {
		if tk.Name == events.BuildExportTaskName {
			build = tk.Handle
		}
	}
	tasks := q.Tasks()
	if len(tasks) != 1 {
		t.Fatalf("tasks = %+v", tasks)
	}
	if err := build(ctx, tasks[0].Payload); err != nil {
		t.Fatal(err)
	}
	if n := e.count(t, `SELECT count(*) FROM event_data_exports x JOIN files f ON f.id = x.file_id
		WHERE x.id = $1 AND x.status = 'ready' AND x.row_count = 2 AND octet_length(x.file_sha256) = 32
		  AND f.purpose = 'event_export' AND f.attached_module = 'events' AND f.attached_entity_id = x.id AND f.org_id = x.org_id`,
		x.ID); n != 1 {
		t.Fatal("the export is not ready with its attached file")
	}
	if _, err := e.svc.DownloadExport(ctx, admin, ev.ID, x.ID); err != nil {
		t.Fatal(err)
	}
	if n := e.count(t, `SELECT download_count FROM event_data_exports WHERE id = $1 AND first_downloaded_at IS NOT NULL`, x.ID); n != 1 {
		t.Fatalf("download_count = %d", n)
	}
	clk.Advance(events.ExportLifetime)
	purge := func() error {
		for _, j := range mod.Jobs() {
			if j.Name == events.PurgeExportsJobName {
				return j.Handle(ctx)
			}
		}
		t.Fatal("events.purge_exports is not registered")
		return nil
	}
	destr.fail = errors.New("destruction_logs unavailable")
	if err := purge(); err == nil {
		t.Fatal("the purge succeeded without its destruction record")
	}
	if n := e.count(t, `SELECT count(*) FROM event_data_exports x JOIN files f ON f.id = x.file_id
		WHERE x.id = $1 AND x.status = 'ready' AND x.purged_at IS NULL`, x.ID); n != 1 {
		t.Fatal("a failed destruction record did not roll the purge back")
	}
	destr.fail = nil
	if err := purge(); err != nil {
		t.Fatal(err)
	}
	if recs, _ := destr.all(); len(recs) != 1 || recs[0].PolicyCode != events.PolicyExportFile || recs[0].Count != 1 ||
		recs[0].Target != "files:event_export" {
		t.Fatalf("destruction records = %+v", recs)
	}
	if n := e.count(t, `SELECT count(*) FROM event_data_exports WHERE id = $1 AND status = 'purged' AND file_id IS NULL
		AND purged_at IS NOT NULL`, x.ID); n != 1 {
		t.Fatal("the export is not purged")
	}
	if n := e.count(t, `SELECT count(*) FROM files WHERE attached_entity_id = $1`, x.ID); n != 0 {
		t.Fatal("the export file row survived the purge")
	}
	// The export log goes after its retention (created_at by the database clock).
	stored, err := e.store.GetExport(ctx, e.org, ev.ID, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := e.store.PurgeExpired(ctx, events.PolicyExportLog, stored.CreatedAt.Add(time.Second), 10, false); err != nil || n != 1 {
		t.Fatalf("export log sweep = %d, %v", n, err)
	}
}
