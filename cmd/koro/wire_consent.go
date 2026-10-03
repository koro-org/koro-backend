package main

import (
	"context"
	"errors"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/consent"
	"koro/internal/platform/config"
	"koro/internal/platform/db"
)

// consentWiring wires the consent module (L2; 01 §4.3). First owner: BE-012.
// Only tickets of the consent module edit this file (BE-001 R4).
//
// Seams (nil in production unless noted):
//
//	store   nil → consent.NewGormStore over the app_rw pool
//	tx      nil → the app_rw pool's TxManager; tests preset db.NopTx
//	iys     nil → consent.NoopIYS (submissions "skipped"); BE-055 sets the
//	        real İYS adapter here
//	cfg     tests only; nil → the consent section of the loaded configuration
//	reauth  the re-authentication verifier of the R operations (03 §6.2:
//	        identity.Reauth.Verify). identity is L4 and builds after
//	        consent, so the binding is late: BE-014 sets c.consent.reauth
//	        in wire_identity.go build, only when it is nil (tests preset a
//	        fake). The type uses stdlib types only (R4 hook rule). nil →
//	        every R operation fails closed (503 consent.reauth_unavailable).
//
// Consumers adapt svc to their own ports: consent.Recorder (identity
// registration, students privacy settings, events form consent fields:
// "// sync-write: consent.Recorder.Record", 01 §6.3), consent.Checker
// (notify's commercial message port, BE-015; outreach dispatch) and
// consent.Notices (identity registration shows the current privacy notice).
type consentWiring struct {
	store  consent.Store
	tx     db.TxManager
	iys    consent.IYSClient
	cfg    *config.Consent
	reauth func(ctx context.Context, userID uuid.UUID, password, totpCode string) (kind string, err error)

	mod consent.Module
	svc consent.Service // set by build
}

// lateReauth adapts consentWiring.reauth to consent.Reauthenticator and
// reads it at call time (see consentWiring).
type lateReauth struct{ w *consentWiring }

func (l lateReauth) Verify(ctx context.Context, userID uuid.UUID, p consent.ReauthProof) (string, error) {
	if l.w.reauth == nil {
		return "", consent.ErrReauthUnavailable
	}
	return l.w.reauth(ctx, userID, p.Password, p.TOTPCode)
}

func (w *consentWiring) build(c *container) error {
	cfg := c.infra.cfg.Consent
	if w.cfg != nil {
		cfg = *w.cfg
	}
	store, tx := w.store, w.tx
	if store == nil {
		if c.infra.db == nil {
			return errors.New("consent: no store (database not opened)")
		}
		store = consent.NewGormStore(c.infra.db.ORM)
	}
	if tx == nil {
		tx = c.infra.db.Tx
	}
	mod, err := consent.New(consent.Deps{
		Store: store, Tx: tx, Outbox: c.infra.outbox, Audit: c.audit.svc, Idempotency: c.infra.idempotency,
		Clock: c.infra.clock, Log: c.infra.log.With("module", "consent"),
		Reauth: lateReauth{w: w}, IYS: w.iys, IYSRequired: cfg.IYSRequired,
	})
	if err != nil {
		return err
	}
	w.mod, w.svc = mod, mod.Service
	c.privacy = append(c.privacy, mod.Privacy)
	return nil
}

// tasks: consent.recorded → consent.submit_iys (critical); the
// consent.iys_reconcile job (05:30, default).
func (w *consentWiring) tasks(c *container) {
	w.mod.Subscribe(c.infra.subs)
	for _, j := range w.mod.Jobs() {
		c.infra.jobs.Schedule(j)
	}
}

// routes: the 10 consent operations (04 §2.5); w.svc is nil under
// `koro openapi`, which Register tolerates.
func (w *consentWiring) routes(c *container, api huma.API) { consent.Register(api, w.svc) }
