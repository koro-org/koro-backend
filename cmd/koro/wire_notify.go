package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"uuid"

	"github.com/danielgtaylor/huma/v2"
	"github.com/redis/go-redis/v9"

	"koro/internal/audit"
	"koro/internal/consent"
	"koro/internal/notify"
	"koro/internal/platform/config"
	"koro/internal/platform/crypto"
	"koro/internal/platform/db"
	"koro/internal/platform/textx"
)

// notifyWiring wires the notify module (L3; 01 §4.5). Owner: BE-005, then
// BE-015, BE-055 (push), BE-113 (adapter selection). Only tickets of the notify
// module edit this file (BE-001 R4).
//
// Seams (all nil in production unless noted):
//
//	store      nil → notify.NewGormStore over the app_rw pool; tests preset
//	           the in-memory store
//	tx         nil → the app_rw pool's TxManager; tests preset db.NopTx
//	transport  nil → the NOTIFY_EMAIL_PROVIDER adapter (emailTransports:
//	           smtp from SMTP_*, or mem); tests preset a notify.MemTransport
//	consent    nil → c.consent.svc adapted to notify.ConsentChecker
//	           (consent.Checker.FilterActive, 06 H-43 note); consent is L2
//	           and built before notify
//	indexer    nil → platform/crypto blind index (CRYPTO_BLIND_INDEX_KEY)
//	templates  nil → the templates embedded in the binary
//	snsCerts   nil → the HTTPS fetcher of SNS signing certificates
//	auditFeed  nil → audit's GORM store (Chained, HeadAt: read only), or the
//	           in-memory audit store of the tests
//	cursor     nil → Redis (the audit alert job position), in-memory without Redis
//	push       nil → the NOTIFY_PUSH_PROVIDER adapter (newPushSender:
//	           notify.PushTransport over FCM/APNs from PUSH_*, BE-055, or the
//	           no-op adapter)
//	cfg        tests only; nil → the notify section of the loaded configuration
//
// Late binding (R4; filled by a higher layer, read at call time):
//
//	platformAdmins  the users with an effective platform_admin role (06 H-33).
//	                identity sets it in wire_identity.go build (BE-016), only
//	                when nil. nil → the admin entitlement grant alerts of
//	                06 H-40 are skipped with a WARN line.
type notifyWiring struct {
	store     notify.Store
	tx        db.TxManager
	transport notify.EmailTransport
	consent   notify.ConsentChecker
	indexer   notify.RecipientIndexer
	templates *notify.Templates
	snsCerts  notify.SNSCertFetcher
	auditFeed notify.AuditFeed
	cursor    notify.CursorStore
	push      notify.PushSender
	cfg       *config.Notify

	platformAdmins func(ctx context.Context) ([]uuid.UUID, error)

	deps notify.Deps // what build passed to notify.New
	mod  notify.Module
	// svc is the sender consumers in higher layers (identity, events, …)
	// adapt to their own port. Set by build.
	svc notify.Service
}

// blindIndexPurpose is the crypto.Indexer purpose of e-mail addresses in
// notify (recipient_bidx, email_bidx).
const blindIndexPurpose = "notify.email"

// blindIndexer adapts crypto.Indexer to notify.RecipientIndexer.
type blindIndexer struct{ x *crypto.Indexer }

func (b blindIndexer) Index(email string) []byte {
	return b.x.BlindIndex(blindIndexPurpose, []byte(textx.NormalizeEmail(email)))
}

// consentFilter adapts consent.Checker to notify.ConsentChecker.
type consentFilter struct{ c consent.Checker }

func (f consentFilter) FilterActive(ctx context.Context, userIDs []uuid.UUID, purpose string) ([]uuid.UUID, error) {
	return f.c.FilterActive(ctx, userIDs, consent.Purpose(purpose))
}

// lateAdmins reads notifyWiring.platformAdmins at call time.
type lateAdmins struct{ w *notifyWiring }

func (l lateAdmins) PlatformAdmins(ctx context.Context) ([]uuid.UUID, error) {
	if l.w.platformAdmins == nil {
		return nil, errors.New("notify: platform admin resolver is not wired")
	}
	return l.w.platformAdmins(ctx)
}

// redisCursor keeps job positions in Redis (the queue's instance: AOF,
// noeviction). A lost key restarts the job from its lookback; the dedupe
// keys make that harmless.
type redisCursor struct{ rdb redis.UniversalClient }

func (r redisCursor) key(name string) string { return "koro:notify:cursor:" + name }

func (r redisCursor) Load(ctx context.Context, name string) (int64, bool, error) {
	v, err := r.rdb.Get(ctx, r.key(name)).Result()
	if errors.Is(err, redis.Nil) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	n, perr := strconv.ParseInt(v, 10, 64)
	return n, perr == nil, nil // a broken value restarts from the lookback
}

func (r redisCursor) Save(ctx context.Context, name string, pos int64) error {
	return r.rdb.Set(ctx, r.key(name), strconv.FormatInt(pos, 10), 0).Err()
}

// memCursor is the cursor of processes without Redis (tests).
type memCursor struct {
	mu  sync.Mutex
	pos map[string]int64
}

func (m *memCursor) Load(_ context.Context, name string) (int64, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.pos[name]
	return v, ok, nil
}

func (m *memCursor) Save(_ context.Context, name string, pos int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pos == nil {
		m.pos = map[string]int64{}
	}
	m.pos[name] = pos
	return nil
}

func (w *notifyWiring) build(c *container) error {
	cfg := c.infra.cfg.Notify
	if w.cfg != nil {
		cfg = *w.cfg
	}

	tpl := w.templates
	if tpl == nil {
		var err error
		if tpl, err = notify.EmbeddedTemplates(); err != nil {
			return err // a missing or broken template stops the process at start
		}
	}
	store, tx := w.store, w.tx
	if c.infra.db != nil {
		if store == nil {
			store = notify.NewGormStore(c.infra.db.ORM)
		}
		if tx == nil {
			tx = c.infra.db.Tx
		}
	}
	if store == nil || tx == nil {
		return errors.New("notify: no store (database not opened)")
	}
	transport := w.transport
	if transport == nil {
		var err error
		if transport, err = emailTransports(cfg).pick("NOTIFY_EMAIL_PROVIDER", cfg.EmailProvider); err != nil {
			return err
		}
	}
	indexer := w.indexer
	if indexer == nil {
		key, err := crypto.ParseKey(c.infra.platform.Crypto.BlindIndexKey.Reveal())
		if err != nil {
			return fmt.Errorf("notify: blind index key: %w", err)
		}
		x, err := crypto.NewIndexer(key)
		if err != nil {
			return fmt.Errorf("notify: blind index key: %w", err)
		}
		indexer = blindIndexer{x: x}
	}
	consentChecker := w.consent
	if consentChecker == nil && c.consent.svc != nil {
		consentChecker = consentFilter{c: c.consent.svc}
	}
	resendSecret, err := cfg.ResendWebhookSecret()
	if err != nil {
		return err
	}
	certs := w.snsCerts
	if certs == nil {
		certs = notify.NewSNSCertFetcher(c.infra.clock)
	}
	feed := w.auditFeed
	if feed == nil {
		switch {
		case c.infra.db != nil:
			feed = audit.NewGormStore(c.infra.db.ORM)
		case c.audit.store != nil:
			feed = c.audit.store
		}
	}
	cursor := w.cursor
	if cursor == nil {
		if c.infra.redis != nil {
			cursor = redisCursor{rdb: c.infra.redis}
		} else {
			cursor = &memCursor{}
		}
	}

	push := w.push
	if push == nil {
		if push, err = newPushSender(cfg, store, c); err != nil {
			return err
		}
	}

	d := notify.Deps{
		Store:       store,
		Tx:          tx,
		Transport:   transport,
		Templates:   tpl,
		Clock:       c.infra.clock,
		Log:         c.infra.log.With("module", "notify"),
		FromAddress: cfg.SMTPFrom,
		FromName:    cfg.SMTPFromName,
		Recipients:  indexer,
		Audit:       c.audit.svc,
		Consent:     consentChecker,
		Push:        push,
		Webhooks:    notify.WebhookConfig{ResendSecret: resendSecret, SESTopicARNs: cfg.SESTopicARNs(), SNSCerts: certs},
		AuditFeed:   feed,
		Cursor:      cursor,
		Admins:      lateAdmins{w: w},
	}
	if c.infra.inbox != nil {
		d.Inbox = c.infra.inbox
	}
	w.deps, w.mod = d, notify.New(d)
	w.svc = w.mod.Service
	if c.infra.inbox != nil {
		if err := c.infra.inbox.Register(w.mod.Webhook); err != nil {
			return err
		}
	}
	c.privacy = append(c.privacy, w.mod.Privacy)
	c.sweepers = append(c.sweepers, w.mod.Sweeper)
	return nil
}

// emailTransports is the selection point of NOTIFY_EMAIL_PROVIDER (BE-113):
// one line per notify.EmailTransport adapter; each passes
// emailTransportContract (internal/notify/transport_contract_test.go).
func emailTransports(cfg config.Notify) adapters[notify.EmailTransport] {
	return adapters[notify.EmailTransport]{
		config.EmailProviderSMTP: func() (notify.EmailTransport, error) {
			t, err := notify.NewSMTPTransport(notify.SMTPConfig{
				Host: cfg.SMTPHost, Port: cfg.SMTPPort, TLS: cfg.SMTPTLS,
				Username: cfg.SMTPUsername, Password: cfg.SMTPPassword.Reveal(), Timeout: cfg.SMTPTimeout,
			})
			if err != nil {
				return nil, fmt.Errorf("smtp transport: %w", err)
			}
			return t, nil
		},
		config.EmailProviderMem: func() (notify.EmailTransport, error) { return notify.NewMemTransport(), nil },
	}
}

// pushOption installs one push provider into the push transport.
type pushOption func(*notify.PushTransportConfig)

// pushProviders is the selection point of NOTIFY_PUSH_PROVIDER (BE-113): one
// line per push provider of notify.PushTransport (BE-055). The transport
// routes each device to the provider of its platform.
func pushProviders(cfg config.Notify, c *container) adapters[pushOption] {
	return adapters[pushOption]{
		config.PushProviderFCM: func() (pushOption, error) {
			f, err := notify.NewFCM(notify.FCMConfig{ProjectID: cfg.PushFCMProjectID,
				CredentialsJSON: []byte(cfg.PushFCMCredentials.Reveal()), Timeout: cfg.PushTimeout, Clock: c.infra.clock})
			if err != nil {
				return nil, err
			}
			return func(p *notify.PushTransportConfig) { p.FCM = f }, nil
		},
		config.PushProviderAPNs: func() (pushOption, error) {
			a, err := notify.NewAPNs(notify.APNsConfig{KeyPEM: []byte(cfg.PushAPNsKey.Reveal()), KeyID: cfg.PushAPNsKeyID,
				TeamID: cfg.PushAPNsTeamID, Topic: cfg.PushAPNsTopic, Sandbox: cfg.PushAPNsSandbox, Timeout: cfg.PushTimeout,
				Clock: c.infra.clock})
			if err != nil {
				return nil, err
			}
			return func(p *notify.PushTransportConfig) { p.APNs = a }, nil
		},
	}
}

// newPushSender builds the notify.PushSender NOTIFY_PUSH_PROVIDER selects:
// the FCM/APNs transport over the resolved providers, or the no-op adapter
// when none is (an error when PUSH_REQUIRED, also caught by config
// validation). A credential that does not parse stops the process.
func newPushSender(cfg config.Notify, tokens notify.PushTokens, c *container) (notify.PushSender, error) {
	names := cfg.ActivePushProviders()
	if len(names) == 0 {
		if cfg.PushRequired {
			return nil, errors.New("notify: PUSH_REQUIRED=true but neither FCM nor APNs is configured")
		}
		return notify.NoopPush{}, nil
	}
	tc := notify.PushTransportConfig{Tokens: tokens, Clock: c.infra.clock,
		Log: c.infra.log.With("module", "notify", "component", "push")}
	options := pushProviders(cfg, c)
	for _, name := range names {
		install, err := options.pick("NOTIFY_PUSH_PROVIDER", name)
		if err != nil {
			return nil, err
		}
		install(&tc)
	}
	return notify.NewPushTransport(tc)
}

// tasks schedules notify.send_audit_alerts (every minute, default; 06 H-40,
// H-43). The webhook processor is registered on the inbox in build;
// notify.purge arrives with BE-057.
func (w *notifyWiring) tasks(c *container) {
	for _, j := range w.mod.Jobs {
		c.infra.jobs.Schedule(j)
	}
}

// routes: the 11 notify operations (04 §2.19, §2.0); w.svc is nil under
// `koro openapi`, which Register tolerates.
func (w *notifyWiring) routes(c *container, api huma.API) { notify.Register(api, w.svc) }
