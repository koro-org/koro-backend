package files

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
	"koro/internal/platform/storage"
)

// Task, job and topic names (01 §6.2, §7).
const (
	// TopicScan is the event that triggers the scan (01 §6.2 file.uploaded, A).
	TopicScan = domainevent.TopicFileUploaded
	// ScanTaskName is the outbox subscription of file.uploaded.
	ScanTaskName = "files.scan_file"
	// TopicRejected tells that a file was taken out of use (01 §6.2
	// file.rejected, O; BE-048).
	TopicRejected = domainevent.TopicFileRejected
	// NotifyRejectedTaskName is the files subscription of file.rejected: it
	// notifies the uploader.
	NotifyRejectedTaskName = "files.notify_rejected"
	// RescanSkippedJobName rescans files whose scan was skipped (no-op
	// scanner) once a real scanner is wired (BE-048: every 10 min, low).
	RescanSkippedJobName = "files.rescan_skipped"
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

	// DefaultRescanBatch is FILES_RESCAN_BATCH: skipped files rescanned per
	// run of files.rescan_skipped (≈ 14 400 a day).
	DefaultRescanBatch = 100
	// rescanSkippedCron runs between the rescan_pending runs (minute 5, 15, …).
	rescanSkippedCron   = "5-59/10 * * * *"
	rescanSkippedUnique = 9 * time.Minute
	// maxRescanFailures consecutive failures end a rescan run: the scanner
	// is most likely down, the next run starts again with those files.
	maxRescanFailures = 3
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

// scan runs the scanner on a file whose scan is pending, removes the
// metadata of a clean (or skipped) upload and stores only the rewritten
// bytes in the main bucket (05 §14.7; BE-059). Once a real scanner is wired
// it also rescans a file whose scan was skipped (it is in the main bucket
// already, rewritten): clean → clean, infected → taken out of use (BE-048).
//
// It is idempotent: a file that is gone, not active or no longer waiting
// for a scan is left alone. A transient error (storage, scanner timeout or
// outage) leaves the file as it was — never clean (fail closed); the task
// is retried and files.rescan_pending / files.rescan_skipped pick it up
// later. Content that cannot be rewritten rejects the file: it never
// reaches the main bucket.
func (s *service) scan(ctx context.Context, id uuid.UUID) error {
	f, err := s.d.Store.Get(ctx, id)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil
	case err != nil:
		return err
	case !s.scannable(f):
		return nil
	}
	m := f.meta()
	rescan := f.ScanStatus == ScanSkipped

	// A quarantined upload is read once: the scanner and the rewrite see the
	// same bytes, and they must still be the bytes completeUpload verified
	// (the presigned PUT URL stays valid for a while after the completion).
	quarantined := f.Bucket != s.d.Buckets.Files
	var content []byte
	if quarantined {
		content, err = s.readVerified(ctx, f)
		if reason, ok := isUnprocessable(err); ok {
			return s.rejectScanned(ctx, f, ScanPending, reason)
		}
		if err != nil {
			return fmt.Errorf("files: read %s from quarantine: %w", f.ID, err)
		}
	}
	v, err := s.d.Scanner.Scan(ctx, ScanObject{
		FileID: f.ID, Purpose: f.Purpose, MIME: m.MIME, Size: m.SizeBytes,
		Open: func(ctx context.Context) (io.ReadCloser, error) {
			if content != nil {
				return io.NopCloser(bytes.NewReader(content)), nil
			}
			rc, _, err := s.d.Objects.Open(ctx, f.Bucket, f.StorageKey)
			return rc, err
		},
	})
	if err != nil {
		return fmt.Errorf("files: scan %s with %s: %w", f.ID, s.d.Scanner.Name(), err)
	}

	var clean *sanitized
	switch v.Status {
	case ScanClean, ScanSkipped:
		if quarantined {
			out, err := s.clean.Sanitize(ctx, m.MIME, content, maxGrowth*f.Purpose.MaxBytes())
			if reason, ok := isUnprocessable(err); ok {
				return s.rejectScanned(ctx, f, v.Status, reason)
			}
			if err != nil {
				return fmt.Errorf("files: remove metadata of %s: %w", f.ID, err)
			}
			// Only the rewritten bytes reach the main bucket, never a copy of
			// the upload.
			err = s.d.Objects.PutObject(ctx, storage.PutInput{
				Bucket: s.d.Buckets.Files, Key: f.StorageKey, Body: bytes.NewReader(out.data), Size: int64(len(out.data)),
				ContentType: out.mime, ContentDisposition: Disposition(m.OriginalName, out.mime),
			})
			if err != nil {
				return fmt.Errorf("files: move %s out of quarantine: %w", f.ID, err)
			}
			clean = &out
		}
	case ScanInfected:
		return s.takeOutInfected(ctx, f, v)
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
		case cur.Status != StatusActive || cur.ScanStatus != f.ScanStatus:
			return nil
		}
		now := s.d.Clock.Now()
		cur.ScanStatus, cur.ScannedAt = v.Status, &now
		cur.Bucket = s.d.Buckets.Files
		if clean != nil { // the row describes the stored (rewritten) bytes
			sum, size := sha256.Sum256(clean.data), int64(len(clean.data))
			cur.SHA256, cur.SizeBytes, cur.DetectedMIME = sum[:], &size, &clean.mime
		}
		applied = true
		return s.d.Store.Update(ctx, cur)
	})
	if err != nil {
		return err
	}
	if gone && quarantined { // deleted while scanning: drop the copy we just made
		s.removeObjects(ctx, f)
		return nil
	}
	if applied && quarantined {
		if err := s.d.Objects.Delete(ctx, f.Bucket, f.StorageKey); err != nil {
			s.d.Log.LogAttrs(ctx, slog.LevelWarn, "quarantine copy not deleted (lifecycle rule removes it)",
				slog.String("file_id", f.ID.String()), slog.String("err", err.Error()))
		}
	}
	if applied {
		s.publishAfterScan(ctx, f) // BE-120: only a servable publishable file is copied
	}
	attrs := []slog.Attr{
		slog.String("file_id", f.ID.String()), slog.String("purpose", string(f.Purpose)),
		slog.String("scanner", s.d.Scanner.Name()), slog.String("scan_status", string(v.Status)),
		slog.Bool("metadata_removed", clean != nil), slog.Bool("rescan", rescan),
	}
	if clean != nil && clean.mime != m.MIME {
		attrs = append(attrs, slog.String("converted_to", clean.mime))
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "file scanned", attrs...)
	return nil
}

// scannable reports whether f waits for a scan: active and pending, or
// skipped while a real scanner is wired (files uploaded with the no-op
// scanner are rescanned, BE-048).
func (s *service) scannable(f File) bool {
	if f.Status != StatusActive {
		return false
	}
	return f.ScanStatus == ScanPending || (f.ScanStatus == ScanSkipped && s.d.Scanner.Real())
}

// errKeep vetoes the deletion of an infected row that must stay.
var errKeep = errors.New("files: keep the row")

// takeOutInfected takes a file with malware out of use (05 §14.7; BE-048):
//
//  1. its objects are deleted from both buckets first — malware is never
//     kept; a failure fails the task, which is retried;
//  2. one transaction records scan_status "infected", audits the failed
//     verification (files.upload.complete, failure, actor files.scan_file,
//     with the scanner and the signature) and writes file.rejected (O):
//     files notifies the uploader, the owning module drops its reference;
//  3. an unattached row is then deleted (02 §3.1 hard delete; audited as
//     files.file.delete). An attached row stays "infected" — never served
//     or attached again — until its owning module deletes it; a row that
//     cannot be deleted now is left to files.orphan_cleanup.
func (s *service) takeOutInfected(ctx context.Context, f File, v ScanVerdict) error {
	objects := [][2]string{{s.d.Buckets.Quarantine, f.StorageKey}, {s.d.Buckets.Files, f.StorageKey}}
	if f.PublicKey != nil && s.d.Public.Enabled() { // a skipped file published before its rescan (BE-120)
		objects = append(objects, [2]string{s.d.Public.Bucket, *f.PublicKey})
	}
	for _, o := range objects {
		if err := s.d.Objects.Delete(ctx, o[0], o[1]); err != nil {
			return fmt.Errorf("files: delete infected %s: %w", f.ID, err)
		}
	}
	var cur File
	var published *string // a public copy recorded after f was read
	applied := false
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		var err error
		cur, err = s.d.Store.Lock(ctx, f.ID)
		switch {
		case errors.Is(err, ErrNotFound):
			return nil
		case err != nil:
			return err
		case cur.Status != StatusActive || cur.ScanStatus != f.ScanStatus:
			return nil
		}
		now := s.d.Clock.Now()
		cur.ScanStatus, cur.ScannedAt = ScanInfected, &now
		if cur.PublicKey != nil && (f.PublicKey == nil || *f.PublicKey != *cur.PublicKey) {
			published = cur.PublicKey
		}
		cur.PublicKey = nil // files_public_key_chk: an infected file has no public copy
		if err := s.d.Store.Update(ctx, cur); err != nil {
			return err
		}
		applied = true
		return s.rejectedInTx(ctx, cur, RejectInfected, map[string]any{
			"error_code": ErrNotClean.Code(), "reason": string(RejectInfected), "scanner": s.d.Scanner.Name(),
			"signature": v.Signature, "rescan": f.ScanStatus == ScanSkipped,
		})
	})
	if err != nil || !applied {
		return err
	}
	if published != nil && s.d.Public.Enabled() {
		s.deletePublic(ctx, f.ID, *published)
	}
	// Alarm line (BE-058 turns it into a metric): no file name or content.
	s.d.Log.LogAttrs(ctx, slog.LevelWarn, "file infected: object deleted",
		slog.String("file_id", f.ID.String()), slog.String("purpose", string(f.Purpose)),
		slog.String("scanner", s.d.Scanner.Name()), slog.String("signature", v.Signature),
		slog.Bool("rescan", f.ScanStatus == ScanSkipped), slog.Bool("attached", cur.Attached()), slog.Bool("alarm", true))
	if cur.Attached() {
		return nil
	}
	err = s.remove(ctx, authz.SystemActor(ScanTaskName), auditDelete, f.ID, func(r File) error {
		if r.Attached() || r.ScanStatus != ScanInfected {
			return errKeep
		}
		return nil
	})
	switch {
	case err == nil, errors.Is(err, errKeep):
	default: // the verdict stands; orphan cleanup deletes the row later
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "infected file row not deleted (orphan cleanup removes it)",
			slog.String("file_id", f.ID.String()), slog.String("err", err.Error()))
	}
	return nil
}

// rejectedInTx audits the failed verification of cur as the scan task and
// writes file.rejected (O) in the transaction carried by ctx.
func (s *service) rejectedInTx(ctx context.Context, cur File, reason RejectReason, meta map[string]any) error {
	if err := s.d.Audit.Log(ctx, fileEvent(string(authz.FilesUploadComplete), audit.Failure,
		authz.SystemActor(ScanTaskName), cur, meta)); err != nil {
		return err
	}
	p := domainevent.FileRejected{FileID: cur.ID, UploaderID: cur.UploadedBy, Purpose: string(cur.Purpose), Reason: string(reason)}
	if cur.Attached() {
		p.AttachedModule, p.AttachedEntityID = *cur.AttachedModule, cur.AttachedEntityID
	}
	return outbox.Emit(ctx, s.d.Outbox, p, "file", cur.ID)
}

// readVerified reads the quarantined object of f and checks it against the
// size and SHA-256 completeUpload verified. A missing or changed object is
// unprocessable (the file is rejected); a storage failure is transient.
func (s *service) readVerified(ctx context.Context, f File) ([]byte, error) {
	if f.SizeBytes == nil {
		return nil, refuse("no_size", nil)
	}
	size := *f.SizeBytes
	rc, _, err := s.d.Objects.Open(ctx, f.Bucket, f.StorageKey)
	switch {
	case errors.Is(err, storage.ErrNotFound):
		return nil, refuse("object_missing", nil)
	case err != nil:
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, size+1))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if int64(len(data)) != size || subtle.ConstantTimeCompare(sum[:], f.SHA256) != 1 {
		return nil, refuse("content_changed", nil)
	}
	return data, nil
}

// rejectScanned marks a pending file rejected (its metadata could not be
// removed), deletes the quarantined object, audits the failed upload
// completion as the scan task and writes file.rejected (the uploader is
// notified, T-163). The row keeps the scan verdict (pending when the file
// was rejected before the scan). The task succeeds: a retry would fail the
// same way.
func (s *service) rejectScanned(ctx context.Context, f File, scan ScanStatus, reason string) error {
	applied := false
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		cur, err := s.d.Store.Lock(ctx, f.ID)
		switch {
		case errors.Is(err, ErrNotFound):
			return nil
		case err != nil:
			return err
		case cur.Status != StatusActive || cur.ScanStatus != ScanPending:
			return nil
		}
		cur.Status, cur.ScanStatus = StatusRejected, scan
		if scan != ScanPending {
			now := s.d.Clock.Now()
			cur.ScannedAt = &now
		}
		if err := s.d.Store.Update(ctx, cur); err != nil {
			return err
		}
		applied = true
		return s.rejectedInTx(ctx, cur, RejectUnprocessable, map[string]any{"error_code": ErrRejected.Code(), "reason": reason})
	})
	if err != nil {
		return err
	}
	if !applied {
		return nil
	}
	if err := s.d.Objects.Delete(ctx, f.Bucket, f.StorageKey); err != nil {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "rejected file not deleted (lifecycle rule removes it)",
			slog.String("file_id", f.ID.String()), slog.String("err", err.Error()))
	}
	s.d.Log.LogAttrs(ctx, slog.LevelWarn, "file rejected: metadata could not be removed",
		slog.String("file_id", f.ID.String()), slog.String("purpose", string(f.Purpose)), slog.String("reason", reason))
	return nil
}

// handleRejected is the file.rejected subscription of files
// (files.notify_rejected): the uploader gets a fixed in-app notice through
// OwnerNotifier (idempotent per file).
func (s *service) handleRejected(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.FileRejected](e)
	if err != nil {
		return queue.Permanent(err)
	}
	reason := RejectReason(p.Reason)
	if reason != RejectInfected && reason != RejectUnprocessable {
		return queue.Permanent(fmt.Errorf("files: file.rejected with unknown reason %q", p.Reason))
	}
	if p.UploaderID == nil { // server-generated: nobody uploaded it
		return nil
	}
	if s.d.Notifier == nil {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "rejected file: uploader not notified (no notifier wired)",
			slog.String("file_id", p.FileID.String()))
		return nil
	}
	return s.d.Notifier.NotifyFileRejected(ctx, RejectionNotice{
		FileID: p.FileID, UserID: *p.UploaderID, Purpose: Purpose(p.Purpose), Reason: reason,
	})
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
			switch err := s.remove(ctx, authz.SystemActor(OrphanCleanupJobName), string(authz.FilesOrphanPurge), f.ID, nil); {
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

// rescanSkippedJob is files.rescan_skipped (BE-048): the migration of the
// files uploaded while the no-op scanner marked them skipped. Each run scans
// up to RescanBatch of them in ID order, continuing after the last one of
// the previous run and starting over at the end. Without a real scanner it
// does nothing.
func (s *service) rescanSkippedJob() queue.Job {
	return queue.Job{
		Name: RescanSkippedJobName, Cron: rescanSkippedCron, Queue: queue.Low, Unique: rescanSkippedUnique,
		Handle: s.rescanSkipped,
	}
}

func (s *service) rescanSkipped(ctx context.Context) error {
	if !s.d.Scanner.Real() {
		return nil
	}
	s.rescanMu.Lock()
	defer s.rescanMu.Unlock()
	batch, err := s.d.Store.SkippedScans(ctx, s.rescanAfter, s.d.RescanBatch)
	if err != nil {
		return err
	}
	// A file that keeps failing is passed over (and retried on the next
	// round); a streak of failures stops the run before the streak, so the
	// next run tries those files first.
	cursor, streakFrom, streak, scanned, failed := s.rescanAfter, s.rescanAfter, 0, 0, 0
	stopped := false
	for _, f := range batch {
		if err := s.scan(ctx, f.ID); err != nil {
			if streak == 0 {
				streakFrom = cursor
			}
			streak++
			failed++
			s.d.Log.LogAttrs(ctx, slog.LevelWarn, "skipped file not rescanned, retried later",
				slog.String("file_id", f.ID.String()), slog.String("err", err.Error()))
			if streak >= maxRescanFailures {
				cursor, stopped = streakFrom, true
				break
			}
			cursor = f.ID
			continue
		}
		streak, cursor = 0, f.ID
		scanned++
	}
	if !stopped && len(batch) < s.d.RescanBatch { // end of the table: start over next time
		cursor = uuid.Nil()
	}
	s.rescanAfter = cursor
	if len(batch) == 0 {
		return nil
	}
	// Not an error: retrying the job at once would only hit a scanner that
	// is down; the next run (10 minutes) continues.
	level := slog.LevelInfo
	if stopped {
		level = slog.LevelWarn
	}
	s.d.Log.LogAttrs(ctx, level, "skipped files rescanned", slog.Int("scanned", scanned),
		slog.Int("failed", failed), slog.Bool("stopped", stopped))
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
		if p.Bucket != s.d.Buckets.Files && p.Bucket != s.d.Buckets.Quarantine && (!s.d.Public.Enabled() || p.Bucket != s.d.Public.Bucket) {
			return queue.Permanent(fmt.Errorf("files: delete object: unknown bucket %q", p.Bucket))
		}
		return s.d.Objects.Delete(ctx, p.Bucket, p.Key)
	}}
}
