package files_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/files"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/problem"
	"koro/internal/platform/queue"
	"koro/internal/platform/storage"
)

var buckets = files.Buckets{Files: "files-test", Quarantine: "quarantine-test"}

// pdf is a valid one-page PDF without metadata.
var pdf = minimalPDF()

// png is a minimal object with PNG magic bytes (not a decodable image).
var png = append([]byte("\x89PNG\r\n\x1a\n"), []byte("not really an image")...)

// env is a files module over in-memory stores (05 §13.2). The audit log is
// the real audit service over its in-memory store, so every event is
// validated like in production.
type env struct {
	clk   *clock.Fake
	store files.Store
	mem   *storage.Mem
	out   *outbox.Mem
	q     *queue.Mem
	audit *audit.MemStore
	mod   files.Module
	svc   files.Service
}

func newEnv(t *testing.T, opts ...func(*files.Deps)) *env {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC))
	e := &env{clk: clk, store: files.NewMemStore(clk.Now), mem: storage.NewMem(clk), out: outbox.NewMem(clk), q: queue.NewMem(),
		audit: audit.NewMemStore()}
	d := files.Deps{
		Store: e.store, Objects: e.mem, Tx: db.NopTx{}, Outbox: e.out, Queue: e.q,
		Idempotency: idempotency.New(idempotency.Deps{Tx: db.NopTx{}, Store: idempotency.NewMemStore(), Clock: clk, Log: obs.Discard()}),
		Clock:       clk, Log: obs.Discard(), Buckets: buckets,
		Audit: audit.New(audit.Deps{Store: e.audit, Tx: db.NopTx{}, Clock: clk, Log: obs.Discard()}).Service,
	}
	for _, o := range opts {
		o(&d)
	}
	mod, err := files.New(d)
	if err != nil {
		t.Fatalf("files.New: %v", err)
	}
	e.mod, e.svc = mod, mod.Service
	return e
}

func actor(t *testing.T, name string) authz.Actor {
	t.Helper()
	p, ok := authztest.Get(name)
	if !ok {
		t.Fatalf("unknown persona %s", name)
	}
	return p.Actor
}

func digest(b []byte) []byte { s := sha256.Sum256(b); return s[:] }

func cvRequest(size int) files.UploadRequest {
	return files.UploadRequest{Purpose: files.PurposeCV, Filename: "CV.pdf", ContentType: files.MIMEPDF, SizeBytes: int64(size)}
}

// upload starts an upload as persona and PUTs content (the browser).
func (e *env) upload(t *testing.T, persona string, req files.UploadRequest, content []byte) files.Upload {
	t.Helper()
	up, err := e.svc.CreateUpload(t.Context(), actor(t, persona), req)
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	f := e.row(t, up.FileID)
	if content != nil {
		e.mem.Upload(f.Bucket, f.StorageKey, content, req.ContentType)
	}
	return up
}

// events returns the audit records written so far, oldest first.
func (e *env) events(t *testing.T) []audit.Record {
	t.Helper()
	recs, err := e.audit.List(t.Context(), audit.Filter{}, nil, 1000)
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(recs) // List returns the newest first
	return recs
}

// scanned uploads content with req as the student persona, completes it and
// runs the scan; it returns the row and the object in the main bucket.
func (e *env) scanned(t *testing.T, req files.UploadRequest, content []byte) (files.File, []byte) {
	t.Helper()
	req.SizeBytes = int64(len(content))
	id := e.active(t, req, content)
	if err := e.scan(t, id); err != nil {
		t.Fatalf("scan: %v", err)
	}
	f := e.row(t, id)
	data, _, _, _ := e.mem.Object(buckets.Files, f.StorageKey)
	return f, data
}

// active uploads and completes a file of the student persona.
func (e *env) active(t *testing.T, req files.UploadRequest, content []byte) uuid.UUID {
	t.Helper()
	up := e.upload(t, "student", req, content)
	if _, err := e.svc.CompleteUpload(t.Context(), actor(t, "student"), up.FileID, digest(content)); err != nil {
		t.Fatalf("CompleteUpload: %v", err)
	}
	return up.FileID
}

func (e *env) row(t *testing.T, id uuid.UUID) files.File {
	t.Helper()
	f, err := e.store.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", id, err)
	}
	return f
}

// subscription returns the outbox subscription of the files module.
func (e *env) subscription(t *testing.T) outbox.Subscription {
	t.Helper()
	r := outbox.NewRegistry()
	e.mod.Subscribe(r)
	if err := r.Err(); err != nil {
		t.Fatal(err)
	}
	subs := r.For(domainevent.TopicFileUploaded)
	if len(subs) != 1 || subs[0].Name != files.ScanTaskName {
		t.Fatalf("subscriptions of file.uploaded = %+v", subs)
	}
	return subs[0]
}

// scan delivers file.uploaded for id to the scan handler.
func (e *env) scan(t *testing.T, id uuid.UUID) error {
	t.Helper()
	ev, err := domainevent.New(domainevent.FileUploaded{FileID: id}, "file", id)
	if err != nil {
		t.Fatal(err)
	}
	return e.subscription(t).Handle(t.Context(), ev)
}

func (e *env) job(t *testing.T, name string) queue.Job {
	t.Helper()
	for _, j := range e.mod.Jobs() {
		if j.Name == name {
			return j
		}
	}
	t.Fatalf("no job %s", name)
	return queue.Job{}
}

func wantErr(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

// fakeScanner returns a fixed verdict and records whether it read the object.
type fakeScanner struct {
	verdict files.ScanVerdict
	err     error
	read    []byte
}

func (s *fakeScanner) Name() string { return "fake" }
func (s *fakeScanner) Real() bool   { return true }
func (s *fakeScanner) Scan(ctx context.Context, obj files.ScanObject) (files.ScanVerdict, error) {
	if s.err != nil {
		return files.ScanVerdict{}, s.err
	}
	rc, err := obj.Open(ctx)
	if err != nil {
		return files.ScanVerdict{}, err
	}
	defer rc.Close()
	s.read, _ = io.ReadAll(rc)
	return s.verdict, nil
}

// problemStatus returns the HTTP status of a problem error (0 otherwise).
func problemStatus(err error) int {
	if p, ok := errors.AsType[*problem.Error](err); ok {
		return p.Status()
	}
	return 0
}

// idempotencyCtx carries an Idempotency-Key as the HTTP middleware would.
func idempotencyCtx(t *testing.T, key string) context.Context {
	t.Helper()
	return idempotency.WithKey(t.Context(), idempotency.Key{Scope: "student:POST /files/uploads", Value: key, Status: 201})
}
