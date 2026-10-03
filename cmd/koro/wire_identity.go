package main

import (
	"context"
	"errors"
	"fmt"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/identity"
	"koro/internal/platform/config"
	"koro/internal/platform/crypto"
	"koro/internal/platform/db"
	"koro/internal/platform/passwd"
	"koro/internal/platform/problem"
)

// identityWiring wires the identity module (L4; 01 §4.6). First owner: BE-014.
// Only tickets of the identity module edit this file (BE-001 R4).
//
// Seams (nil in production unless noted):
//
//	store    nil → identity.NewGormStore over the app_rw pool
//	tx       nil → the app_rw pool's TxManager; tests preset db.NopTx
//	deriver  nil → crypto.Deriver over CRYPTO_TOKEN_KEYS (e-mail tokens)
//	hasher   nil → passwd.Hasher over passwd (PASSWORD_PEPPER). Only `koro
//	         api` sets passwd (cmd_api.go): the worker never hashes, so
//	         there every password operation answers 503
//	passwd   the passwd configuration section of `koro api`
//	cfg      tests only; nil → the identity section of the configuration
//
// What identity provides to others, all set in build:
//
//	c.actors        the real ActorResolver (cmd/koro/actor.go), only when
//	                nil (the authz matrix presets its persona resolver)
//	c.consent.reauth the re-authentication verifier of consent's R
//	                operations (06 H-42), only when nil (tests preset a fake)
//	c.notify.platformAdmins the platform_admins in effect (06 H-33) for
//	                notify's admin alerts (06 H-40), only when nil
//	c.privacy       the identity KVKK provider (kvkk calls it last)
//
// Consumed: c.consent.svc (registration consents, privacy notice lookup),
// c.notify.svc (worker e-mails), c.audit.svc, hooks.memberships (read at
// call time, so orgs may fill it later) and hooks.retention.
type identityWiring struct {
	store   identity.Store
	tx      db.TxManager
	deriver *crypto.Deriver
	hasher  identity.PasswordHasher
	passwd  *config.Passwd
	cfg     *config.Identity

	mod identity.Module
	svc identity.Service // set by build
}

func (w *identityWiring) build(c *container) error {
	cfg := c.infra.cfg.Identity
	if w.cfg != nil {
		cfg = *w.cfg
	}
	store, tx := w.store, w.tx
	if store == nil {
		if c.infra.db == nil {
			return errors.New("identity: no store (database not opened)")
		}
		store = identity.NewGormStore(c.infra.db.ORM)
	}
	if tx == nil {
		tx = c.infra.db.Tx
	}
	deriver := w.deriver
	if deriver == nil {
		keys, err := crypto.ParseKeyring(c.infra.platform.Crypto.TokenKeys.Reveal())
		if err != nil {
			return fmt.Errorf("identity: CRYPTO_TOKEN_KEYS: %w", err)
		}
		if deriver, err = crypto.NewDeriver(keys); err != nil {
			return fmt.Errorf("identity: CRYPTO_TOKEN_KEYS: %w", err)
		}
	}
	hasher := w.hasher
	if hasher == nil && w.passwd != nil {
		h, err := passwd.New([]byte(w.passwd.Pepper.Reveal()), w.passwd.Concurrency)
		if err != nil {
			return fmt.Errorf("identity: %w", err)
		}
		hasher = h
	}
	d := identity.Deps{
		Store: store, Tx: tx, Outbox: c.infra.outbox, Queue: c.infra.queue, Audit: c.audit.svc,
		Clock: c.infra.clock, Log: c.infra.log.With("module", "identity"), Limiter: c.infra.limiter,
		Deriver: deriver, Consent: c.consent.svc, Notices: noticeLookup{svc: c.consent.svc}, Mailer: c.notify.svc,
		LinkBaseURL: cfg.LinkBaseURL,
		Memberships: func(ctx context.Context, userID uuid.UUID) ([]authz.Membership, error) {
			if h := c.hooks.memberships; h != nil {
				return h(ctx, userID)
			}
			return nil, nil
		},
		Hasher: hasher,
		// MFA and TOTP: BE-016 (MFA accounts answer 503 until then; none exist).
	}
	mod, err := identity.New(d)
	if err != nil {
		return err
	}
	w.mod, w.svc = mod, mod.Service
	c.privacy = append(c.privacy, mod.Privacy)
	if c.actors == nil {
		c.actors = actorResolver{auth: mod.Service, students: mod.Students, clock: c.infra.clock}
	}
	if c.notify.platformAdmins == nil { // 06 H-40 admin alerts (BE-015 hook)
		c.notify.platformAdmins = mod.PlatformAdmins
	}
	if c.consent.reauth == nil {
		svc := mod.Service
		c.consent.reauth = func(ctx context.Context, userID uuid.UUID, password, totpCode string) (string, error) {
			return svc.Verify(ctx, userID, identity.ReauthProof{Password: password, TOTPCode: totpCode})
		}
	}
	return nil
}

// tasks: identity.email_token_issued → identity.send_email_token; the
// identity.send_security_notice task; identity.sessions_cleanup (03:30)
// and identity.email_tokens_cleanup (03:35) on the low queue (01 §7).
func (w *identityWiring) tasks(c *container) {
	w.mod.Subscribe(c.infra.subs)
	for _, t := range w.mod.Tasks() {
		c.infra.jobs.Handle(t)
	}
	sessions := c.retention("session_expired", 0, identity.DefaultRetention)
	tokens := c.retention("email_token", 0, identity.DefaultRetention)
	for _, j := range w.mod.Jobs(identity.Retention(sessions), identity.Retention(tokens)) {
		c.infra.jobs.Schedule(j)
	}
}

// routes: the 14 identity operations of BE-014 (04 §2.1); w.svc is nil
// under `koro openapi`, which Register tolerates.
func (w *identityWiring) routes(c *container, api huma.API) { identity.Register(api, w.svc) }

// noticeLookup adapts consent.Service.GetNotice to identity.NoticeLookup:
// published and retired versions are found (anonymous reader), drafts are
// not.
type noticeLookup struct{ svc consent.Service }

func (n noticeLookup) NoticeByID(ctx context.Context, id uuid.UUID) (identity.NoticeInfo, error) {
	if n.svc == nil {
		return identity.NoticeInfo{}, problem.ErrUnavailable
	}
	v, err := n.svc.GetNotice(ctx, authz.Anonymous(), id)
	if err != nil {
		return identity.NoticeInfo{}, err
	}
	info := identity.NoticeInfo{
		ID: v.ID, Kind: v.Kind, Audience: v.Audience, Version: v.Version, BodySHA256: v.BodySHA256,
		Published: v.Status() == consent.StatusPublished,
	}
	if v.PurposeCode != nil {
		info.Purpose = *v.PurposeCode
	}
	return info, nil
}
