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
// BE-015. Only tickets of the notify module edit this file (BE-001 R4).
//
// Seams (all nil in production unless noted):
//
//	store      nil → notify.NewGormStore over the app_rw pool; tests preset
//	           the in-memory store
//	tx         nil → the app_rw pool's TxManager; tests preset db.NopTx
//	transport  nil → the SMTP adapter from config (SMTP_*); tests preset a
//	           notify.MemTransport
//	consent    nil → c.consent.svc adapted to notify.ConsentChecker
//	           (consent.Checker.FilterActive, 06 H-43 note); consent is L2
//	           and built before notify
//	indexer    nil → platform/crypto blind index (CRYPTO_BLIND_INDEX_KEY)
//	templates  nil → the templates embedded in the binary
//	snsCerts   nil → the HTTPS fetcher of SNS signing certificates
//	auditFeed  nil → audit's GORM store (Chained, HeadAt: read only), or the
//	           in-memory audit store of the tests
//	cursor     nil → Redis (the audit alert job position), in-memory without Redis
//	push       nil → notify.PushTransport over FCM/APNs from config (PUSH_*,
//	           BE-055), or the no-op adapter when neither is configured
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
		transport, err = notify.NewSMTPTransport(notify.SMTPConfig{
			Host: cfg.SMTPHost, Port: cfg.SMTPPort, TLS: cfg.SMTPTLS,
			Username: cfg.SMTPUsername, Password: cfg.SMTPPassword.Reveal(), Timeout: cfg.SMTPTimeout,
		})
		if err != nil {
			return fmt.Errorf("smtp transport: %w", err)
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
		Push:        push, // nil → NoopPush
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

// newPushSender builds the FCM/APNs transport from the push settings
// (BE-055); nil (no-op adapter) when no provider is configured, an error
// when PUSH_REQUIRED and none is (also caught by config validation) or a
// credential does not parse (the process does not start).
func newPushSender(cfg config.Notify, tokens notify.PushTokens, c *container) (notify.PushSender, error) {
	var (
		fcm  *notify.FCM
		apns *notify.APNs
		err  error
	)
	if cfg.FCMEnabled() {
		fcm, err = notify.NewFCM(notify.FCMConfig{ProjectID: cfg.PushFCMProjectID,
			CredentialsJSON: []byte(cfg.PushFCMCredentials.Reveal()), Timeout: cfg.PushTimeout, Clock: c.infra.clock})
		if err != nil {
			return nil, err
		}
	}
	if cfg.APNsEnabled() {
		apns, err = notify.NewAPNs(notify.APNsConfig{KeyPEM: []byte(cfg.PushAPNsKey.Reveal()), KeyID: cfg.PushAPNsKeyID,
			TeamID: cfg.PushAPNsTeamID, Topic: cfg.PushAPNsTopic, Sandbox: cfg.PushAPNsSandbox, Timeout: cfg.PushTimeout,
			Clock: c.infra.clock})
		if err != nil {
			return nil, err
		}
	}
	if fcm == nil && apns == nil {
		if cfg.PushRequired {
			return nil, errors.New("notify: PUSH_REQUIRED=true but neither FCM nor APNs is configured")
		}
		return nil, nil
	}
	return notify.NewPushTransport(notify.PushTransportConfig{Tokens: tokens, FCM: fcm, APNs: apns, Clock: c.infra.clock,
		Log: c.infra.log.With("module", "notify", "component", "push")})
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
