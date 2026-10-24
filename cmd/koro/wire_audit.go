package main

import (
	"errors"
	"fmt"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/audit"
	"koro/internal/platform/config"
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
//	signer   nil → Ed25519 keys from AUDIT_ROOT_SIGNING_KEYS (root, worker
//	         only); none in the API and in dev without keys
//	         (audit.daily_root then fails loudly)
//	verifier nil → Ed25519 public keys from AUDIT_ROOT_VERIFY_KEYS; none →
//	         the signer verifies (worker in dev), else verification of a
//	         range with roots fails (audit.signer_not_configured)
//	archive  nil → audit.DirArchive at AUDIT_ROOT_ARCHIVE_DIR (root); none
//	         in the API and when unset (dev). S3 object lock is OPS work (H-02)
//	root     the AuditRoot section of `koro worker` (cmd_worker.go). The API
//	         never sets it: it neither signs nor archives roots, so it gets
//	         no seed (BE-107, least privilege)
//
// Consumers: every module's service takes svc as its audit.Logger port
// ("// sync-write: audit.Logger.Log", 01 §6.3). ActorNames (display names)
// is bound once identity exists (BE-014). The retention sweeper of the log
// and its roots goes to c.sweepers (kvkk.retention_sweep; BE-057).
type auditWiring struct {
	store    audit.Store
	signer   audit.RootSigner
	verifier audit.RootVerifier
	archive  audit.RootArchive
	root     *config.AuditRoot

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

	verifier, signer, archive, err := w.rootKeys(c.infra.cfg.Audit)
	if err != nil {
		return err
	}

	m := audit.New(audit.Deps{
		Store:       store,
		Tx:          tx,
		Clock:       c.infra.clock,
		Log:         c.infra.log.With("module", "audit"),
		Signer:      signer,
		Verifier:    verifier,
		Archive:     archive,
		Idempotency: c.infra.idempotency,
		// Alarm: nil → ERROR log with alarm=true until GlitchTip/DPO
		// notification is wired (BE-058).
	})
	w.svc, w.jobs = m.Service, m.Jobs
	c.sweepers = append(c.sweepers, m.Sweeper)
	return nil
}

// rootKeys returns the verifier, signer and archive of the daily roots:
// the presets, else the configured ones. When the worker has both seeds and
// public keys, every seed must match its public key (a mismatch would make
// the API report the worker's roots as tampered).
func (w *auditWiring) rootKeys(cfg config.Audit) (audit.RootVerifier, audit.RootSigner, audit.RootArchive, error) {
	verifier, signer, archive := w.verifier, w.signer, w.archive
	var ev *audit.Ed25519Verifier
	if verifier == nil {
		keys, err := cfg.VerifyKeys()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("audit root verify keys: %w", err)
		}
		if len(keys) > 0 {
			if ev, err = audit.NewEd25519Verifier(keys); err != nil {
				return nil, nil, nil, err
			}
			verifier = ev
		}
	}
	if w.root == nil {
		return verifier, signer, archive, nil
	}
	if signer == nil {
		keys, err := w.root.RootKeys()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("audit root keys: %w", err)
		}
		if len(keys) > 0 {
			es, err := audit.NewEd25519Signer(keys)
			if err != nil {
				return nil, nil, nil, err
			}
			if ev != nil {
				if err := ev.CheckCovers(es); err != nil {
					return nil, nil, nil, err
				}
			}
			signer = es
		}
	}
	if archive == nil && w.root.RootArchiveDir != "" {
		a, err := audit.NewDirArchive(w.root.RootArchiveDir)
		if err != nil {
			return nil, nil, nil, err
		}
		archive = a
	}
	return verifier, signer, archive, nil
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
