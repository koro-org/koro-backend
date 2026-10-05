package main

import (
	"errors"
	"fmt"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/audit"
	"koro/internal/platform/db"
	"koro/internal/platform/queue"
)

// auditWiring wires the audit module (L1; 01 §4.1). Owner: BE-007.
// Only tickets of the audit module edit this file (BE-001 R4).
//
// Seams (nil in production unless noted):
//
//	store    nil → the GORM store over c.infra.db (app_rw; the chain job
//	         switches to audit_chainer itself); tests preset audit.MemStore
//	signer   nil → Ed25519 keys from AUDIT_ROOT_SIGNING_KEYS; none in dev
//	         without keys (audit.daily_root then fails loudly)
//	archive  nil → audit.DirArchive at AUDIT_ROOT_ARCHIVE_DIR; none when
//	         unset (dev). S3 object lock is OPS work (H-02)
//
// Consumers: every module's service takes svc as its audit.Logger port
// ("// sync-write: audit.Logger.Log", 01 §6.3). ActorNames (display names)
// is bound once identity exists (BE-014). The retention sweeper of the log
// and its roots goes to c.sweepers (kvkk.retention_sweep; BE-057).
type auditWiring struct {
	store   audit.Store
	signer  audit.RootSigner
	archive audit.RootArchive

	svc  audit.Service // set by build
	jobs []queue.Job   // audit.chain, audit.daily_root, audit.verify_chain
}

func (w *auditWiring) build(c *container) error {
	store, tx := w.store, db.TxManager(db.NopTx{})
	if c.infra.db != nil {
		tx = c.infra.db.Tx
		if store == nil {
			store = audit.NewGormStore(c.infra.db.ORM)
		}
	}
	if store == nil {
		return errors.New("audit: no store (database not opened)")
	}

	cfg := c.infra.cfg.Audit
	signer := w.signer
	if signer == nil {
		keys, err := cfg.RootKeys()
		if err != nil {
			return fmt.Errorf("audit root keys: %w", err)
		}
		if len(keys) > 0 {
			s, err := audit.NewEd25519Signer(keys)
			if err != nil {
				return err
			}
			signer = s
		}
	}
	archive := w.archive
	if archive == nil && cfg.RootArchiveDir != "" {
		a, err := audit.NewDirArchive(cfg.RootArchiveDir)
		if err != nil {
			return err
		}
		archive = a
	}

	m := audit.New(audit.Deps{
		Store:       store,
		Tx:          tx,
		Clock:       c.infra.clock,
		Log:         c.infra.log.With("module", "audit"),
		Signer:      signer,
		Archive:     archive,
		Idempotency: c.infra.idempotency,
		// Alarm: nil → ERROR log with alarm=true until GlitchTip/DPO
		// notification is wired (BE-058).
	})
	w.svc, w.jobs = m.Service, m.Jobs
	c.sweepers = append(c.sweepers, m.Sweeper)
	return nil
}

// tasks schedules the three audit jobs of 01 §7.
func (w *auditWiring) tasks(c *container) {
	for _, j := range w.jobs {
		c.infra.jobs.Schedule(j)
	}
}

// routes registers the four audit operations (04 §2.21); w.svc is nil
// under `koro openapi`, which Register tolerates.
func (w *auditWiring) routes(c *container, api huma.API) { audit.Register(api, w.svc) }
