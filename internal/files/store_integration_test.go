//go:build integration

package files_test

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/files"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
	"koro/internal/platform/storage"
	"koro/internal/platform/storage/storagetest"
	"koro/internal/platform/testutil"
)

func TestMain(m *testing.M) { os.Exit(storagetest.Main(m, testutil.Run)) }

func TestStore_Contract_Gorm(t *testing.T) {
	t.Parallel()
	storeContract(t, func(t *testing.T) (files.Store, db.TxManager) {
		d := testutil.NewDB(t)
		return files.NewGormStore(d.ORM), d.Tx
	})
}

// realEnv is the files module over PostgreSQL and SeaweedFS.
type realEnv struct {
	clk   *clock.Fake
	d     *db.DB
	s3    *storage.S3
	store files.Store
	audit audit.Store
	mod   files.Module
	b     files.Buckets
}

func newRealEnv(t *testing.T) *realEnv {
	t.Helper()
	d := testutil.NewDB(t)
	s3 := storagetest.Client(t)
	clk := clock.NewFake(time.Now().UTC().Truncate(time.Second))
	e := &realEnv{clk: clk, d: d, s3: s3, store: files.NewGormStore(d.ORM), audit: audit.NewGormStore(d.ORM),
		b: files.Buckets{Files: storagetest.Bucket(t, s3), Quarantine: storagetest.Bucket(t, s3)}}
	mod, err := files.New(files.Deps{
		Store: e.store, Objects: s3, Tx: d.Tx, Outbox: outbox.NewMem(clk), Queue: queue.NewMem(),
		Idempotency: idempotency.New(idempotency.Deps{Tx: d.Tx, Store: idempotency.NewGormStore(d.ORM), Clock: clk, Log: obs.Discard()}),
		Clock:       clk, Log: obs.Discard(), Buckets: e.b,
		Audit: audit.New(audit.Deps{Store: e.audit, Tx: d.Tx, Clock: clk, Log: obs.Discard()}).Service,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.mod = mod
	return e
}

// browserUpload starts an upload and PUTs content with the presigned URL.
func (e *realEnv) browserUpload(t *testing.T, content []byte) files.Upload {
	t.Helper()
	up, err := e.mod.Service.CreateUpload(t.Context(), actor(t, "student"), cvRequest(len(content)))
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPut, up.UploadURL, bytes.NewReader(content))
	for k, v := range up.UploadHeaders {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("presigned PUT: status %d", resp.StatusCode)
	}
	return up
}

func (e *realEnv) scan(t *testing.T, id uuid.UUID) {
	t.Helper()
	r := outbox.NewRegistry()
	e.mod.Subscribe(r)
	ev, err := domainevent.New(domainevent.FileUploaded{FileID: id}, "file", id)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.For(files.TopicScan)[0].Handle(t.Context(), ev); err != nil {
		t.Fatalf("scan: %v", err)
	}
}

func (e *realEnv) exists(t *testing.T, bucket, key string) bool {
	t.Helper()
	_, err := e.s3.Stat(t.Context(), bucket, key)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		t.Fatal(err)
	}
	return err == nil
}

// TestFiles_SeaweedFS_UploadVerifyScanDownload runs the whole pipeline of
// 05 §14.7 against PostgreSQL and SeaweedFS.
func TestFiles_SeaweedFS_UploadVerifyScanDownload(t *testing.T) {
	t.Parallel()
	e := newRealEnv(t)
	svc := e.mod.Service

	// A tampered upload (PNG declared as PDF) is rejected and deleted.
	bad := e.browserUpload(t, png)
	_, err := svc.CompleteUpload(t.Context(), actor(t, "student"), bad.FileID, digest(png))
	wantErr(t, err, files.ErrContentMismatch)
	badRow, _ := e.store.Get(t.Context(), bad.FileID)
	if badRow.Status != files.StatusUploading || e.exists(t, e.b.Quarantine, badRow.StorageKey) {
		t.Fatalf("rejected upload: status %s, object kept %v", badRow.Status, e.exists(t, e.b.Quarantine, badRow.StorageKey))
	}

	cv := cvPDF(t) // with author, XMP and an EXIF/GPS photo (BE-059)
	up := e.browserUpload(t, cv)
	m, err := svc.CompleteUpload(t.Context(), actor(t, "student"), up.FileID, digest(cv))
	if err != nil || m.Status != files.StatusActive || m.ScanStatus != files.ScanPending {
		t.Fatalf("CompleteUpload = %+v, %v", m, err)
	}
	_, err = svc.PresignGet(t.Context(), up.FileID, time.Minute)
	wantErr(t, err, files.ErrNotClean)

	e.scan(t, up.FileID)
	f, _ := e.store.Get(t.Context(), up.FileID)
	if f.ScanStatus != files.ScanSkipped || f.Bucket != e.b.Files || e.exists(t, e.b.Quarantine, f.StorageKey) || !e.exists(t, e.b.Files, f.StorageKey) {
		t.Fatalf("after scan: %+v", f)
	}

	d, err := svc.PresignGet(t.Context(), up.FileID, 0)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(d.URL)
	if u.Query().Get("X-Amz-Expires") != "300" {
		t.Fatalf("download URL lifetime %s, want 300s", u.Query().Get("X-Amz-Expires"))
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, d.URL, nil)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Equal(digest(body), f.SHA256) ||
		!strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") || resp.Header.Get("Content-Type") != files.MIMEPDF {
		t.Fatalf("download: status %d, disposition %q, type %q", resp.StatusCode, resp.Header.Get("Content-Disposition"), resp.Header.Get("Content-Type"))
	}
	noSecrets(t, "downloaded CV", body)

	// The writes are in audit_events (same transaction, PostgreSQL CHECKs).
	recs, err := e.audit.List(t.Context(), audit.Filter{ResourceType: "file", ResourceID: &up.FileID}, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, r := range recs {
		actions = append(actions, r.Action+"/"+string(r.Outcome))
	}
	if len(recs) != 2 || recs[0].Action != "files.upload.complete" || recs[1].Action != "files.upload.create" {
		t.Fatalf("audit of the upload = %v", actions)
	}
	badRecs, err := e.audit.List(t.Context(), audit.Filter{ResourceType: "file", ResourceID: &bad.FileID}, nil, 10)
	if err != nil || len(badRecs) != 2 || badRecs[0].Outcome != audit.Failure {
		t.Fatalf("audit of the tampered upload = %+v, %v", badRecs, err)
	}
}

func TestOrphanCleanup_DeletesObjectAndRow(t *testing.T) {
	t.Parallel()
	e := newRealEnv(t)
	svc := e.mod.Service
	ctx := t.Context()

	// An abandoned upload (object in quarantine, never completed), a
	// completed but unattached file (object moved to the main bucket) and
	// an attached file.
	abandoned := e.browserUpload(t, pdf)
	loose := e.browserUpload(t, pdf)
	if _, err := svc.CompleteUpload(ctx, actor(t, "student"), loose.FileID, digest(pdf)); err != nil {
		t.Fatal(err)
	}
	e.scan(t, loose.FileID)
	kept := e.browserUpload(t, pdf)
	if _, err := svc.CompleteUpload(ctx, actor(t, "student"), kept.FileID, digest(pdf)); err != nil {
		t.Fatal(err)
	}
	if err := svc.Attach(ctx, actor(t, "student"), kept.FileID, files.Owner{Module: "students", EntityID: uuid.NewV7(), Purpose: files.PurposeCV}); err != nil {
		t.Fatal(err)
	}
	rows := map[uuid.UUID]files.File{}
	for _, id := range []uuid.UUID{abandoned.FileID, loose.FileID, kept.FileID} {
		f, err := e.store.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		rows[id] = f
	}

	// Younger than files_orphan (24 h): nothing happens.
	var job queue.Job
	for _, j := range e.mod.Jobs() {
		if j.Name == files.OrphanCleanupJobName {
			job = j
		}
	}
	if err := job.Handle(ctx); err != nil {
		t.Fatal(err)
	}
	for id := range rows {
		if _, err := e.store.Get(ctx, id); err != nil {
			t.Fatalf("young file %s deleted: %v", id, err)
		}
	}

	e.clk.Advance(files.DefaultOrphanRetention + time.Minute)
	if err := job.Handle(ctx); err != nil {
		t.Fatalf("orphan cleanup: %v", err)
	}
	for _, id := range []uuid.UUID{abandoned.FileID, loose.FileID} {
		if _, err := e.store.Get(ctx, id); !errors.Is(err, files.ErrNotFound) {
			t.Errorf("orphan row %s not deleted: %v", id, err)
		}
		key := rows[id].StorageKey
		if e.exists(t, e.b.Quarantine, key) || e.exists(t, e.b.Files, key) {
			t.Errorf("orphan object %s not deleted", key)
		}
	}
	if _, err := e.store.Get(ctx, kept.FileID); err != nil {
		t.Fatalf("attached file deleted: %v", err)
	}
	if !e.exists(t, rows[kept.FileID].Bucket, rows[kept.FileID].StorageKey) {
		t.Fatal("attached file's object deleted")
	}
}
