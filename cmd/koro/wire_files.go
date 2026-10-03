package main

import (
	"fmt"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/files"
	"koro/internal/platform/config"
	"koro/internal/platform/db"
	"koro/internal/platform/storage"
)

// filesWiring wires the files module (L2; 01 §4.2). Owner: BE-008, then
// BE-048 (ClamAV scanner). Only tickets of the files module edit this file
// (BE-001 R4).
//
// Seams (nil in production):
//
//	store    nil → files.NewGormStore over the app_rw pool
//	tx       nil → the app_rw pool's TxManager; tests preset db.NopTx
//	objects  nil → the S3 client from config (S3_*); tests preset storage.Mem
//	buckets  nil → S3_BUCKET_FILES / S3_BUCKET_QUARANTINE
//	scanner  nil → files.NoopScanner (scan_status "skipped"); BE-048 sets ClamAV here
//	cfg      tests only; nil → the files section of the loaded configuration
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
		s3, err := storage.NewS3(storage.Config{
			Endpoint: sc.Endpoint, PresignEndpoint: sc.PresignEndpoint, Region: sc.Region,
			AccessKey: sc.AccessKey.Reveal(), SecretKey: sc.SecretKey.Reveal(),
			PathStyle: sc.PathStyle, Timeout: sc.Timeout, Clock: c.infra.clock,
		})
		if err != nil {
			return fmt.Errorf("object storage: %w", err)
		}
		objects = s3
	}
	buckets := files.Buckets{Files: sc.BucketFiles, Quarantine: sc.BucketQuarantine}
	if w.buckets != nil {
		buckets = *w.buckets
	}
	tx := w.tx
	if tx == nil {
		tx = c.infra.db.Tx
	}
	mod, err := files.New(files.Deps{
		Store: store, Objects: objects, Tx: tx, Outbox: c.infra.outbox, Queue: c.infra.queue,
		Idempotency: c.infra.idempotency, Clock: c.infra.clock, Log: c.infra.log.With("module", "files"),
		Buckets: buckets, Scanner: w.scanner, ScanRequired: cfg.ScanRequired, UploadURLTTL: cfg.UploadURLTTL,
		OrphanRetention: c.retention("files_orphan", 0, files.DefaultOrphanRetention),
	})
	if err != nil {
		return err
	}
	w.mod, w.svc = mod, mod.Service
	c.privacy = append(c.privacy, mod.Privacy)
	return nil
}

// tasks: file.uploaded → files.scan_file; files.orphan_cleanup (04:40) and
// files.rescan_pending (every 10 min); the files.delete_object retry task.
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
