package files

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/queue"
	"koro/internal/platform/storage"
)

// Task, job and topic names (01 §6.2, §7).
const (
	// TopicScan is the event that triggers the scan (01 §6.2 file.uploaded, A).
	TopicScan = domainevent.TopicFileUploaded
	// ScanTaskName is the outbox subscription of file.uploaded.
	ScanTaskName = "files.scan_file"
	// OrphanCleanupJobName deletes unattached files (01 §7: daily 04:40, low).
	OrphanCleanupJobName = "files.orphan_cleanup"
	// RescanPendingJobName re-enqueues stuck scans (01 §7: every 10 min, low).
	RescanPendingJobName = "files.rescan_pending"
	// DeleteObjectTaskName retries an object deletion that failed after commit.
	DeleteObjectTaskName = "files.delete_object"

	// DefaultOrphanRetention is files_orphan (02 §24.2: 24 hours).
	DefaultOrphanRetention = 24 * time.Hour
	// RescanAfter is how long a scan may stay pending before it is re-enqueued.
	RescanAfter = 10 * time.Minute

	orphanCron   = "40 4 * * *"
	orphanUnique = 23 * time.Hour
	rescanCron   = "*/10 * * * *"
	rescanUnique = 9 * time.Minute
	batchSize    = 1000 // 01 §7: ≤ 1000 rows per batch
)

// moduleRe is files_attached_module_chk.
var moduleRe = regexp.MustCompile(`^[a-z_]+$`)

// handleUploaded is the file.uploaded subscription (files.scan_file).
func (s *service) handleUploaded(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.FileUploaded](e)
	if err != nil {
		return queue.Permanent(err)
	}
	return s.scan(ctx, p.FileID)
}

// scan runs the scanner on a pending file and moves a clean (or skipped)
// object from the quarantine bucket to the main bucket. It is idempotent:
// a file that is gone, not active or no longer pending is left alone. An
// error leaves the file pending; the task is retried and
// files.rescan_pending re-enqueues it later.
func (s *service) scan(ctx context.Context, id uuid.UUID) error {
	f, err := s.d.Store.Get(ctx, id)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil
	case err != nil:
		return err
	case f.Status != StatusActive || f.ScanStatus != ScanPending:
		return nil
	}
	m := f.meta()
	v, err := s.d.Scanner.Scan(ctx, ScanObject{
		FileID: f.ID, Purpose: f.Purpose, MIME: m.MIME, Size: m.SizeBytes,
		Open: func(ctx context.Context) (io.ReadCloser, error) {
			rc, _, err := s.d.Objects.Open(ctx, f.Bucket, f.StorageKey)
			return rc, err
		},
	})
	if err != nil {
		return fmt.Errorf("files: scan %s with %s: %w", f.ID, s.d.Scanner.Name(), err)
	}

	quarantined := f.Bucket != s.d.Buckets.Files
	switch v.Status {
	case ScanClean, ScanSkipped:
		if quarantined {
			err := s.d.Objects.Copy(ctx, storage.CopyInput{
				SrcBucket: f.Bucket, SrcKey: f.StorageKey, DstBucket: s.d.Buckets.Files, DstKey: f.StorageKey,
				ContentType: m.MIME, ContentDisposition: Disposition(m.OriginalName, m.MIME),
			})
			if err != nil {
				return fmt.Errorf("files: move %s out of quarantine: %w", f.ID, err)
			}
		}
	case ScanInfected:
		// Never keep malware: drop the object first; the row records the
		// verdict (BE-048 deletes it and notifies the owner).
		if err := s.d.Objects.Delete(ctx, f.Bucket, f.StorageKey); err != nil {
			return fmt.Errorf("files: delete infected %s: %w", f.ID, err)
		}
	default:
		return fmt.Errorf("files: scanner %s returned an invalid verdict %q", s.d.Scanner.Name(), v.Status)
	}

	applied, gone := false, false
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		cur, err := s.d.Store.Lock(ctx, id)
		switch {
		case errors.Is(err, ErrNotFound):
			gone = true
			return nil
		case err != nil:
			return err
		case cur.Status != StatusActive || cur.ScanStatus != ScanPending:
			return nil
		}
		now := s.d.Clock.Now()
		cur.ScanStatus, cur.ScannedAt = v.Status, &now
		if v.Status != ScanInfected {
			cur.Bucket = s.d.Buckets.Files
		}
		applied = true
		return s.d.Store.Update(ctx, cur)
	})
	if err != nil {
		return err
	}
	if gone { // deleted while scanning: drop the copy we just made
		s.removeObjects(ctx, f)
		return nil
	}
	if applied && quarantined && v.Status != ScanInfected {
		if err := s.d.Objects.Delete(ctx, f.Bucket, f.StorageKey); err != nil {
			s.d.Log.LogAttrs(ctx, slog.LevelWarn, "quarantine copy not deleted (lifecycle rule removes it)",
				slog.String("file_id", f.ID.String()), slog.String("err", err.Error()))
		}
	}
	level, attrs := slog.LevelInfo, []slog.Attr{
		slog.String("file_id", f.ID.String()), slog.String("purpose", string(f.Purpose)),
		slog.String("scanner", s.d.Scanner.Name()), slog.String("scan_status", string(v.Status)),
	}
	if v.Status == ScanInfected {
		level = slog.LevelWarn
		attrs = append(attrs, slog.String("signature", v.Signature), slog.Bool("alarm", true))
	}
	s.d.Log.LogAttrs(ctx, level, "file scanned", attrs...)
	return nil
}

// orphanCleanupJob is files.orphan_cleanup (01 §7): unattached files older
// than files_orphan (24 hours) are deleted with their objects, uploads that
// never completed included.
func (s *service) orphanCleanupJob() queue.Job {
	return queue.Job{
		Name: OrphanCleanupJobName, Cron: orphanCron, Queue: queue.Low, Unique: orphanUnique,
		Handle: s.cleanupOrphans,
	}
}

func (s *service) cleanupOrphans(ctx context.Context) error {
	if err := authz.Can(ctx, authz.SystemActor(OrphanCleanupJobName), authz.FilesOrphanPurge, authz.Resource{Type: authz.TypeFile}); err != nil {
		return queue.Permanent(err)
	}
	keep, err := s.d.OrphanRetention(ctx)
	if err != nil {
		return err
	}
	cutoff := s.d.Clock.Now().Add(-keep)
	deleted, skipped, after := 0, 0, uuid.Nil()
	for {
		batch, err := s.d.Store.Orphans(ctx, cutoff, after, batchSize)
		if err != nil {
			return err
		}
		for _, f := range batch {
			after = f.ID
			switch err := s.remove(ctx, f.ID, nil); {
			case err == nil:
				deleted++
			case errors.Is(err, ErrReferenced):
				// Referenced but never attached: a bug in the owning module.
				skipped++
				s.d.Log.LogAttrs(ctx, slog.LevelError, "orphan file is referenced, not deleted",
					slog.String("file_id", f.ID.String()), slog.String("purpose", string(f.Purpose)))
			default:
				return err
			}
		}
		if len(batch) < batchSize {
			break
		}
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "files orphan cleanup done", slog.Int("deleted", deleted), slog.Int("skipped", skipped))
	return nil
}

// rescanPendingJob is files.rescan_pending (01 §7): scans pending for more
// than RescanAfter are enqueued again (lost task, exhausted retries).
func (s *service) rescanPendingJob() queue.Job {
	return queue.Job{
		Name: RescanPendingJobName, Cron: rescanCron, Queue: queue.Low, Unique: rescanUnique,
		Handle: s.rescanPending,
	}
}

func (s *service) rescanPending(ctx context.Context) error {
	cutoff := s.d.Clock.Now().Add(-RescanAfter)
	n, after := 0, uuid.Nil()
	for {
		batch, err := s.d.Store.PendingScans(ctx, cutoff, after, batchSize)
		if err != nil {
			return err
		}
		for _, f := range batch {
			after = f.ID
			ev, err := domainevent.New(domainevent.FileUploaded{FileID: f.ID}, "file", f.ID)
			if err != nil {
				return err
			}
			if err := s.d.Outbox.AfterCommit(ctx, ev); err != nil {
				return err
			}
			n++
		}
		if len(batch) < batchSize {
			break
		}
	}
	if n > 0 {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "pending file scans re-enqueued", slog.Int("count", n))
	}
	return nil
}

// deleteObject is the payload of files.delete_object.
type deleteObject struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
}

// deleteObjectTask retries an object deletion that failed after the row was
// deleted (asynq retries with back-off; Delete is idempotent).
func (s *service) deleteObjectTask() queue.Task {
	return queue.Task{Name: DeleteObjectTaskName, Handle: func(ctx context.Context, payload []byte) error {
		var p deleteObject
		if err := json.Unmarshal(payload, &p); err != nil || p.Bucket == "" || p.Key == "" {
			return queue.Permanent(fmt.Errorf("files: delete object: bad payload: %w", err))
		}
		if p.Bucket != s.d.Buckets.Files && p.Bucket != s.d.Buckets.Quarantine {
			return queue.Permanent(fmt.Errorf("files: delete object: unknown bucket %q", p.Bucket))
		}
		return s.d.Objects.Delete(ctx, p.Bucket, p.Key)
	}}
}
