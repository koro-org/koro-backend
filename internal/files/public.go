package files

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
	"koro/internal/platform/storage"
)

// Public media (BE-120). The images of public records — org logos and
// event covers — need URLs that outlive the 5-minute presigned download
// (storage.MaxGetTTL): koro-web caches its public pages, browsers and
// crawlers keep Open Graph images. Such a file is copied once, after its
// scan cleared it, into the public media bucket (anonymously readable, no
// listing; PublicMedia):
//
//	key            <purpose>/<file id>-<first 16 bytes of the SHA-256, hex><ext>
//	               content addressed: the bytes of a file never change after
//	               the scan, a new image is a new file and so a new URL; the
//	               random file ID keeps one object per file (a deletion
//	               never breaks another file's URL) and the URL unguessable
//	Cache-Control  public, max-age=31536000, immutable
//	Content-Type   the verified type; no Content-Disposition (an <img> or a
//	               crawler shows it; only re-encoded JPEG/PNG/WebP qualify)
//
// Who sees the URL is the owning module's decision (01 §4.2): orgs shows it
// for an active org that is not a sensitive club, events for a published
// (or completed) public event of such an org; everything else keeps the
// signed URL only. files never makes an unscanned, infected or rejected
// file public: the copy needs servable (clean, or skipped while
// FILES_SCAN_REQUIRED=false), an infected verdict deletes it, and so does
// every deletion of the row (removeObjects).
//
// The copy is made after the scan (scan → publish, best effort) and by the
// files.publish_public job (files cleared before BE-120, or while public
// media was off, or whose copy failed). The first copy of an attached file
// writes file.published (O) for the web's revalidation (webcache).
//
// A file its owning module keeps as evidence only (Retire, BE-180: a logo
// replaced while its org has an open report) loses the copy and never gets
// another; the signed URL stays for whoever the owner lets read it.
const (
	// PublishPublicJobName is the public media backfill (every 10 minutes,
	// minute 2, 12, …; low).
	PublishPublicJobName = "files.publish_public"
	publishCron          = "2-59/10 * * * *"
	publishUnique        = 9 * time.Minute

	// PublicCacheControl is the Cache-Control of every public copy.
	PublicCacheControl = "public, max-age=31536000, immutable"

	// auditPublish is the audit action of the public copy (system actor
	// files.publish_public; T-161 lists the module-call actions for 03 §3.7).
	auditPublish = "files.file.publish"

	// auditRetire is the audit action of Retire (a module call, BE-180).
	auditRetire = "files.file.retire"
)

// publicKeyRe is files_public_key_format_chk (000149).
var publicKeyRe = regexp.MustCompile(`^(org_logo|event_cover)/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}-[0-9a-f]{32}\.(jpg|png|webp)$`)

// publishablePurposes lists the purposes with a public copy (Unpublished).
func publishablePurposes() []Purpose {
	var out []Purpose
	for _, p := range UploadPurposes() {
		if p.Publishable() {
			out = append(out, p)
		}
	}
	return out
}

// publicKey is the content-addressed key of f's public copy.
func publicKey(f File) string {
	return fmt.Sprintf("%s/%s-%s%s", f.Purpose, f.ID, hex.EncodeToString(f.SHA256[:16]), extension(f.meta().MIME))
}

// PublicURL implements Files.
func (s *service) PublicURL(ctx context.Context, fileID uuid.UUID) (string, error) {
	if !s.d.Public.Enabled() {
		return "", nil
	}
	f, err := s.d.Store.Get(ctx, fileID)
	if err != nil {
		return "", err
	}
	if !f.Purpose.Publishable() {
		return "", ErrPurposeMismatch
	}
	if err := s.servable(f); err != nil {
		return "", err
	}
	if f.PublicKey == nil {
		return "", nil // the copy follows the scan or files.publish_public
	}
	return s.d.Public.BaseURL + "/" + *f.PublicKey, nil
}

// needsCopy reports whether f is due for a public copy.
func (s *service) needsCopy(f File) bool {
	return s.d.Public.Enabled() && f.Purpose.Publishable() && f.PublicKey == nil && f.RetiredAt == nil && len(f.SHA256) == 32 &&
		s.servable(f) == nil
}

// Retire implements Files. The audit record has no org scope: the file's
// org must not read from its own trail why its old logo was kept (a report
// on it is the moderators' matter, 03 Y7); the metadata names the org.
func (s *service) Retire(ctx context.Context, a authz.Actor, fileID uuid.UUID) error {
	if err := checkActor(a); err != nil {
		return err
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		f, err := s.d.Store.Lock(ctx, fileID)
		switch {
		case errors.Is(err, ErrNotFound):
			return nil
		case err != nil:
			return err
		case f.RetiredAt != nil:
			return nil
		}
		key := f.PublicKey
		now := s.d.Clock.Now()
		f.RetiredAt, f.PublicKey = &now, nil
		if err := s.d.Store.Update(ctx, f); err != nil {
			return err
		}
		meta := map[string]any{"public_copy": key != nil}
		if f.OrgID != nil {
			meta["org_id"] = f.OrgID.String()
		}
		ev := fileEvent(auditRetire, audit.Success, a, f, meta)
		ev.OrgID = nil
		if err := s.d.Audit.Log(ctx, ev); err != nil {
			return err
		}
		if key != nil && s.d.Public.Enabled() {
			db.AfterCommit(ctx, func(ctx context.Context) { s.deletePublic(ctx, f.ID, *key) })
		}
		return nil
	})
}

// publish makes the public copy of the file id when it is due (idempotent):
// the object is copied from the main bucket, then the row records the key
// in a transaction that re-reads it (gone, changed or published meanwhile
// → the copy is dropped unless it is the recorded one), audits the write
// and, for an attached file, writes file.published. It runs outside a
// transaction (object storage I/O).
func (s *service) publish(ctx context.Context, id uuid.UUID) error {
	f, err := s.d.Store.Get(ctx, id)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil
	case err != nil:
		return err
	case !s.needsCopy(f):
		return nil
	}
	key := publicKey(f)
	err = s.d.Objects.Copy(ctx, storage.CopyInput{
		SrcBucket: f.Bucket, SrcKey: f.StorageKey, DstBucket: s.d.Public.Bucket, DstKey: key,
		ContentType: f.meta().MIME, CacheControl: PublicCacheControl,
	})
	if err != nil {
		return fmt.Errorf("files: copy %s to public media: %w", f.ID, err)
	}
	drop := false
	sys := authz.SystemActor(PublishPublicJobName)
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		cur, err := s.d.Store.Lock(ctx, id)
		switch {
		case errors.Is(err, ErrNotFound):
			drop = true // deleted meanwhile: its removal did not know the copy
			return nil
		case err != nil:
			return err
		case cur.PublicKey != nil:
			drop = *cur.PublicKey != key
			return nil
		case !s.needsCopy(cur) || cur.StorageKey != f.StorageKey || !bytes.Equal(cur.SHA256, f.SHA256):
			drop = true
			return nil
		}
		cur.PublicKey = &key
		if err := s.d.Store.Update(ctx, cur); err != nil {
			return err
		}
		if err := s.d.Audit.Log(ctx, fileEvent(auditPublish, audit.Success, sys, cur, map[string]any{"attached": cur.Attached()})); err != nil {
			return err
		}
		if !cur.Attached() {
			return nil
		}
		return outbox.Emit(ctx, s.d.Outbox, domainevent.FilePublished{
			FileID: cur.ID, Purpose: string(cur.Purpose), AttachedModule: *cur.AttachedModule, AttachedEntityID: *cur.AttachedEntityID,
		}, "file", cur.ID)
	})
	if err != nil {
		// The copy may be another run's recorded one: it stays (unlisted,
		// unguessable) and the next run records it.
		return err
	}
	if drop {
		s.deletePublic(ctx, f.ID, key)
		return nil
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "file published",
		slog.String("file_id", f.ID.String()), slog.String("purpose", string(f.Purpose)))
	return nil
}

// publishAfterScan makes the public copy of a file the scan just cleared;
// a failure is logged and left to files.publish_public.
func (s *service) publishAfterScan(ctx context.Context, f File) {
	if !s.d.Public.Enabled() || !f.Purpose.Publishable() {
		return
	}
	if err := s.publish(ctx, f.ID); err != nil {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "public media copy failed, retried by "+PublishPublicJobName,
			slog.String("file_id", f.ID.String()), slog.String("err", err.Error()))
	}
}

// deletePublic removes a public copy; a failure is retried by the
// files.delete_object task.
func (s *service) deletePublic(ctx context.Context, fileID uuid.UUID, key string) {
	if err := s.d.Objects.Delete(ctx, s.d.Public.Bucket, key); err != nil {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "public media copy not deleted, retrying in a task",
			slog.String("file_id", fileID.String()), slog.String("err", err.Error()))
		s.d.Queue.AfterCommit(ctx, DeleteObjectTaskName, deleteObject{Bucket: s.d.Public.Bucket, Key: key},
			fileID.String()+":"+s.d.Public.Bucket)
	}
}

// publishPublicJob is files.publish_public (BE-120): every cleared
// publishable file without a public copy gets one.
func (s *service) publishPublicJob() queue.Job {
	return queue.Job{
		Name: PublishPublicJobName, Cron: publishCron, Queue: queue.Low, Unique: publishUnique,
		Handle: s.publishPending,
	}
}

func (s *service) publishPending(ctx context.Context) error {
	if !s.d.Public.Enabled() {
		return nil
	}
	scans := []ScanStatus{ScanClean}
	if !s.d.ScanRequired {
		scans = append(scans, ScanSkipped)
	}
	published, failed, after := 0, 0, uuid.Nil()
	for {
		batch, err := s.d.Store.Unpublished(ctx, scans, after, batchSize)
		if err != nil {
			return err
		}
		for _, f := range batch {
			after = f.ID
			if err := s.publish(ctx, f.ID); err != nil {
				// Not an error of the job: the next run tries again.
				failed++
				s.d.Log.LogAttrs(ctx, slog.LevelWarn, "public media copy failed, retried next run",
					slog.String("file_id", f.ID.String()), slog.String("err", err.Error()))
				continue
			}
			published++
		}
		if len(batch) < batchSize {
			break
		}
	}
	if published+failed > 0 {
		s.d.Log.LogAttrs(ctx, slog.LevelInfo, "public media backfill done", slog.Int("published", published), slog.Int("failed", failed))
	}
	return nil
}
