package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/consent"
	"koro/internal/identity"
	"koro/internal/platform/config"
	"koro/internal/platform/crypto"
	"koro/internal/platform/db"
	"koro/internal/platform/iys"
	"koro/internal/platform/iys/iysfake"
	"koro/internal/platform/iys/netgsm"
	"koro/internal/platform/iys/official"
	"koro/internal/platform/obs"
	"koro/internal/platform/queue"
)

// consentWiring wires the consent module (L2; 01 §4.3). First owner: BE-012.
// Only tickets of the consent module edit this file (BE-001 R4).
//
// Seams (nil in production unless noted):
//
//	store   nil → consent.NewGormStore over the app_rw pool
//	tx      nil → the app_rw pool's TxManager; tests preset db.NopTx
//	iys     nil → the İYS adapter CONSENT_IYS_PROVIDER names (iysAdapters:
//	        the adapters[T] selection point; BE-105, BE-114): its Submit behind
//	        consent.NewIYSClient, its change feed and single-state reads
//	        (consent.IYSFeedReader, consent.IYSStateReader) when it has
//	        them; consent.NoopIYS with none (commercial grants refused,
//	        withdrawals "skipped"; CONSENT_IYS_REQUIRED refuses that).
//	        Tests preset FakeIYS (client and both readers). A reader gets
//	        consent.iys_status_sync (BE-103, BE-105);
//	        CONSENT_IYS_SYNC_MAX_AGE refuses to start without one. An
//	        adapter that takes webhook deliveries (netgsm, BE-110) is
//	        consent.IYSPushReceiver too (receiveIYSWebhook)
//	push    nil → the adapter's own receiver when it has one; tests that
//	        preset iys preset a Netgsm receiver over an in-memory store
//	        (the matrix's receiveIYSWebhook)
//	netgsmStore nil → netgsm.NewGormStore over the app_rw pool (the
//	        webhook items, 000147; purged daily whatever the adapter);
//	        tests preset netgsm.NewMemStore
//	cfg     tests only; nil → the consent section of the loaded configuration
//	deriver nil → crypto.Deriver over CRYPTO_TOKEN_KEYS (the one-click
//	        unsubscribe tokens, BE-062); tests preset the shared test deriver
//	reauth  the re-authentication verifier of the R operations (03 §6.2:
//	        identity.Reauth.Verify). identity is L4 and builds after
//	        consent, so the binding is late: BE-014 sets c.consent.reauth
//	        in wire_identity.go build, only when it is nil (tests preset a
//	        fake). The type uses stdlib types only (R4 hook rule). nil →
//	        every R operation fails closed (503 consent.reauth_unavailable).
//	termsPublished what a published terms of use version triggers
//	        (consent.TermsWatcher; BE-124): identity sets it in
//	        wire_identity.go build (its terms gate and the reader notices),
//	        only when nil; read at call time (lateTermsWatcher). nil → nobody
//	        is told.
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
	store       consent.Store
	tx          db.TxManager
	iys         consent.IYSClient
	push        consent.IYSPushReceiver
	netgsmStore netgsm.Store
	cfg         *config.Consent
	deriver     *crypto.Deriver
	reauth      func(ctx context.Context, userID uuid.UUID, password, totpCode string) (kind string, err error)

	termsPublished func(ctx context.Context, noticeID uuid.UUID)

	mod   consent.Module
	svc   consent.Service          // set by build
	links consent.UnsubscribeLinks // set by build; nil without CONSENT_UNSUBSCRIBE_BASE_URL
	jobs  []queue.Job              // set by build: the adapter's own jobs (the Netgsm backup check)
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

// lateTermsWatcher adapts consentWiring.termsPublished to
// consent.TermsWatcher and reads it at call time (see consentWiring).
type lateTermsWatcher struct{ w *consentWiring }

func (l lateTermsWatcher) TermsPublished(ctx context.Context, noticeID uuid.UUID) {
	if f := l.w.termsPublished; f != nil {
		f(ctx, noticeID)
	}
}

// iysRecipients resolves İYS recipients for the adapters (their
// Recipients interfaces) over identity's Users (Contact, FindByEmail) at
// call time (worker only). identity holds e-mail addresses only, so the
// MESAJ and ARAMA types have no recipient.
type iysRecipients struct{ c *container }

var (
	_ official.Recipients = iysRecipients{}
	_ iysfake.Recipients  = iysRecipients{}
	_ netgsm.Recipients   = iysRecipients{}
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

// iysAdapters is the selection point of CONSENT_IYS_PROVIDER (the BE-113
// pattern since BE-114): one constructor per value config.IYSProviders accepts
// (TestWire_EveryConfiguredAdapterIsWired keeps them equal); none builds
// no adapter (nil: consent uses NoopIYS). A new adapter is a package of
// internal/platform/iys that passes iyscontract.Run, its settings in
// config.Consent and its line here; consent does not change. The fake is
// refused in staging and prod by the configuration (config.Consent).
func iysAdapters(c *container, cfg config.Consent) adapters[consent.IYSSubmitter] {
	return adapters[consent.IYSSubmitter]{
		config.IYSProviderNone: func() (consent.IYSSubmitter, error) { return nil, nil },
		config.IYSProviderFake: func() (consent.IYSSubmitter, error) {
			// Development only: İYS in the worker's memory, with İYS's
			// time rules and identity's addresses.
			return iysfake.New(iysfake.Options{Clock: c.infra.clock, Recipients: iysRecipients{c: c}}), nil
		},
		config.IYSProviderOfficial: func() (consent.IYSSubmitter, error) {
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
		config.IYSProviderNetgsm: func() (consent.IYSSubmitter, error) {
			// The pilot's integrator (BE-110): uploads by API, İYS's answers
			// and the recipients' own changes by webhook into
			// iys_netgsm_events (shared by the api, which receives, and the
			// worker, which reads and confirms each success answer with
			// /iys/search before it counts: Q-16, BE-114).
			store := c.consent.netgsmEvents(c)
			if store == nil {
				return nil, errors.New("consent: the Netgsm İYS adapter needs the database (webhook items)")
			}
			n := cfg.Netgsm
			a, err := netgsm.New(netgsm.Config{
				BaseURL: n.BaseURL, Username: n.Username, Password: n.Password.Reveal(), IYSCode: n.IYSCode, BrandCode: n.BrandCode,
				RecipientType: cfg.IYSRecipientType, RefIn: n.RefIDIn, WebhookToken: n.WebhookToken.Reveal(),
				WebhookIPs: n.AllowedPrefixes(), Recipients: iysRecipients{c: c}, Store: store, Clock: c.infra.clock,
				Log: c.infra.log.With("module", "consent", "iys", "netgsm"), Timeout: cfg.IYSTimeout,
				RequestsPerMinute: n.RequestsPerMinute, StatusQueriesPerHour: n.StatusQueriesPerHour,
				ResultWait: waitOrNone(n.ResultWait), ResendAfter: n.ResendAfter,
			})
			if err != nil {
				return nil, err
			}
			return a, nil
		},
	}
}

// netgsmEvents is the store of the Netgsm webhook items (000147): the
// preset one (tests) or the app_rw pool's; nil without a database.
func (w *consentWiring) netgsmEvents(c *container) netgsm.Store {
	if w.netgsmStore == nil && c.infra.db != nil {
		w.netgsmStore = netgsm.NewGormStore(c.infra.db.ORM)
	}
	return w.netgsmStore
}

// waitOrNone maps CONSENT_IYS_NETGSM_RESULT_WAIT=0 to "no wait" (the
// adapter's zero is its default).
func waitOrNone(d time.Duration) time.Duration {
	if d == 0 {
		return -1
	}
	return d
}

// iysGrantHolders lists the users with an unscoped grant in force of an
// İYS purpose of channel ch from consent's ledger (the Netgsm backup
// check, consent.iys_netgsm_recheck), in user ID order without duplicates.
func iysGrantHolders(store consent.Store, ch consent.IYSChannel) func(ctx context.Context) ([]uuid.UUID, error) {
	return func(ctx context.Context) ([]uuid.UUID, error) {
		purposes, err := store.Purposes(ctx)
		if err != nil {
			return nil, err
		}
		seen := map[uuid.UUID]bool{}
		var out []uuid.UUID
		for _, p := range purposes {
			if !p.RequiresIYS || p.IYSChannel == nil || *p.IYSChannel != ch {
				continue
			}
			for after := uuid.Nil(); ; {
				grants, err := store.GrantedChains(ctx, p.Code, after, iysGrantPage)
				if err != nil {
					return nil, err
				}
				for _, g := range grants {
					if !seen[g.UserID] {
						seen[g.UserID] = true
						out = append(out, g.UserID)
					}
				}
				if len(grants) < iysGrantPage {
					break
				}
				after = grants[len(grants)-1].UserID
			}
		}
		slices.SortFunc(out, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
		return out, nil
	}
}

const iysGrantPage = 1000

// iysAdapter builds the İYS adapter CONSENT_IYS_PROVIDER names; nil for
// none (also the zero configuration of tests).
func iysAdapter(c *container, cfg config.Consent) (consent.IYSSubmitter, error) {
	name := cfg.IYSProvider
	if name == "" {
		name = config.IYSProviderNone
	}
	return iysAdapters(c, cfg).pick("CONSENT_IYS_PROVIDER", name)
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
	client, push := w.iys, w.push
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
			if push == nil {
				push, _ = a.(consent.IYSPushReceiver)
			}
			w.iys = client
		}
		if n, ok := a.(*netgsm.Client); ok && cfg.Netgsm.RecheckPerHour > 0 {
			w.jobs = append(w.jobs, n.RecheckJob(iys.Email, cfg.Netgsm.RecheckPerHour, iysGrantHolders(store, consent.IYSEmail)))
		}
	}
	// The retention of the Netgsm webhook items runs whatever the adapter:
	// items of an earlier Netgsm period go too (iys_netgsm_event, 7 days).
	if events := w.netgsmEvents(c); events != nil {
		w.jobs = append(w.jobs, netgsm.PurgeJob(netgsm.PurgeDeps{Store: events, Clock: c.infra.clock,
			Period: c.retention(netgsm.RetentionPolicyCode, 0, netgsm.Retention), Log: c.infra.log.With("module", "consent")}))
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
		Reauth: lateReauth{w: w}, IYS: client, IYSFeed: feed, IYSStates: states, IYSPush: push, IYSRequired: cfg.IYSRequired,
		IYSSyncMaxAge: cfg.IYSSyncMaxAge,
		Unsubscribe:   consent.UnsubscribeConfig{BaseURL: cfg.UnsubscribeBaseURL, TTL: cfg.UnsubscribeTTL, Deriver: deriver},
		TermsWatch:    lateTermsWatcher{w: w},
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
// consent.iys_reconcile job (05:30, default), with an İYS client that
// reads İYS consent.iys_status_sync (hourly :20, default; BE-103, BE-105),
// with the Netgsm adapter its backup check consent.iys_netgsm_recheck
// (every 10 minutes, default) and always consent.iys_netgsm_purge (04:40,
// low: the retention of the webhook items; BE-110).
func (w *consentWiring) tasks(c *container) {
	w.mod.Subscribe(c.infra.subs)
	for _, j := range append(w.mod.Jobs(), w.jobs...) {
		c.infra.jobs.Schedule(j)
	}
}

// routes: the 13 consent operations (04 §2.5; BE-062: getUnsubscribeLink,
// confirmUnsubscribe; BE-110: receiveIYSWebhook); w.svc is nil under
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
