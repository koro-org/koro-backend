package main

import (
	"context"
	"encoding/json"
	"errors"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/kvkk"
	"koro/internal/platform/config"
	"koro/internal/platform/db"
	"koro/internal/platform/privacy"
	"koro/internal/platform/problem"
)

// kvkkWiring wires the kvkk module (L13; 01 §4.22). First owner: BE-021.
// Only tickets of the kvkk module edit this file (BE-001 R4).
//
// Seams (nil in production unless noted):
//
//	store   nil → kvkk.NewGormStore over the app_rw pool
//	tx      nil → the app_rw pool's TxManager; tests preset db.NopTx
//	cfg     tests only; nil → the kvkk section of the loaded configuration
//	reauth  the re-authentication verifier of the R steps; nil →
//	        identity's Reauthenticator (tests preset a fake)
//
// Consumed (all lower layers, built before kvkk): c.identity.svc (users,
// contact address, re-authentication), c.files.svc (export file),
// c.notify.svc (e-mails), c.audit.svc, hooks.talentAuditor (read at call
// time; nil → no profiling report) and c.privacy, read at call time so that
// every provider registered by any module is erased and exported. kvkk adds
// its own provider to c.privacy.
//
// c.sweepers (RetentionSweeper) is read by BE-049 (kvkk.retention_sweep).
type kvkkWiring struct {
	store  kvkk.Store
	tx     db.TxManager
	cfg    *config.Kvkk
	reauth func(ctx context.Context, userID uuid.UUID, password, totpCode string) (kind string, err error)

	mod   kvkk.Module
	svc   kvkk.Service // set by build
	files kvkk.Files   // c.files.svc, kept for the matrix fixtures (export file)
}

func (w *kvkkWiring) build(c *container) error {
	cfg := c.infra.cfg.Kvkk
	if w.cfg != nil {
		cfg = *w.cfg
	}
	store, tx := w.store, w.tx
	if store == nil {
		if c.infra.db == nil {
			return errors.New("kvkk: no store (database not opened)")
		}
		store = kvkk.NewGormStore(c.infra.db.ORM)
	}
	if tx == nil {
		tx = c.infra.db.Tx
	}
	if w.reauth == nil && c.identity.svc != nil {
		svc := c.identity.svc
		w.reauth = func(ctx context.Context, userID uuid.UUID, password, totpCode string) (string, error) {
			return svc.Verify(ctx, userID, identity.ReauthProof{Password: password, TOTPCode: totpCode})
		}
	}
	fs := c.files.svc
	w.files = fs
	mod, err := kvkk.New(kvkk.Deps{
		Store: store, Tx: tx, Outbox: c.infra.outbox, Audit: c.audit.svc, Idempotency: c.infra.idempotency,
		Clock: c.infra.clock, Log: c.infra.log.With("module", "kvkk"),
		Reauth: kvkkReauth{w: w}, Users: kvkkUsers{svc: c.identity.svc}, Files: fs,
		RemoveFile: func(ctx context.Context, a authz.Actor, id uuid.UUID) error { return fs.Delete(ctx, a, id) },
		Mailer:     c.notify.svc,
		Providers:  func() []privacy.Provider { return c.privacy },
		Talent:     kvkkTalent{c: c},
		DPOEmail:   cfg.DPOEmail, ExportRetention: cfg.ExportRetention,
	})
	if err != nil {
		return err
	}
	w.mod, w.svc = mod, mod.Service
	c.privacy = append(c.privacy, mod.Privacy)
	return nil
}

// tasks: dsr.submitted → kvkk.notify_dpo, dsr.erasure_approved →
// kvkk.erase_account, dsr.completed → kvkk.finish_request (critical);
// kvkk.dsr_deadlines (09:00, default) and kvkk.purge_exports (hourly, low).
func (w *kvkkWiring) tasks(c *container) {
	w.mod.Subscribe(c.infra.subs)
	for _, j := range w.mod.Jobs() {
		c.infra.jobs.Schedule(j)
	}
}

// routes: the 13 data subject request operations (04 §2.22); w.svc is nil
// under `koro openapi`, which Register tolerates.
func (w *kvkkWiring) routes(c *container, api huma.API) { kvkk.Register(api, w.svc, c.infra.clock) }

// kvkkReauth adapts kvkkWiring.reauth to kvkk.Reauthenticator (read at call
// time; nil fails closed).
type kvkkReauth struct{ w *kvkkWiring }

func (r kvkkReauth) Verify(ctx context.Context, userID uuid.UUID, p kvkk.ReauthProof) (string, error) {
	if r.w.reauth == nil {
		return "", kvkk.ErrReauthUnavailable
	}
	return r.w.reauth(ctx, userID, p.Password, p.TOTPCode)
}

// kvkkUsers adapts identity.Users to kvkk.Users.
type kvkkUsers struct{ svc identity.Service }

func (u kvkkUsers) Exists(ctx context.Context, id uuid.UUID) (bool, error) {
	if u.svc == nil {
		return false, problem.ErrUnavailable
	}
	_, err := u.svc.Get(ctx, id)
	switch {
	case errors.Is(err, identity.ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

func (u kvkkUsers) Contact(ctx context.Context, id uuid.UUID) (kvkk.Contact, bool, error) {
	if u.svc == nil {
		return kvkk.Contact{}, false, problem.ErrUnavailable
	}
	c, err := u.svc.Contact(ctx, id)
	switch {
	case errors.Is(err, identity.ErrNotFound):
		return kvkk.Contact{}, false, nil
	case err != nil:
		return kvkk.Contact{}, false, err
	}
	return kvkk.Contact{Email: c.Email, FirstName: c.FirstName, Locale: c.Locale}, true, nil
}

// kvkkTalent adapts hooks.talentAuditor (late bound by talent, BE-052).
type kvkkTalent struct{ c *container }

func (t kvkkTalent) Report(ctx context.Context, requestedBy, userID uuid.UUID, reason string) (json.RawMessage, error) {
	h := t.c.hooks.talentAuditor
	if h == nil || h.Report == nil {
		return nil, problem.ErrUnavailable
	}
	return h.Report(ctx, requestedBy, userID, reason)
}

// Available implements kvkk.TalentAuditor: no hook → no report section.
func (t kvkkTalent) Available() bool {
	h := t.c.hooks.talentAuditor
	return h != nil && h.Report != nil
}
