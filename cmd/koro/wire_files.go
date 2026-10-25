package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/files"
	"koro/internal/notify"
	"koro/internal/platform/config"
	"koro/internal/platform/db"
	"koro/internal/platform/httpx"
	"koro/internal/platform/queue"
	"koro/internal/platform/storage"
)

// filesWiring wires the files module (L2; 01 §4.2). Owner: BE-008, then
// BE-048 (ClamAV scanner), BE-113 (adapter selection). Only tickets of the
// files module edit this file (BE-001 R4).
//
// Seams (nil in production):
//
//	store    nil → files.NewGormStore over the app_rw pool
//	tx       nil → the app_rw pool's TxManager; tests preset db.NopTx
//	objects  nil → the STORAGE_PROVIDER adapter (objectStores: s3 from S3_*,
//	         or mem); tests preset storage.Mem
//	buckets  nil → S3_BUCKET_FILES / S3_BUCKET_QUARANTINE
//	scanner  nil → the FILES_SCANNER adapter (scanners: "clamav" builds
//	         files.ClamAV, clamd over TCP, BE-048; "noop"
//	         files.NoopScanner, scan_status "skipped")
//	cfg      tests only; nil → the files section of the loaded configuration
//
// Every file row write is audited through c.audit.svc (L1, BE-059).
//
// Late binding (read at call time): the uploader of a rejected file is
// notified through c.notify.svc (L3, built after files) by filesNotifier;
// FILES_CLAMAV_READY_CHECK=true adds clamd to /readyz.
type filesWiring struct {
	store   files.Store
	tx      db.TxManager
	objects storage.Objects
	buckets *files.Buckets
	scanner files.Scanner
	cfg     *config.Files

	// mod is the built module; svc is what higher layers adapt to their
	// own ports (Attach, Get, PresignGet, Delete, PutGenerated).
	mod files.Module
	svc files.Service
}

func (w *filesWiring) build(c *container) error {
	cfg := c.infra.cfg.Files
	if w.cfg != nil {
		cfg = *w.cfg
	}
	sc := c.infra.platform.Storage
	store := w.store
	if store == nil {
		store = files.NewGormStore(c.infra.db.ORM)
	}
	objects := w.objects
	if objects == nil {
		var err error
		if objects, err = objectStores(sc, c).pick("STORAGE_PROVIDER", sc.Provider); err != nil {
			return err
		}
	}
	buckets := files.Buckets{Files: sc.BucketFiles, Quarantine: sc.BucketQuarantine}
	if w.buckets != nil {
		buckets = *w.buckets
	}
	tx := w.tx
	if tx == nil {
		tx = c.infra.db.Tx
	}
	scanner := w.scanner
	if scanner == nil {
		var err error
		if scanner, err = scanners(cfg, c).pick("FILES_SCANNER", cfg.Scanner); err != nil {
			return err
		}
	}
	mod, err := files.New(files.Deps{
		Store: store, Objects: objects, Tx: tx, Outbox: c.infra.outbox, Queue: c.infra.queue,
		Idempotency: c.infra.idempotency, Clock: c.infra.clock, Log: c.infra.log.With("module", "files"),
		Buckets: buckets, Audit: c.audit.svc, Scanner: scanner, ScanRequired: cfg.ScanRequired, UploadURLTTL: cfg.UploadURLTTL,
		OrphanRetention: c.retention("files_orphan", 0, files.DefaultOrphanRetention),
		Notifier:        filesNotifier{c: c}, RescanBatch: cfg.RescanBatch,
	})
	if err != nil {
		return err
	}
	w.mod, w.svc = mod, mod.Service
	c.privacy = append(c.privacy, mod.Privacy)
	return nil
}

// objectStores is the selection point of STORAGE_PROVIDER (BE-113): one line
// per storage.Objects adapter; each passes objectsContract
// (internal/platform/storage/objects_contract_test.go).
func objectStores(sc config.Storage, c *container) adapters[storage.Objects] {
	return adapters[storage.Objects]{
		config.StorageProviderS3: func() (storage.Objects, error) {
			s3, err := storage.NewS3(storage.Config{
				Endpoint: sc.Endpoint, PresignEndpoint: sc.PresignEndpoint, Region: sc.Region,
				AccessKey: sc.AccessKey.Reveal(), SecretKey: sc.SecretKey.Reveal(),
				PathStyle: sc.PathStyle, Timeout: sc.Timeout, Clock: c.infra.clock,
			})
			if err != nil {
				return nil, fmt.Errorf("object storage: %w", err)
			}
			return s3, nil
		},
		config.StorageProviderMem: func() (storage.Objects, error) { return storage.NewMem(c.infra.clock), nil },
	}
}

// scanners is the selection point of FILES_SCANNER (BE-113): one line per
// files.Scanner adapter; each passes scannerContract
// (internal/files/scanner_contract_test.go). FILES_CLAMAV_READY_CHECK=true
// adds clamd to /readyz.
func scanners(cfg config.Files, c *container) adapters[files.Scanner] {
	return adapters[files.Scanner]{
		config.ScannerNoop: func() (files.Scanner, error) { return files.NoopScanner{}, nil },
		config.ScannerClamAV: func() (files.Scanner, error) {
			sc, err := files.NewClamAV(files.ClamAVConfig{
				Addr: cfg.ClamAVAddr, Timeout: cfg.ClamAVTimeout, MaxConns: cfg.ClamAVMaxConns,
				MaxBytes: cfg.ClamAVMaxBytes, Clock: c.infra.clock,
			})
			if err != nil {
				return nil, err
			}
			if cfg.ClamAVReadyCheck {
				c.readyChecks = append(c.readyChecks, httpx.Check{Name: "clamav", Run: sc.Ping})
			}
			c.shutdownHooks = append(c.shutdownHooks, sc.Close)
			return sc, nil
		},
	}
}

// tasks: file.uploaded → files.scan_file, file.rejected →
// files.notify_rejected; files.orphan_cleanup (04:40), files.rescan_pending
// (every 10 min) and files.rescan_skipped (every 10 min, minute 5); the
// files.delete_object retry task.
func (w *filesWiring) tasks(c *container) {
	w.mod.Subscribe(c.infra.subs)
	for _, j := range w.mod.Jobs() {
		c.infra.jobs.Schedule(j)
	}
	for _, t := range w.mod.Tasks() {
		c.infra.jobs.Handle(t)
	}
}

// routes: createUpload, completeUpload, deleteMyFile (04 §2.20).
func (w *filesWiring) routes(c *container, api huma.API) { files.Register(api, w.svc) }

// filesNotifier adapts notify (L3) to files.OwnerNotifier: a fixed,
// transactional in-app notice to the uploader (preference "security",
// which cannot be switched off). The text names no file; the data carries
// the file ID only. The dedupe key makes a repeated file.rejected delivery
// a no-op. notify is built after files, so the sender is read at call
// time; a missing sender fails the task (retried), it never drops the
// notice silently.
type filesNotifier struct{ c *container }

// Notification templates of files (internal/notify/templates/files).
const (
	templateFileInfected      = "files.file_infected"
	templateFileUnprocessable = "files.file_unprocessable"
)

func (n filesNotifier) NotifyFileRejected(ctx context.Context, r files.RejectionNotice) error {
	svc := n.c.notify.svc
	if svc == nil {
		return errors.New("files: notify is not wired")
	}
	tmpl := templateFileUnprocessable
	if r.Reason == files.RejectInfected {
		tmpl = templateFileInfected
	}
	user := r.UserID
	_, err := svc.Send(ctx, notify.Message{
		UserID: &user, Channel: notify.ChannelInApp, Category: notify.CategoryTransactional,
		Template: tmpl, Data: map[string]any{"file_id": r.FileID},
		DedupeKey: "files.rejected:" + r.FileID.String(),
	})
	if notify.IsPermanent(err) { // an invalid message never improves: do not retry
		return queue.Permanent(err)
	}
	return err
}
