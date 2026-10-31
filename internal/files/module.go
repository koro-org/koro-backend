// Package files is the file core (01 §4.2; BE-008): file metadata, presigned
// upload and download URLs, the per-purpose MIME/size allow-list, the virus
// scan hook and orphan cleanup.
//
// Upload pipeline (05 §14.7, rapor §8.2):
//
//	createUpload   authz (03 k30) + allow-list → row "uploading" + presigned
//	               PUT into the QUARANTINE bucket (server key, signed type and length)
//	browser        PUT → quarantine bucket (the API never carries file bytes)
//	completeUpload size ≤ limit and = declared → SHA-256 = client's → magic bytes
//	               ∈ purpose allow-list and = declared type → row "active",
//	               scan "pending" → file.uploaded (A); a failed check deletes the object
//	worker         file.uploaded → read the quarantined object (SHA-256 must
//	               still match) → Scanner (FILES_SCANNER: ClamAV over clamd
//	               INSTREAM, package clamav; or NoopScanner → "skipped") →
//	               clean/skipped: remove the metadata (sanitize.go: images
//	               decoded and encoded again, PDFs rewritten) and PUT only the
//	               rewritten bytes into the MAIN bucket with Content-Type +
//	               Content-Disposition: attachment, delete the quarantine
//	               copy; a rewrite that fails rejects the file (status
//	               "rejected", object deleted, never served); infected:
//	               delete the objects, mark the row "infected", audit, then
//	               delete the unattached row (BE-048). Both failures write
//	               file.rejected (O): the uploader gets an in-app notice and
//	               the owning module drops its reference. A scanner error is
//	               no verdict: the file stays pending (fail closed) and is
//	               scanned again (task retry, files.rescan_pending)
//	rescan         files.rescan_skipped scans the files the no-op scanner
//	               skipped, in batches, once a real scanner is wired
//	download       the OWNING module checks authz, then files.PresignGet:
//	               ≤ 5 minutes, attachment, only clean (or skipped while
//	               FILES_SCAN_REQUIRED=false) objects of the main bucket
//	public media   (BE-120, public.go) a cleared org_logo / event_cover is
//	               copied once into the public media bucket under a
//	               content-addressed key with an immutable Cache-Control
//	               (after the scan; files.publish_public catches up);
//	               files.PublicURL returns its permanent URL and the OWNING
//	               module shows it only on public records (active org that
//	               is not a sensitive club, published public event). An
//	               attached file's first copy writes file.published (O): the
//	               web revalidates the owner's page
//
// Every write of a file row is audited in its transaction (audit.Logger):
// files.upload.create, files.upload.complete (also failed verifications),
// files.file.attach, files.file.publish, files.file.delete_own,
// files.file.delete and files.orphan.purge.
//
// What files does not do: decide who may read or attach a file (the owning
// module does, 01 §4.2, 03 §3.6).
package files

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/outbox"
	"koro/internal/platform/privacy"
	"koro/internal/platform/queue"
	"koro/internal/platform/storage"
)

// Files is the cross-module contract (01 §4.2). Consumers declare a port
// with the methods they use; cmd/koro binds this service to it.
type Files interface {
	// CreateUpload starts a browser upload (createUpload): the allow-list
	// and authz (03 k30) are checked, an "uploading" row is written and a
	// presigned PUT into the quarantine bucket is returned.
	CreateUpload(ctx context.Context, a authz.Actor, in UploadRequest) (Upload, error)
	// CompleteUpload verifies the uploaded object (size, SHA-256, magic
	// bytes; 422 on a mismatch), activates the row and queues the scan.
	// Repeating it with the same checksum returns the current state.
	CompleteUpload(ctx context.Context, a authz.Actor, fileID uuid.UUID, sha256 []byte) (FileMeta, error)
	// PutGenerated stores a server-generated file (export, PDF) directly in
	// the main bucket as active and clean. Call it outside a transaction.
	PutGenerated(ctx context.Context, in GeneratedFile) (uuid.UUID, error)
	// Attach binds the file to its owning record in the caller's
	// transaction (01 §6.3 "* → files.Files.Attach"): it stops being an
	// orphan. The caller has already checked that its actor a (a user or a
	// system job; written to the audit log) may use the file (uploader or
	// org, via Get). Attaching to the same owner again is a no-op; another
	// owner is ErrAlreadyAttached; a rejected file is ErrRejected.
	Attach(ctx context.Context, a authz.Actor, fileID uuid.UUID, owner Owner) error
	// Get returns the file's metadata or ErrNotFound.
	Get(ctx context.Context, fileID uuid.UUID) (FileMeta, error)
	// States returns the State of every file of ids that exists (one read
	// for a whole list; a missing file has no entry): what an owning module
	// shows of its records' evidence (BE-154, T-393).
	States(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]State, error)
	// PresignGet returns a download URL valid for ttl (0 → 5 minutes, at
	// most storage.MaxGetTTL) with Content-Disposition: attachment. Only an
	// active file whose scan is clean, or skipped while FILES_SCAN_REQUIRED
	// is false, is served (else ErrNotClean). The caller (owning module) has
	// checked authz and writes the access audit.
	PresignGet(ctx context.Context, fileID uuid.UUID, ttl time.Duration) (Download, error)
	// PublicURL returns the permanent public URL of a publishable file
	// (org_logo, event_cover; BE-120): content addressed (a new image is a
	// new file and so a new URL) and served with an immutable
	// Cache-Control. It is "" while there is no public copy yet (public
	// media off, or the copy follows the scan) and an error for a missing
	// file (ErrNotFound), another purpose (ErrPurposeMismatch) or a file
	// that may not be served (ErrRejected, ErrNotActive, ErrNotClean). The
	// caller shows it only on a public record (it decides, 01 §4.2).
	PublicURL(ctx context.Context, fileID uuid.UUID) (string, error)
	// Read returns the content of a file the owning module sends on (the
	// invoice PDF e-mail, BE-042) under the rule of PresignGet: only an
	// active file whose scan is clean (or skipped while FILES_SCAN_REQUIRED
	// is false) is read (else ErrNotClean); more than maxBytes is
	// ErrTooLarge. The caller has checked authz and writes any audit.
	Read(ctx context.Context, fileID uuid.UUID, maxBytes int64) ([]byte, FileMeta, error)
	// Delete removes the row and its objects (hard delete, 02 §3.1) on
	// behalf of a (a user or a system job; written to the audit log). The
	// owning module clears its reference first: a referenced row fails with
	// ErrReferenced. Inside a transaction the objects are removed after the
	// commit; an object that cannot be removed then is retried by the
	// files.delete_object task. A missing file is not an error.
	Delete(ctx context.Context, a authz.Actor, fileID uuid.UUID) error
}

// Service is the files service: the contract plus the HTTP-only operations.
type Service interface {
	Files
	// DeleteOwn is deleteMyFile: the uploader deletes an unattached file.
	DeleteOwn(ctx context.Context, a authz.Actor, fileID uuid.UUID) error
}

// Buckets names the two private buckets (05 §14.7).
type Buckets struct {
	Files      string // verified and scanned objects
	Quarantine string // browser uploads until they are scanned
}

// PublicMedia is the public media bucket (BE-120; S3_BUCKET_PUBLIC,
// S3_PUBLIC_BASE_URL): anonymously readable, without listing. The zero
// value turns public media off.
type PublicMedia struct {
	Bucket  string
	BaseURL string // the objects' URL prefix, without a trailing slash
}

// Enabled reports whether public media is configured.
func (p PublicMedia) Enabled() bool { return p.Bucket != "" }

// Deps are the dependencies of the files module. Everything but Scanner is
// required (New panics otherwise, 05 §3).
type Deps struct {
	Store       Store
	Objects     storage.Objects
	Tx          db.TxManager
	Outbox      outbox.Writer       // file.uploaded (mechanism A)
	Queue       queue.Enqueuer      // files.delete_object retries
	Idempotency *idempotency.Keeper // optional Idempotency-Key of createUpload
	Clock       clock.Clock
	Log         *slog.Logger
	Buckets     Buckets
	Audit       AuditLogger // every row write, in its transaction
	// Public is the public media bucket (BE-120); zero → off.
	Public PublicMedia

	// Scanner scans uploads. nil → NoopScanner (scan_status "skipped").
	// A scanner with a size limit (MaxBytes() int64, the ClamAV adapter)
	// must accept MaxScanBytes.
	Scanner Scanner
	// ScanRequired is FILES_SCAN_REQUIRED: downloads need a clean scan.
	// It needs a real scanner.
	ScanRequired bool
	// Notifier tells uploaders about rejected files (file.rejected). nil →
	// the rejection is only logged.
	Notifier OwnerNotifier
	// RescanBatch is FILES_RESCAN_BATCH (files.rescan_skipped); 0 →
	// DefaultRescanBatch.
	RescanBatch int
	// UploadURLTTL is FILES_UPLOAD_URL_TTL; 0 → storage.MaxPutTTL.
	UploadURLTTL time.Duration
	// OrphanRetention returns the files_orphan retention period
	// (kvkk.retention_policies, 02 §24.2). nil → DefaultOrphanRetention.
	OrphanRetention func(ctx context.Context) (time.Duration, error)
}

func (d *Deps) validate() error {
	var errs []error
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"Store", d.Store != nil}, {"Objects", d.Objects != nil}, {"Tx", d.Tx != nil},
		{"Outbox", d.Outbox != nil}, {"Queue", d.Queue != nil}, {"Idempotency", d.Idempotency != nil},
		{"Clock", d.Clock != nil}, {"Log", d.Log != nil}, {"Audit", d.Audit != nil},
		{"Buckets", d.Buckets.Files != "" && d.Buckets.Quarantine != "" && d.Buckets.Files != d.Buckets.Quarantine},
	} {
		if !c.ok {
			errs = append(errs, fmt.Errorf("files.Deps.%s is missing or invalid", c.name))
		}
	}
	if d.Scanner == nil {
		d.Scanner = NoopScanner{}
	}
	if d.ScanRequired && !d.Scanner.Real() {
		errs = append(errs, fmt.Errorf("files: FILES_SCAN_REQUIRED=true needs a real scanner, got %q (FILES_SCANNER=clamav)", d.Scanner.Name()))
	}
	if l, ok := d.Scanner.(interface{ MaxBytes() int64 }); ok && l.MaxBytes() < MaxScanBytes() {
		errs = append(errs, fmt.Errorf("files: scanner %q accepts %d bytes, uploads need %d (FILES_CLAMAV_MAX_BYTES, clamd StreamMaxLength)",
			d.Scanner.Name(), l.MaxBytes(), MaxScanBytes()))
	}
	if d.RescanBatch <= 0 {
		d.RescanBatch = DefaultRescanBatch
	}
	if d.RescanBatch > batchSize {
		errs = append(errs, fmt.Errorf("files: rescan batch %d exceeds %d", d.RescanBatch, batchSize))
	}
	if d.UploadURLTTL <= 0 {
		d.UploadURLTTL = storage.MaxPutTTL
	}
	if d.UploadURLTTL > storage.MaxPutTTL {
		errs = append(errs, fmt.Errorf("files: upload URL lifetime %s exceeds %s", d.UploadURLTTL, storage.MaxPutTTL))
	}
	switch p := d.Public; {
	case (p.Bucket == "") != (p.BaseURL == ""), strings.HasSuffix(p.BaseURL, "/"):
		errs = append(errs, errors.New("files.Deps.Public needs both a bucket and a base URL without a trailing slash"))
	case p.Bucket != "" && (p.Bucket == d.Buckets.Files || p.Bucket == d.Buckets.Quarantine):
		errs = append(errs, errors.New("files: the public media bucket must differ from the files and quarantine buckets"))
	}
	if d.OrphanRetention == nil {
		d.OrphanRetention = func(context.Context) (time.Duration, error) { return DefaultOrphanRetention, nil }
	}
	return errors.Join(errs...)
}

// Module is what files hands to the composition root.
type Module struct {
	Service Service
	// Privacy is the KVKK provider (the user's unattached files).
	Privacy privacy.Provider

	svc *service
}

// New builds the files module. It returns an error for invalid
// dependencies (a missing one is a wiring bug, a scanner/flag mismatch a
// configuration error; both stop the process at start).
func New(d Deps) (Module, error) {
	if err := d.validate(); err != nil {
		return Module{}, err
	}
	s := &service{d: d, clean: newSanitizer()}
	return Module{Service: s, Privacy: privacyProvider{s: s}, svc: s}, nil
}

// Subscribe registers the outbox subscriptions of files (01 §6.2):
// file.uploaded → files.scan_file, file.rejected → files.notify_rejected.
func (m Module) Subscribe(r *outbox.Registry) {
	r.Subscribe(TopicScan, ScanTaskName, m.svc.handleUploaded, outbox.Queue(queue.Default))
	r.Subscribe(TopicRejected, NotifyRejectedTaskName, m.svc.handleRejected, outbox.Queue(queue.Default))
}

// Jobs returns the scheduled jobs of files (01 §7).
func (m Module) Jobs() []queue.Job {
	return []queue.Job{m.svc.orphanCleanupJob(), m.svc.rescanPendingJob(), m.svc.rescanSkippedJob(), m.svc.publishPublicJob()}
}

// MaxScanBytes is the largest object a scanner must accept: a skipped file
// is rescanned after its metadata removal, which may grow it up to twice
// the largest browser upload (sanitize.go maxGrowth).
func MaxScanBytes() int64 {
	var m int64
	for p, r := range purposeRule {
		if p.Uploadable() {
			m = max(m, r.maxBytes)
		}
	}
	return maxGrowth * m
}

// Tasks returns the task handlers of files (files.delete_object).
func (m Module) Tasks() []queue.Task { return []queue.Task{m.svc.deleteObjectTask()} }
