package events_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/events"
	"koro/internal/platform/privacy"
	"koro/internal/platform/problem"
)

// spyDestructions is kvkk's privacy.DestructionRecorder for the tests: it
// keeps every record with the transaction it was written in (T-318).
type spyDestructions struct {
	mu   sync.Mutex
	recs []privacy.Destruction
	txs  []int
	fail error
}

func (s *spyDestructions) Record(ctx context.Context, d privacy.Destruction) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	s.recs, s.txs = append(s.recs, d), append(s.txs, txOf(ctx))
	return nil
}

func (s *spyDestructions) all() ([]privacy.Destruction, []int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]privacy.Destruction(nil), s.recs...), append([]int(nil), s.txs...)
}

// TestPurgeExports_RecordsDestruction (T-318, BE-083; 02 §16.3): every
// export file events.purge_exports deletes leaves one destruction_logs row
// (policy event_export_file, module events, target files:event_export,
// file_delete, count 1, the file id), written in the transaction that
// deletes the file, next to the events.export.purge audit record. The rows
// of one run share its run id; a second run records nothing (idempotent).
func TestPurgeExports_RecordsDestruction(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.exportEvent(t, "student")
	x1 := e.export(t, ev.Event.ID, events.ExportInput{Reason: exportWhy, Fields: []string{"first_name"}})
	x2 := e.export(t, ev.Event.ID, events.ExportInput{Reason: exportWhy, Fields: []string{"status"}})
	e.build(t)
	files := map[string]bool{e.stored(t, x1).FileID.String(): true, e.stored(t, x2).FileID.String(): true}
	e.clk.Advance(events.ExportLifetime - time.Minute)
	e.job(t, events.PurgeExportsJobName)
	if recs, _ := e.destr.all(); len(recs) != 0 {
		t.Fatalf("destruction recorded before the expiry: %+v", recs)
	}
	e.clk.Advance(time.Minute)
	now := e.clk.Now()
	e.job(t, events.PurgeExportsJobName)
	recs, txs := e.destr.all()
	if len(recs) != 2 {
		t.Fatalf("destruction records = %+v, want one per file", recs)
	}
	for i, r := range recs {
		if r.PolicyCode != events.PolicyExportFile || r.Module != "events" || r.Target != "files:event_export" ||
			r.Method != privacy.MethodFileDelete || r.Count != 1 || r.RunID == uuid.Nil() || r.RunID != recs[0].RunID ||
			!r.PeriodCutoff.Equal(now) || r.StartedAt.IsZero() || r.FinishedAt.Before(r.StartedAt) ||
			r.Details["job"] != events.PurgeExportsJobName || !files[r.Details["file_id"].(string)] {
			t.Fatalf("destruction record %d = %+v", i, r)
		}
		delete(files, r.Details["file_id"].(string))
		if txs[i] == 0 {
			t.Fatalf("destruction record %d written outside a transaction", i)
		}
	}
	if txs[0] == txs[1] {
		t.Fatal("two files recorded in one transaction: want one transaction per export")
	}
	if n := len(e.audit.of(authz.EventsExportPurge)); n != 2 {
		t.Fatalf("purge audit records = %d, want 2 (kept next to the destruction record)", n)
	}
	e.clk.Advance(time.Hour)
	e.job(t, events.PurgeExportsJobName)
	if recs, _ := e.destr.all(); len(recs) != 2 {
		t.Fatalf("a second run recorded again: %d records", len(recs))
	}
}

// TestPurgeExports_DestructionNotRecorded_FileKept: without the destruction
// record (hook not wired) the purge refuses and the file stays until a run
// can record it; a failing record fails the job (retried next hour).
func TestPurgeExports_DestructionNotRecorded_FileKept(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(d *events.Deps) { d.Destructions = nil })
	ev := e.exportEvent(t, "student")
	x := e.export(t, ev.Event.ID, events.ExportInput{Reason: exportWhy, Fields: []string{"first_name"}})
	e.build(t)
	fileID := *e.stored(t, x).FileID
	e.clk.Advance(events.ExportLifetime)
	if err := e.runJob(t, events.PurgeExportsJobName); err == nil {
		t.Fatal("the purge ran without a destruction record")
	}
	if got := e.stored(t, x); got.Status != events.ExportReady || got.FileID == nil {
		t.Fatalf("export without a destruction record = %+v", got)
	}
	if _, err := e.files.Get(t.Context(), fileID); err != nil {
		t.Fatalf("the file was deleted unrecorded: %v", err)
	}
	if n := len(e.audit.of(authz.EventsExportPurge)); n != 0 {
		t.Fatalf("purge audit records = %d", n)
	}

	f := newEnv(t)
	f.destr.fail = errors.New("destruction_logs unavailable")
	ev = f.exportEvent(t, "student")
	f.export(t, ev.Event.ID, events.ExportInput{Reason: exportWhy, Fields: []string{"first_name"}})
	f.build(t)
	f.clk.Advance(events.ExportLifetime)
	if err := f.runJob(t, events.PurgeExportsJobName); err == nil || errors.Is(err, problem.ErrNotFound) {
		t.Fatalf("purge with a failing record = %v, want the record's error", err)
	}
}

// runJob runs the job name once and returns its error.
func (e *env) runJob(t *testing.T, name string) error {
	t.Helper()
	for _, j := range e.mod.Jobs() {
		if j.Name == name {
			return j.Handle(t.Context())
		}
	}
	t.Fatalf("job %s is not registered", name)
	return nil
}
