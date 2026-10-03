package files

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/problem"
	"koro/internal/platform/storage"
	"koro/internal/platform/textx"
)

// maxNameRunes is files_original_name_chk (02 §3.1).
const maxNameRunes = 255

type service struct {
	d     Deps
	clean *sanitizer // metadata removal (sanitize.go)
}

// Audit actions of the file writes that have no HTTP operation of their own
// (the owning module calls Attach and Delete). They are not in 03 yet: T-161
// asks to list them in 03 §3.7 next to files.orphan.purge.
const (
	auditAttach = "files.file.attach"
	auditDelete = "files.file.delete"
)

// fileEvent is the audit event of a write on f by a (01 §4.1). Metadata
// holds IDs, kinds and sizes only: never the file name or content.
func fileEvent(action string, outcome audit.Outcome, a authz.Actor, f File, meta map[string]any) audit.Event {
	id := f.ID
	if meta == nil {
		meta = map[string]any{}
	}
	meta["purpose"] = string(f.Purpose)
	return audit.Event{
		Action: action, Outcome: outcome, OrgID: f.OrgID, ResourceType: authz.TypeFile, ResourceID: &id, Metadata: meta,
	}.By(a)
}

// checkActor accepts the actor of a module call (Attach, Delete): a user
// or a system job, never an anonymous or zero actor (the audit needs one).
func checkActor(a authz.Actor) error {
	switch {
	case a.Kind == authz.KindUser && a.UserID != uuid.Nil():
	case a.Kind == authz.KindSystem && a.SystemJob != "":
	default:
		return fmt.Errorf("files: the actor must be a user or a system job, got %s", a.Kind)
	}
	return nil
}

var _ Service = (*service)(nil)

// resource is the authz view of a file (03 §3.5: OrgID = files.org_id,
// OwnerUserID = uploaded_by, Attrs purpose).
func resource(f File) authz.Resource {
	return authz.Resource{
		Type: authz.TypeFile, ID: f.ID, OrgID: f.OrgID, OwnerUserID: f.UploadedBy,
		Attrs: map[string]string{"purpose": string(f.Purpose)},
	}
}

// storageKey is "<purpose>/<yyyy>/<mm>/<id>" (02 §3.1): server generated,
// never the original name.
func storageKey(p Purpose, at time.Time, id uuid.UUID) string {
	return fmt.Sprintf("%s/%04d/%02d/%s", p, at.Year(), int(at.Month()), id)
}

// invalid builds a 422 naming the offending request member.
func invalid(sentinel *problem.Error, location, message string) error {
	return sentinel.WithFields(problem.FieldError{Location: location, Message: message})
}

// cleanName returns the display name of an upload: the last path element,
// normalised (05 §9.5), at most 255 characters.
func cleanName(raw string) (string, error) {
	base := raw[strings.LastIndexAny(raw, `/\`)+1:]
	name, err := textx.NormalizeText(base)
	switch {
	case err != nil:
		return "", invalid(ErrInvalidUpload, "body.filename", "must be printable text")
	case name == "" || name == "." || name == "..":
		return "", invalid(ErrInvalidUpload, "body.filename", "must not be empty")
	case utf8.RuneCountInString(name) > maxNameRunes:
		return "", invalid(ErrInvalidUpload, "body.filename", "must be at most 255 characters")
	}
	return name, nil
}

// checkUpload applies the allow-list of the purpose (02 §3.1, 05 §14.7).
func checkUpload(in UploadRequest) (string, error) {
	r, ok := purposeRule[in.Purpose]
	if !ok || r.generated {
		return "", invalid(ErrInvalidUpload, "body.purpose", "is not an uploadable purpose")
	}
	if !in.Purpose.AllowsMIME(in.ContentType) {
		return "", invalid(ErrInvalidUpload, "body.content_type", "is not allowed for this purpose")
	}
	if in.SizeBytes < 1 {
		return "", invalid(ErrInvalidUpload, "body.size_bytes", "must be positive")
	}
	if in.SizeBytes > r.maxBytes {
		return "", invalid(ErrTooLarge, "body.size_bytes", fmt.Sprintf("must be at most %d bytes for this purpose", r.maxBytes))
	}
	switch {
	case in.OrgID != nil && *in.OrgID == uuid.Nil():
		return "", invalid(ErrInvalidUpload, "body.org_id", "must not be the nil UUID")
	case r.org == orgRequired && in.OrgID == nil:
		return "", invalid(ErrInvalidUpload, "body.org_id", "is required for this purpose")
	case r.org == "" && in.OrgID != nil:
		return "", invalid(ErrInvalidUpload, "body.org_id", "is not allowed for this purpose")
	}
	return cleanName(in.Filename)
}

// CreateUpload implements Files.
func (s *service) CreateUpload(ctx context.Context, a authz.Actor, in UploadRequest) (Upload, error) {
	name, err := checkUpload(in)
	if err != nil {
		return Upload{}, err
	}
	uploader := a.UserID
	draft := File{Purpose: in.Purpose, OrgID: in.OrgID, UploadedBy: &uploader}
	if err := authz.Can(ctx, a, authz.FilesUploadCreate, resource(draft)); err != nil {
		return Upload{}, err
	}
	hash, err := idempotency.Hash(struct {
		Purpose     Purpose
		Name, Type  string
		Size        int64
		Org, Person *uuid.UUID
	}{in.Purpose, name, in.ContentType, in.SizeBytes, in.OrgID, &uploader})
	if err != nil {
		return Upload{}, err
	}
	out, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (Upload, error) {
		now := s.d.Clock.Now()
		f := draft
		f.ID = uuid.NewV7()
		f.Bucket, f.StorageKey = s.d.Buckets.Quarantine, storageKey(in.Purpose, now, f.ID)
		f.OriginalName, f.DeclaredMIME, f.SizeBytes = &name, in.ContentType, &in.SizeBytes
		f.Status, f.ScanStatus = StatusUploading, ScanPending
		f.CreatedAt, f.UpdatedAt = now, now
		if in.Purpose.Personal() {
			f.OwnerUserID = &uploader
		}
		if err := s.d.Store.Insert(ctx, f); err != nil {
			return Upload{}, err
		}
		ev := fileEvent(string(authz.FilesUploadCreate), audit.Success, a, f,
			map[string]any{"mime": in.ContentType, "size_bytes": in.SizeBytes})
		if err := s.d.Audit.Log(ctx, ev); err != nil {
			return Upload{}, err
		}
		// Presigning is a local computation (no request to the storage).
		p, err := s.d.Objects.PresignPut(ctx, storage.PresignPutInput{
			Bucket: f.Bucket, Key: f.StorageKey, ContentType: in.ContentType, ContentLength: in.SizeBytes,
			TTL: s.d.UploadURLTTL,
		})
		if err != nil {
			return Upload{}, fmt.Errorf("files: presign upload: %w", err)
		}
		return Upload{FileID: f.ID, UploadURL: p.URL, UploadHeaders: p.Headers, ExpiresAt: p.ExpiresAt}, nil
	})
	if err != nil {
		return Upload{}, err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "file upload started",
		slog.String("file_id", out.FileID.String()), slog.String("purpose", string(in.Purpose)))
	return out, nil
}

// CompleteUpload implements Files.
func (s *service) CompleteUpload(ctx context.Context, a authz.Actor, fileID uuid.UUID, sum []byte) (FileMeta, error) {
	f, err := s.d.Store.Get(ctx, fileID)
	if err != nil {
		return FileMeta{}, err
	}
	if err := authz.Can(ctx, a, authz.FilesUploadComplete, resource(f)); err != nil {
		return FileMeta{}, err
	}
	if len(sum) != sha256.Size {
		return FileMeta{}, invalid(ErrChecksumMismatch, "body.sha256", "must be a SHA-256 digest")
	}
	if f.Status != StatusUploading {
		return completed(f, sum)
	}

	// Verify the object outside any transaction (05 §6.2: no I/O in a tx).
	detected, size, err := s.verify(ctx, a, f, sum)
	if err != nil {
		return FileMeta{}, err
	}

	var out File
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		cur, err := s.d.Store.Lock(ctx, fileID)
		if err != nil {
			return err
		}
		if cur.Status != StatusUploading { // a concurrent completion won
			out = cur
			return nil
		}
		cur.Status, cur.SHA256, cur.DetectedMIME, cur.SizeBytes = StatusActive, sum, &detected, &size
		if err := s.d.Store.Update(ctx, cur); err != nil {
			return err
		}
		aev := fileEvent(string(authz.FilesUploadComplete), audit.Success, a, cur,
			map[string]any{"mime": detected, "size_bytes": size})
		if err := s.d.Audit.Log(ctx, aev); err != nil {
			return err
		}
		ev, err := domainevent.New(domainevent.FileUploaded{FileID: cur.ID}, "file", cur.ID)
		if err != nil {
			return err
		}
		out = cur
		return s.d.Outbox.AfterCommit(ctx, ev)
	})
	if err != nil {
		return FileMeta{}, err
	}
	if out.Status == StatusActive && !bytes.Equal(out.SHA256, sum) {
		return completed(out, sum)
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "file upload completed",
		slog.String("file_id", out.ID.String()), slog.String("purpose", string(out.Purpose)))
	return out.meta(), nil
}

// completed answers a completion of a file that is no longer uploading:
// the same checksum is a retry (idempotent), another one a conflict, a
// rejected file ErrRejected. Once the worker rewrote the object (metadata
// removal) the row holds the checksum of the rewritten bytes, so a late
// retry gets the conflict.
func completed(f File, sum []byte) (FileMeta, error) {
	if f.Status == StatusRejected {
		return FileMeta{}, ErrRejected
	}
	if f.Status == StatusActive && subtle.ConstantTimeCompare(f.SHA256, sum) == 1 {
		return f.meta(), nil
	}
	return FileMeta{}, ErrNotUploading
}

// verify checks the uploaded object of f: size (≤ purpose limit and equal to
// the declared one), SHA-256 (equal to the client's) and magic bytes (an
// allowed type of the purpose, equal to the declared type). A failed check
// deletes the object (05 §14.7: "başarısızsa sil") and is audited as a
// failed files.upload.complete; the row stays "uploading" and orphan
// cleanup removes it.
func (s *service) verify(ctx context.Context, a authz.Actor, f File, sum []byte) (detected string, size int64, err error) {
	declared := int64(0)
	if f.SizeBytes != nil {
		declared = *f.SizeBytes
	}
	info, err := s.d.Objects.Stat(ctx, f.Bucket, f.StorageKey)
	switch {
	case errors.Is(err, storage.ErrNotFound):
		return "", 0, ErrObjectMissing
	case err != nil:
		return "", 0, ErrStorageUnavailable.Wrap(err)
	case info.Size > f.Purpose.MaxBytes():
		return "", 0, s.reject(ctx, a, f, invalid(ErrTooLarge, "object", "uploaded object exceeds the purpose limit"))
	case info.Size != declared:
		return "", 0, s.reject(ctx, a, f, ErrSizeMismatch)
	}

	rc, _, err := s.d.Objects.Open(ctx, f.Bucket, f.StorageKey)
	switch {
	case errors.Is(err, storage.ErrNotFound):
		return "", 0, ErrObjectMissing
	case err != nil:
		return "", 0, ErrStorageUnavailable.Wrap(err)
	}
	defer rc.Close()
	h := sha256.New()
	head := &prefix{max: SniffLen}
	n, err := io.Copy(io.MultiWriter(h, head), io.LimitReader(rc, declared+1))
	switch {
	case err != nil:
		return "", 0, ErrStorageUnavailable.Wrap(err)
	case n != declared:
		return "", 0, s.reject(ctx, a, f, ErrSizeMismatch)
	case subtle.ConstantTimeCompare(h.Sum(nil), sum) != 1:
		return "", 0, s.reject(ctx, a, f, invalid(ErrChecksumMismatch, "body.sha256", "does not match the uploaded object"))
	}
	detected = DetectMIME(head.buf)
	if detected == "" || !f.Purpose.AllowsMIME(detected) || detected != f.DeclaredMIME {
		return "", 0, s.reject(ctx, a, f, ErrContentMismatch)
	}
	return detected, n, nil
}

// reject deletes the quarantined object of a failed verification, audits
// the failed completion and returns err. A failed delete is logged: the
// quarantine lifecycle rule (1 day) and orphan cleanup remove the object
// later. A failed audit write is logged too: the 422 stands either way.
func (s *service) reject(ctx context.Context, a authz.Actor, f File, err error) error {
	if derr := s.d.Objects.Delete(ctx, f.Bucket, f.StorageKey); derr != nil {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "rejected upload not deleted",
			slog.String("file_id", f.ID.String()), slog.String("err", derr.Error()))
	}
	ev := fileEvent(string(authz.FilesUploadComplete), audit.Failure, a, f, map[string]any{"error_code": problemCode(err)})
	if aerr := s.d.Audit.Log(ctx, ev); aerr != nil {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "failed upload completion not audited",
			slog.String("file_id", f.ID.String()), slog.String("err", aerr.Error()))
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "file upload rejected",
		slog.String("file_id", f.ID.String()), slog.String("purpose", string(f.Purpose)), slog.String("reason", problemCode(err)))
	return err
}

func problemCode(err error) string {
	if p, ok := errors.AsType[*problem.Error](err); ok {
		return p.Code()
	}
	return "error"
}

// prefix keeps the first max bytes written to it.
type prefix struct {
	buf []byte
	max int
}

func (p *prefix) Write(b []byte) (int, error) {
	if room := p.max - len(p.buf); room > 0 {
		p.buf = append(p.buf, b[:min(room, len(b))]...)
	}
	return len(b), nil
}

// DeleteOwn implements Service (deleteMyFile).
func (s *service) DeleteOwn(ctx context.Context, a authz.Actor, fileID uuid.UUID) error {
	f, err := s.d.Store.Get(ctx, fileID)
	if err != nil {
		return err
	}
	if err := authz.Can(ctx, a, authz.FilesFileDeleteOwn, resource(f)); err != nil {
		return err
	}
	return s.remove(ctx, a, string(authz.FilesFileDeleteOwn), fileID, func(f File) error {
		if f.Attached() {
			return ErrAttached
		}
		return nil
	})
}

// Delete implements Files.
func (s *service) Delete(ctx context.Context, a authz.Actor, fileID uuid.UUID) error {
	if err := checkActor(a); err != nil {
		return err
	}
	return s.remove(ctx, a, auditDelete, fileID, nil)
}

// remove deletes the row in a transaction (joining the caller's), audits
// the deletion as action by a in it and removes the objects after the
// commit. guard may veto the deletion of the locked row.
func (s *service) remove(ctx context.Context, a authz.Actor, action string, fileID uuid.UUID, guard func(File) error) error {
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		f, err := s.d.Store.Lock(ctx, fileID)
		switch {
		case errors.Is(err, ErrNotFound) && guard == nil:
			return nil // idempotent system delete
		case err != nil:
			return err
		}
		if guard != nil {
			if err := guard(f); err != nil {
				return err
			}
		}
		if err := s.d.Store.Delete(ctx, fileID); err != nil {
			return err
		}
		ev := fileEvent(action, audit.Success, a, f, map[string]any{"status": string(f.Status), "attached": f.Attached()})
		if err := s.d.Audit.Log(ctx, ev); err != nil {
			return err
		}
		db.AfterCommit(ctx, func(ctx context.Context) { s.removeObjects(ctx, f) })
		return nil
	})
}

// removeObjects deletes the file's key from both buckets (a concurrent scan
// may be moving it); a failure is retried by the files.delete_object task.
func (s *service) removeObjects(ctx context.Context, f File) {
	for _, b := range []string{s.d.Buckets.Quarantine, s.d.Buckets.Files} {
		if err := s.d.Objects.Delete(ctx, b, f.StorageKey); err != nil {
			s.d.Log.LogAttrs(ctx, slog.LevelWarn, "file object not deleted, retrying in a task",
				slog.String("file_id", f.ID.String()), slog.String("err", err.Error()))
			s.d.Queue.AfterCommit(ctx, DeleteObjectTaskName, deleteObject{Bucket: b, Key: f.StorageKey},
				f.ID.String()+":"+b)
		}
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "file deleted",
		slog.String("file_id", f.ID.String()), slog.String("purpose", string(f.Purpose)))
}

// Get implements Files.
func (s *service) Get(ctx context.Context, fileID uuid.UUID) (FileMeta, error) {
	f, err := s.d.Store.Get(ctx, fileID)
	if err != nil {
		return FileMeta{}, err
	}
	return f.meta(), nil
}

// Attach implements Files.
func (s *service) Attach(ctx context.Context, a authz.Actor, fileID uuid.UUID, owner Owner) error {
	if !moduleRe.MatchString(owner.Module) || owner.EntityID == uuid.Nil() || !owner.Purpose.Valid() {
		return fmt.Errorf("files: attach: invalid owner %q/%s/%q", owner.Module, owner.EntityID, owner.Purpose)
	}
	if err := checkActor(a); err != nil {
		return err
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		f, err := s.d.Store.Lock(ctx, fileID)
		if err != nil {
			return err
		}
		switch {
		case f.Purpose != owner.Purpose:
			return ErrPurposeMismatch
		case f.Status == StatusRejected:
			return ErrRejected
		case f.Status != StatusActive:
			return ErrNotActive
		case f.ScanStatus == ScanInfected:
			return ErrNotClean
		case f.Attached() && *f.AttachedModule == owner.Module && *f.AttachedEntityID == owner.EntityID:
			return nil
		case f.Attached():
			return ErrAlreadyAttached
		}
		now := s.d.Clock.Now()
		f.AttachedModule, f.AttachedEntityID, f.AttachedAt = &owner.Module, &owner.EntityID, &now
		if err := s.d.Store.Update(ctx, f); err != nil {
			return err
		}
		return s.d.Audit.Log(ctx, fileEvent(auditAttach, audit.Success, a, f,
			map[string]any{"owner_module": owner.Module, "owner_entity_id": owner.EntityID.String()}))
	})
}

// PresignGet implements Files.
func (s *service) PresignGet(ctx context.Context, fileID uuid.UUID, ttl time.Duration) (Download, error) {
	if ttl == 0 {
		ttl = storage.MaxGetTTL
	}
	if ttl < 0 || ttl > storage.MaxGetTTL {
		return Download{}, fmt.Errorf("files: %w: %s", storage.ErrInvalidTTL, ttl)
	}
	f, err := s.d.Store.Get(ctx, fileID)
	if err != nil {
		return Download{}, err
	}
	if err := s.servable(f); err != nil {
		return Download{}, err
	}
	m := f.meta()
	p, err := s.d.Objects.PresignGet(ctx, storage.PresignGetInput{
		Bucket: f.Bucket, Key: f.StorageKey, TTL: ttl,
		ResponseContentDisposition: Disposition(m.OriginalName, m.MIME), ResponseContentType: m.MIME,
	})
	if err != nil {
		return Download{}, fmt.Errorf("files: presign download: %w", err)
	}
	// Access log (05 §14.7); the owning module writes the audit entry.
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "file download url issued",
		slog.String("file_id", f.ID.String()), slog.String("purpose", string(f.Purpose)), slog.Duration("ttl", ttl))
	return Download{URL: p.URL, ExpiresAt: p.ExpiresAt}, nil
}

// servable reports whether f may be downloaded (05 §14.7: an unscanned or
// unapproved object is never served).
func (s *service) servable(f File) error {
	if f.Status == StatusRejected {
		return ErrRejected
	}
	if f.Status != StatusActive {
		return ErrNotActive
	}
	switch {
	case f.ScanStatus == ScanClean:
	case f.ScanStatus == ScanSkipped && !s.d.ScanRequired:
	default:
		return ErrNotClean
	}
	if f.Bucket != s.d.Buckets.Files { // never sign a quarantined object
		return ErrNotClean
	}
	return nil
}

// Disposition returns the Content-Disposition of a download: always
// "attachment" (05 §14.7), the display name with the extension of the
// verified type (RFC 6266/2231 encoding for non-ASCII names).
func Disposition(name, mimeType string) string {
	ext := extension(mimeType)
	base := strings.TrimSuffix(name, filepath.Ext(name))
	switch got := strings.ToLower(filepath.Ext(name)); {
	case base == "":
		name = "file" + ext
	case got == ext, got == ".jpeg" && ext == ".jpg":
	default:
		name = base + ext
	}
	if v := mime.FormatMediaType("attachment", map[string]string{"filename": name}); v != "" {
		return v
	}
	return "attachment"
}

// PutGenerated implements Files.
func (s *service) PutGenerated(ctx context.Context, in GeneratedFile) (uuid.UUID, error) {
	if db.InTx(ctx) {
		return uuid.Nil(), errors.New("files: PutGenerated must run outside a transaction (object storage I/O)")
	}
	size := int64(len(in.Content))
	switch {
	case !in.Purpose.Valid():
		return uuid.Nil(), invalid(ErrInvalidUpload, "purpose", "unknown purpose")
	case !in.Purpose.AllowsMIME(in.ContentType):
		return uuid.Nil(), invalid(ErrInvalidUpload, "content_type", "is not allowed for this purpose")
	case size < 1 || size > in.Purpose.MaxBytes():
		return uuid.Nil(), invalid(ErrTooLarge, "content", "size out of range for this purpose")
	}
	name, err := cleanName(in.Name)
	if err != nil {
		return uuid.Nil(), err
	}
	now := s.d.Clock.Now()
	sum := sha256.Sum256(in.Content)
	f := File{
		ID: uuid.NewV7(), Purpose: in.Purpose, OwnerUserID: in.OwnerUserID, OrgID: in.OrgID,
		Bucket: s.d.Buckets.Files, OriginalName: &name, DeclaredMIME: in.ContentType, DetectedMIME: &in.ContentType,
		SizeBytes: &size, SHA256: sum[:], Status: StatusActive,
		// Server-generated content is trusted input: it is never scanned
		// (scanned_at stays NULL) and is served like a clean file.
		ScanStatus: ScanClean, CreatedAt: now, UpdatedAt: now,
	}
	f.StorageKey = storageKey(in.Purpose, now, f.ID)
	err = s.d.Objects.PutObject(ctx, storage.PutInput{
		Bucket: f.Bucket, Key: f.StorageKey, Body: bytes.NewReader(in.Content), Size: size,
		ContentType: in.ContentType, ContentDisposition: Disposition(name, in.ContentType),
	})
	if err != nil {
		return uuid.Nil(), ErrStorageUnavailable.Wrap(err)
	}
	if err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error { return s.d.Store.Insert(ctx, f) }); err != nil {
		s.removeObjects(ctx, f)
		return uuid.Nil(), err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "generated file stored",
		slog.String("file_id", f.ID.String()), slog.String("purpose", string(f.Purpose)))
	return f.ID, nil
}
