package events_test

import (
	"errors"
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/events"
	"koro/internal/platform/problem"
)

func TestStore_ExportContract_Mem(t *testing.T) {
	t.Parallel()
	exportContract(t, func(*testing.T) (events.Store, world) {
		return events.NewMemStore(func() time.Time { return t0 }), memWorld()
	})
}

// exportContract is ExportStore (02 §11.9; BE-050): the export takes
// event_id and org_id from the events row (06 H-38) and is read only
// through its event; the CHECKs hold; a download is counted only while
// ready; the job reads (due, queued) are by id after a position; the KVKK
// reads go by requester; the export log is swept by created_at, never a
// row still holding its file.
func exportContract(t *testing.T, newStore func(t *testing.T) (events.Store, world)) {
	t.Helper()
	setup := func(t *testing.T) (events.Store, world, events.Event, uuid.UUID) {
		t.Helper()
		s, w := newStore(t)
		org, by := w.org(t), w.user(t)
		e, ok, err := s.InsertEvent(t.Context(), row(org, by, "Dışa Aktarılan", start))
		if err != nil || !ok {
			t.Fatalf("InsertEvent: %v %v", ok, err)
		}
		return s, w, e, by
	}
	queued := func(e events.Event, by uuid.UUID) events.Export {
		ip := "203.0.113.7"
		return events.Export{ID: uuid.NewV7(), EventID: e.ID, OrgID: e.OrgID, RequestedBy: by,
			Reason: "Yoklama listesi kapı için gerekli", Fields: []string{"first_name", "answers.motivation"},
			Format: events.FormatCSV, Status: events.ExportQueued, IP: &ip, ExpiresAt: start.Add(7 * 24 * time.Hour)}
	}

	t.Run("insert takes the scope from the event; reads go through it", func(t *testing.T) {
		t.Parallel()
		s, w, e, by := setup(t)
		ctx := t.Context()
		x, err := s.InsertExport(ctx, queued(e, by))
		if err != nil {
			t.Fatal(err)
		}
		if x.CreatedAt.IsZero() || !x.UpdatedAt.Equal(x.CreatedAt) || x.DownloadCount != 0 || x.IncludeSensitive ||
			x.IP == nil || *x.IP != "203.0.113.7" || !slices.Equal([]string(x.Fields), []string{"first_name", "answers.motivation"}) ||
			x.Format != events.FormatCSV || x.Status != events.ExportQueued {
			t.Fatalf("stored export = %+v", x)
		}
		other := queued(e, by)
		other.OrgID = w.org(t)
		if _, err := s.InsertExport(ctx, other); !errors.Is(err, events.ErrNotFound) {
			t.Fatalf("insert under another org = %v, want ErrNotFound (06 H-38)", err)
		}
		if _, err := s.GetExport(ctx, w.org(t), e.ID, x.ID); !errors.Is(err, events.ErrNotFound) {
			t.Fatalf("GetExport of another org = %v", err)
		}
		if _, err := s.GetExport(ctx, e.OrgID, uuid.NewV7(), x.ID); !errors.Is(err, events.ErrNotFound) {
			t.Fatalf("GetExport through another event = %v", err)
		}
		got, err := s.LockExport(ctx, e.OrgID, e.ID, x.ID)
		if err != nil || got.ID != x.ID {
			t.Fatalf("LockExport = %+v, %v", got, err)
		}
	})

	t.Run("checks", func(t *testing.T) {
		t.Parallel()
		s, _, e, by := setup(t)
		ctx := t.Context()
		for name, mut := range map[string]func(*events.Export){
			"a short reason":  func(x *events.Export) { x.Reason = "kısa" },
			"no fields":       func(x *events.Export) { x.Fields = nil },
			"41 fields":       func(x *events.Export) { x.Fields = slices.Repeat([]string{"status"}, 41) },
			"unknown format":  func(x *events.Export) { x.Format = "pdf" },
			"ready, no file":  func(x *events.Export) { x.Status = events.ExportReady },
			"unknown status":  func(x *events.Export) { x.Status = "done" },
			"purged, no time": func(x *events.Export) { x.Status = events.ExportPurged },
		} {
			x := queued(e, by)
			mut(&x)
			if _, err := s.InsertExport(ctx, x); !errors.Is(err, problem.ErrValidation) {
				t.Errorf("%s: %v, want a check violation (422)", name, err)
			}
		}
	})

	t.Run("update, download count, purge", func(t *testing.T) {
		t.Parallel()
		s, w, e, by := setup(t)
		ctx := t.Context()
		x, err := s.InsertExport(ctx, queued(e, by))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.RecordDownload(ctx, e.OrgID, e.ID, x.ID, t0); !errors.Is(err, events.ErrNotFound) {
			t.Fatalf("download of a queued export = %v, want ErrNotFound", err)
		}
		file, n := w.file(t), 12
		sum := make([]byte, 32)
		x.Status, x.FileID, x.FileSHA256, x.RowCount = events.ExportReady, &file, sum, &n
		ready, err := s.UpdateExport(ctx, x)
		if err != nil || ready.Status != events.ExportReady || ready.FileID == nil || *ready.FileID != file ||
			ready.RowCount == nil || *ready.RowCount != 12 || len(ready.FileSHA256) != 32 {
			t.Fatalf("UpdateExport(ready) = %+v, %v", ready, err)
		}
		first, err := s.RecordDownload(ctx, e.OrgID, e.ID, x.ID, t0)
		if err != nil || first.DownloadCount != 1 || first.FirstDownloadedAt == nil || !first.FirstDownloadedAt.Equal(t0) {
			t.Fatalf("first download = %+v, %v", first, err)
		}
		second, err := s.RecordDownload(ctx, e.OrgID, e.ID, x.ID, t0.Add(time.Hour))
		if err != nil || second.DownloadCount != 2 || !second.FirstDownloadedAt.Equal(t0) {
			t.Fatalf("second download = %+v, %v (first_downloaded_at stays)", second, err)
		}
		if _, err := s.RecordDownload(ctx, w.org(t), e.ID, x.ID, t0); !errors.Is(err, events.ErrNotFound) {
			t.Fatalf("download under another org = %v", err)
		}
		bad := second
		bad.Status, bad.PurgedAt = events.ExportPurged, new(t0)
		if _, err := s.UpdateExport(ctx, bad); !errors.Is(err, problem.ErrValidation) {
			t.Fatalf("purged with its file = %v, want a check violation", err)
		}
		bad.FileID = nil
		purged, err := s.UpdateExport(ctx, bad)
		if err != nil || purged.Status != events.ExportPurged || purged.FileID != nil || purged.PurgedAt == nil ||
			purged.DownloadCount != 2 {
			t.Fatalf("purged = %+v, %v", purged, err)
		}
		if _, err := s.RecordDownload(ctx, e.OrgID, e.ID, x.ID, t0); !errors.Is(err, events.ErrNotFound) {
			t.Fatalf("download of a purged export = %v", err)
		}
	})

	t.Run("lists, job reads, KVKK reads and the export log", func(t *testing.T) {
		t.Parallel()
		s, w, e, by := setup(t)
		ctx := t.Context()
		other := w.user(t)
		var all []events.Export
		for i := range 3 {
			x := queued(e, by)
			if i == 2 {
				x.RequestedBy = other
			}
			got, err := s.InsertExport(ctx, x)
			if err != nil {
				t.Fatal(err)
			}
			all = append(all, got)
		}
		page1, err := s.ListExports(ctx, e.OrgID, e.ID, nil, 2)
		if err != nil || len(page1) != 2 {
			t.Fatalf("ListExports = %d rows, %v", len(page1), err)
		}
		after := &events.After{At: page1[1].CreatedAt, ID: page1[1].ID}
		page2, err := s.ListExports(ctx, e.OrgID, e.ID, after, 2)
		if err != nil || len(page2) != 1 {
			t.Fatalf("second page = %d rows, %v", len(page2), err)
		}
		seen := []uuid.UUID{page1[0].ID, page1[1].ID, page2[0].ID}
		for _, x := range all {
			if !slices.Contains(seen, x.ID) {
				t.Fatalf("export %s missing from the pages", x.ID)
			}
		}
		if !page1[0].CreatedAt.After(page2[0].CreatedAt) && !page1[0].CreatedAt.Equal(page2[0].CreatedAt) {
			t.Fatal("the list is not newest first")
		}
		if rows, _ := s.ListExports(ctx, w.org(t), e.ID, nil, 10); len(rows) != 0 {
			t.Fatal("another org lists the event's exports")
		}
		// created_at comes from the database clock: the bounds from the rows.
		cutoff := all[0].CreatedAt
		for _, x := range all {
			if x.CreatedAt.After(cutoff) {
				cutoff = x.CreatedAt
			}
		}
		cutoff = cutoff.Add(time.Second)
		// The job reads: queued ones created before a time, by id; due ready ones.
		q, err := s.QueuedExports(ctx, cutoff, uuid.Nil(), 2)
		if err != nil || len(q) != 2 {
			t.Fatalf("QueuedExports = %d, %v", len(q), err)
		}
		rest, err := s.QueuedExports(ctx, cutoff, q[1].ID, 10)
		if err != nil || len(rest) != 1 {
			t.Fatalf("QueuedExports after = %d, %v", len(rest), err)
		}
		if old, _ := s.QueuedExports(ctx, all[0].CreatedAt.Add(-time.Hour), uuid.Nil(), 10); len(old) != 0 {
			t.Fatal("QueuedExports returned exports created after the bound")
		}
		file := w.file(t)
		r := all[0]
		r.Status, r.FileID, r.FileSHA256 = events.ExportReady, &file, make([]byte, 32)
		if _, err := s.UpdateExport(ctx, r); err != nil {
			t.Fatal(err)
		}
		due, err := s.ExportsDue(ctx, r.ExpiresAt, uuid.Nil(), 10)
		if err != nil || len(due) != 1 || due[0].ID != r.ID {
			t.Fatalf("ExportsDue at expiry = %+v, %v", due, err)
		}
		if early, _ := s.ExportsDue(ctx, r.ExpiresAt.Add(-time.Second), uuid.Nil(), 10); len(early) != 0 {
			t.Fatal("ExportsDue before expiry")
		}
		// KVKK: the requester's exports and the IP erasure.
		mine, err := s.ExportsOfUser(ctx, by)
		if err != nil || len(mine) != 2 {
			t.Fatalf("ExportsOfUser = %d, %v", len(mine), err)
		}
		n, err := s.ClearExportIPs(ctx, by)
		if err != nil || n != 2 {
			t.Fatalf("ClearExportIPs = %d, %v", n, err)
		}
		if n, _ := s.ClearExportIPs(ctx, by); n != 0 {
			t.Fatalf("second ClearExportIPs = %d", n)
		}
		if x, _ := s.GetExport(ctx, e.OrgID, e.ID, all[2].ID); x.IP == nil {
			t.Fatal("another requester's IP was cleared")
		}
		// The export log: rows created before the cutoff without a file go;
		// the ready one waits for its file purge.
		if n, err := s.PurgeExpired(ctx, events.PolicyExportLog, cutoff, 0, true); err != nil || n != 2 {
			t.Fatalf("dry run = %d, %v", n, err)
		}
		if n, err := s.PurgeExpired(ctx, events.PolicyExportLog, cutoff, 1, false); err != nil || n != 1 {
			t.Fatalf("first batch = %d, %v", n, err)
		}
		if n, err := s.PurgeExpired(ctx, events.PolicyExportLog, cutoff, 10, false); err != nil || n != 1 {
			t.Fatalf("second batch = %d, %v", n, err)
		}
		if _, err := s.GetExport(ctx, e.OrgID, e.ID, r.ID); err != nil {
			t.Fatalf("the export still holding its file was swept: %v", err)
		}
		if n, _ := s.PurgeExpired(ctx, events.PolicyExportLog, cutoff, 10, true); n != 0 {
			t.Fatalf("after the sweep %d rows are due", n)
		}
	})
}
