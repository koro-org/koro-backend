package main

import (
	"context"
	"errors"
	"fmt"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/consent"
	"koro/internal/identity"
	"koro/internal/platform/config"
	"koro/internal/platform/crypto"
	"koro/internal/platform/db"
)

// consentWiring wires the consent module (L2; 01 §4.3). First owner: BE-012.
// Only tickets of the consent module edit this file (BE-001 R4).
//
// Seams (nil in production unless noted):
//
//	store   nil → consent.NewGormStore over the app_rw pool
//	tx      nil → the app_rw pool's TxManager; tests preset db.NopTx
//	iys     nil → consent.IYSHTTP when the İYS settings are present
//	        (CONSENT_IYS_*, BE-055), else consent.NoopIYS (submissions
//	        "skipped"; CONSENT_IYS_REQUIRED refuses that). A client that
//	        also reads consent states (consent.IYSStatusReader; tests'
//	        FakeIYS) gets consent.iys_status_sync (BE-103);
//	        CONSENT_IYS_SYNC_MAX_AGE refuses to start without one
//	cfg     tests only; nil → the consent section of the loaded configuration
//	deriver nil → crypto.Deriver over CRYPTO_TOKEN_KEYS (the one-click
//	        unsubscribe tokens, BE-062); tests preset the shared test deriver
//	reauth  the re-authentication verifier of the R operations (03 §6.2:
//	        identity.Reauth.Verify). identity is L4 and builds after
//	        consent, so the binding is late: BE-014 sets c.consent.reauth
//	        in wire_identity.go build, only when it is nil (tests preset a
//	        fake). The type uses stdlib types only (R4 hook rule). nil →
//	        every R operation fails closed (503 consent.reauth_unavailable).
//
// The İYS recipient address (T-159) is resolved at delivery time from
// identity (L4, built after consent): iysRecipients reads c.identity.svc
// when called, so no identity code is touched here.
//
// Consumers adapt svc to their own ports: consent.Recorder (identity
// registration, students privacy settings, events form consent fields:
// "// sync-write: consent.Recorder.Record", 01 §6.3), consent.Checker
// (notify's commercial message port, BE-015; outreach dispatch) and
// consent.Notices (identity registration shows the current privacy notice).
// links (consent.UnsubscribeLinks, BE-062) is set only with
// CONSENT_UNSUBSCRIBE_BASE_URL: outreach mints each commercial e-mail
// recipient's link with it and refuses commercial e-mail while it is nil.
type consentWiring struct {
	store   consent.Store
	tx      db.TxManager
	iys     consent.IYSClient
	cfg     *config.Consent
	deriver *crypto.Deriver
	reauth  func(ctx context.Context, userID uuid.UUID, password, totpCode string) (kind string, err error)

	mod   consent.Module
	svc   consent.Service          // set by build
	links consent.UnsubscribeLinks // set by build; nil without CONSENT_UNSUBSCRIBE_BASE_URL
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

// iysRecipients resolves İYS recipients through identity's Users.Contact
// at call time (worker only). identity holds e-mail addresses only, so the
// sms and call channels have no recipient.
type iysRecipients struct{ c *container }

func (r iysRecipients) IYSRecipient(ctx context.Context, userID uuid.UUID, ch consent.IYSChannel) (string, error) {
	if ch != consent.IYSEmail {
		return "", fmt.Errorf("%w: identity holds no phone numbers (channel %s)", consent.ErrIYSRecipientGone, ch)
	}
	users := r.c.identity.svc
	if users == nil {
		return "", errors.New("consent: identity is not wired (İYS recipient)")
	}
	contact, err := users.Contact(ctx, userID)
	switch {
	case errors.Is(err, identity.ErrNotFound):
		return "", consent.ErrIYSRecipientGone // erased before the delivery (T-159)
	case err != nil:
		return "", err
	}
	return contact.Email, nil
}

func (w *consentWiring) build(c *container) error {
	cfg := c.infra.cfg.Consent
	if w.cfg != nil {
		cfg = *w.cfg
	}
	iys := w.iys
	if iys == nil && cfg.IYSEnabled() {
		client, err := consent.NewIYSHTTP(consent.IYSHTTPConfig{
			BaseURL: cfg.IYSBaseURL, Username: cfg.IYSUsername, Password: cfg.IYSPassword.Reveal(),
			IYSCode: cfg.IYSCode, BrandCode: cfg.IYSBrandCode, RecipientType: cfg.IYSRecipientType,
			Recipients: iysRecipients{c: c}, Timeout: cfg.IYSTimeout, Clock: c.infra.clock,
		})
		if err != nil {
			return err
		}
		iys = client
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
	deriver := w.deriver
	if deriver == nil {
		keys, err := crypto.ParseKeyring(c.infra.platform.Crypto.TokenKeys.Reveal())
		if err != nil {
			return fmt.Errorf("consent: CRYPTO_TOKEN_KEYS: %w", err)
		}
		if deriver, err = crypto.NewDeriver(keys); err != nil {
			return fmt.Errorf("consent: CRYPTO_TOKEN_KEYS: %w", err)
		}
	}
	mod, err := consent.New(consent.Deps{
		Store: store, Tx: tx, Outbox: c.infra.outbox, Audit: c.audit.svc, Idempotency: c.infra.idempotency,
		Clock: c.infra.clock, Log: c.infra.log.With("module", "consent"),
		Reauth: lateReauth{w: w}, IYS: iys, IYSRequired: cfg.IYSRequired, IYSSyncMaxAge: cfg.IYSSyncMaxAge,
		Unsubscribe: consent.UnsubscribeConfig{BaseURL: cfg.UnsubscribeBaseURL, TTL: cfg.UnsubscribeTTL, Deriver: deriver},
	})
	if err != nil {
		return err
	}
	w.mod, w.svc, w.links = mod, mod.Service, mod.Links
	c.privacy = append(c.privacy, mod.Privacy)
	c.sweepers = append(c.sweepers, mod.Sweeper)
	return nil
}

// tasks: consent.recorded → consent.submit_iys (critical); the
// consent.iys_reconcile job (05:30, default) and, with an İYS client that
// reads consent states, consent.iys_status_sync (hourly :20, default;
// BE-103: not with the HTTP adapter yet, T-354).
func (w *consentWiring) tasks(c *container) {
	w.mod.Subscribe(c.infra.subs)
	for _, j := range w.mod.Jobs() {
		c.infra.jobs.Schedule(j)
	}
}

// routes: the 12 consent operations (04 §2.5; BE-062: getUnsubscribeLink,
// confirmUnsubscribe); w.svc is nil under
// `koro openapi`, which Register tolerates.
func (w *consentWiring) routes(c *container, api huma.API) { consent.Register(api, w.svc) }
