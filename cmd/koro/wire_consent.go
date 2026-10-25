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
	"koro/internal/platform/iys"
	"koro/internal/platform/iys/iysfake"
	"koro/internal/platform/iys/official"
	"koro/internal/platform/obs"
)

// consentWiring wires the consent module (L2; 01 §4.3). First owner: BE-012.
// Only tickets of the consent module edit this file (BE-001 R4).
//
// Seams (nil in production unless noted):
//
//	store   nil → consent.NewGormStore over the app_rw pool
//	tx      nil → the app_rw pool's TxManager; tests preset db.NopTx
//	iys     nil → the İYS adapter CONSENT_IYS_PROVIDER names (iysAdapters:
//	        the one place adapters are chosen; BE-105): its Submit behind
//	        consent.NewIYSClient, its change feed and single-state reads
//	        (consent.IYSFeedReader, consent.IYSStateReader) when it has
//	        them; consent.NoopIYS with none (commercial grants refused,
//	        withdrawals "skipped"; CONSENT_IYS_REQUIRED refuses that).
//	        Tests preset FakeIYS (client and both readers). A reader gets
//	        consent.iys_status_sync (BE-103, BE-105);
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
// The İYS recipient address (T-159) is resolved at call time from
// identity (L4, built after consent) in both directions (delivery and
// status reads: user → address; the change feed: address → user):
// iysRecipients reads c.identity.svc when called, so no identity code is
// touched here.
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

// iysRecipients resolves İYS recipients for the adapters (their
// Recipients interfaces) over identity's Users (Contact, FindByEmail) at
// call time (worker only). identity holds e-mail addresses only, so the
// MESAJ and ARAMA types have no recipient.
type iysRecipients struct{ c *container }

var (
	_ official.Recipients = iysRecipients{}
	_ iysfake.Recipients  = iysRecipients{}
)

// Recipient returns the e-mail address of a live user.
func (r iysRecipients) Recipient(ctx context.Context, userID uuid.UUID, t iys.Type) (string, error) {
	if t != iys.Email {
		return "", fmt.Errorf("%w: identity holds no phone numbers (type %s)", iys.ErrRecipientGone, t)
	}
	users := r.c.identity.svc
	if users == nil {
		return "", errors.New("consent: identity is not wired (İYS recipient)")
	}
	contact, err := users.Contact(ctx, userID)
	switch {
	case errors.Is(err, identity.ErrNotFound):
		return "", iys.ErrRecipientGone // erased before the delivery (T-159)
	case err != nil:
		return "", err
	}
	return contact.Email, nil
}

// User returns the live account of an address (an erased account is no
// user any more).
func (r iysRecipients) User(ctx context.Context, t iys.Type, address string) (uuid.UUID, error) {
	if t != iys.Email {
		return uuid.Nil(), iys.ErrRecipientGone
	}
	users := r.c.identity.svc
	if users == nil {
		return uuid.Nil(), errors.New("consent: identity is not wired (İYS recipient)")
	}
	id, found, err := users.FindByEmail(ctx, address)
	switch {
	case err != nil:
		return uuid.Nil(), err
	case !found:
		return uuid.Nil(), iys.ErrRecipientGone
	}
	if _, err := users.Contact(ctx, id); errors.Is(err, identity.ErrNotFound) {
		return uuid.Nil(), iys.ErrRecipientGone
	} else if err != nil {
		return uuid.Nil(), err
	}
	return id, nil
}

// iysAdapters is the one place İYS adapters are chosen: one constructor
// per value of CONSENT_IYS_PROVIDER but none (config.IYSProviders;
// TestWire_EveryIYSProviderIsWired keeps them equal). A new adapter is a
// package of internal/platform/iys that passes iyscontract.Run, its
// settings in config.Consent and its line here; consent does not change.
var iysAdapters = map[string]func(c *container, cfg config.Consent) (consent.IYSSubmitter, error){
	config.IYSProviderFake: func(c *container, _ config.Consent) (consent.IYSSubmitter, error) {
		// Development only (config refuses it in staging and prod): İYS in
		// the worker's memory, with İYS's time rules and identity's
		// addresses.
		return iysfake.New(iysfake.Options{Clock: c.infra.clock, Recipients: iysRecipients{c: c}}), nil
	},
	config.IYSProviderOfficial: func(c *container, cfg config.Consent) (consent.IYSSubmitter, error) {
		a, err := official.New(official.Config{
			BaseURL: cfg.IYSBaseURL, Username: cfg.IYSUsername, Password: cfg.IYSPassword.Reveal(),
			IYSCode: cfg.IYSCode, BrandCode: cfg.IYSBrandCode, RecipientType: cfg.IYSRecipientType,
			Recipients: iysRecipients{c: c}, Timeout: cfg.IYSTimeout, Clock: c.infra.clock,
			Log: c.infra.log.With("module", "consent", "iys", "official"),
		})
		if err != nil {
			return nil, err
		}
		return a, nil
	},
	config.IYSProviderNetgsm: func(*container, config.Consent) (consent.IYSSubmitter, error) {
		return nil, errors.New("consent: CONSENT_IYS_PROVIDER=netgsm: the Netgsm adapter is not built yet (BE-110)")
	},
}

// iysAdapter builds the İYS adapter CONSENT_IYS_PROVIDER names; nil for
// none.
func iysAdapter(c *container, cfg config.Consent) (consent.IYSSubmitter, error) {
	if cfg.IYSProvider == "" || cfg.IYSProvider == config.IYSProviderNone {
		return nil, nil
	}
	build, ok := iysAdapters[cfg.IYSProvider]
	if !ok {
		return nil, fmt.Errorf("consent: unknown CONSENT_IYS_PROVIDER %q", cfg.IYSProvider)
	}
	return build(c, cfg)
}

func (w *consentWiring) build(c *container) error {
	cfg := c.infra.cfg.Consent
	if w.cfg != nil {
		cfg = *w.cfg
	}
	client := w.iys
	var feed consent.IYSFeedReader
	var states consent.IYSStateReader
	if client == nil {
		a, err := iysAdapter(c, cfg)
		if err != nil {
			return err
		}
		if a != nil {
			client = consent.NewIYSClient(a)
			feed, _ = a.(consent.IYSFeedReader)
			states, _ = a.(consent.IYSStateReader)
			w.iys = client
		}
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
		Reauth: lateReauth{w: w}, IYS: client, IYSFeed: feed, IYSStates: states, IYSRequired: cfg.IYSRequired,
		IYSSyncMaxAge: cfg.IYSSyncMaxAge,
		Unsubscribe:   consent.UnsubscribeConfig{BaseURL: cfg.UnsubscribeBaseURL, TTL: cfg.UnsubscribeTTL, Deriver: deriver},
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
// reads İYS, consent.iys_status_sync (hourly :20, default; BE-103, BE-105).
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

// metrics registers koro_consent_iys_alarms_total{reason} (BE-105) on the
// worker's registry (the İYS tasks run there); nil m → metrics off.
func (w *consentWiring) metrics(m *obs.Metrics) error {
	return m.CounterFunc("koro_consent_iys_alarms_total",
		"İYS alarms for the DPO by reason (too_late: İYS refused a consent older than three business days; conflict: "+
			"İYS kept an older refusal, the grant was withdrawn; sweep_unavailable; read_rejected).",
		[]string{"reason"}, func() []obs.Sample {
			as := w.mod.IYSAlarms()
			out := make([]obs.Sample, len(as))
			for i, a := range as {
				out[i] = obs.Sample{Labels: []string{a.Reason}, Value: float64(a.Count)}
			}
			return out
		})
}
