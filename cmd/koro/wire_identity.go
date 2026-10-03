package main

import (
	"context"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/danielgtaylor/huma/v2"
	"github.com/redis/go-redis/v9"

	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/identity"
	"koro/internal/platform/config"
	"koro/internal/platform/crypto"
	"koro/internal/platform/crypto/rewrap"
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
//	secrets  nil → the shared envelope c.keys.envelope() (TOTP secrets;
//	         BE-055/T-185: OpenBao Transit or the local KEK); the column
//	         mfa_totp.secret_enc is declared for crypto.rewrap
//	challenges nil → Redis (c.infra.redis; 5-minute MFA login challenges,
//	         06 H-16); without Redis (openapi) MFA answers 503. memChallenges
//	         (tests) → identity.MemChallenges on the container clock
//	domains  nil → hooks.domainResolver, read at call time (directory,
//	         BE-020); both nil → every domain is unknown: 422
//	         identity.edu_domain_unknown, the document path stays open
//	         (BE-018 safe default: nobody becomes a student by e-mail)
//	universityNames late-bound university names (directory has no hook
//	         for them yet, T-223; nil → answers carry the university ID only)
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
// c.notify.svc (worker e-mails), c.audit.svc, c.files.svc (student
// documents: Get, Attach, PresignGet; Delete through identity.FileRemover),
// hooks.memberships and hooks.domainResolver (read at call time, so orgs
// and directory may fill them later) and hooks.retention.
type identityWiring struct {
	store   identity.Store
	tx      db.TxManager
	deriver *crypto.Deriver
	hasher  identity.PasswordHasher
	passwd  *config.Passwd
	cfg     *config.Identity

	secrets       identity.SecretBox
	challenges    identity.ChallengeStore
	memChallenges bool

	domains         identity.DomainResolver
	universityNames identity.UniversityNames

	// late is the container, kept for the late-bound reads above and for
	// the matrix fixtures (student documents live in the files store).
	late *container

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
	// TOTP secrets: the shared envelope of platform/crypto (BE-055, T-185;
	// OpenBao Transit or the local KEK), never a LocalKEK of identity's own.
	secrets := w.secrets
	if secrets == nil {
		env, err := c.keys.envelope()
		if err != nil {
			return fmt.Errorf("identity: TOTP secrets: %w", err)
		}
		secrets = env
	}
	if err := c.keys.addRewrap(rewrap.Column{Name: "identity.mfa_totp.secret_enc", Table: "mfa_totp", Key: "user_id",
		Column: "secret_enc", PurposePrefix: identity.TOTPSecretPurposePrefix, PurposeColumn: "user_id"}); err != nil {
		return fmt.Errorf("identity: %w", err)
	}
	challenges := w.challenges
	switch {
	case challenges != nil:
	case w.memChallenges:
		challenges = identity.NewMemChallenges(c.infra.clock)
	case c.infra.redis != nil:
		challenges = redisChallenges{rdb: c.infra.redis}
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
		Hasher: hasher, Secrets: secrets, Challenges: challenges,
		Domains: func(ctx context.Context, domain string) (uuid.UUID, bool, error) {
			if w.domains != nil {
				return w.domains(ctx, domain)
			}
			if h := c.hooks.domainResolver; h != nil {
				return h(ctx, domain)
			}
			return uuid.Nil(), false, nil // directory not wired: unknown (422, document path)
		},
		UniversityNames: func(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
			if w.universityNames != nil {
				return w.universityNames(ctx, ids)
			}
			return nil, nil
		},
	}
	if fs := c.files.svc; fs != nil {
		d.Files = fs
		d.RemoveFile = func(ctx context.Context, a authz.Actor, id uuid.UUID) error { return fs.Delete(ctx, a, id) }
	}
	mod, err := identity.New(d)
	if err != nil {
		return err
	}
	w.mod, w.svc, w.secrets, w.challenges, w.late = mod, mod.Service, secrets, challenges, c
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
// identity.send_security_notice, identity.send_role_alert and
// identity.send_edu_notice tasks; identity.sessions_cleanup (03:30) and
// identity.email_tokens_cleanup (03:35) on the low queue,
// identity.edu_expire (00:30) and identity.edu_renewal_reminders (10:00)
// (01 §7).
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

// routes: the identity operations of BE-014, BE-016 and BE-018 (04 §2.1,
// plus adminApprovePlatformRole of 06 H-17); w.svc is nil under `koro
// openapi`, which Register tolerates.
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

// redisChallenges is identity.ChallengeStore over the queue's Redis
// (06 H-16: MFA login challenges live 5 minutes in Redis). Keys are
// already digests (identity never hands over a usable challenge ID); Take
// is GETDEL, so a challenge is used at most once even under concurrency.
type redisChallenges struct{ rdb redis.UniversalClient }

const redisChallengePrefix = "koro:"

func (r redisChallenges) Put(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if ttl <= 0 {
		return fmt.Errorf("identity challenge: ttl %s", ttl)
	}
	return r.rdb.Set(ctx, redisChallengePrefix+key, value, ttl).Err()
}

func (r redisChallenges) Take(ctx context.Context, key string) ([]byte, bool, error) {
	b, err := r.rdb.GetDel(ctx, redisChallengePrefix+key).Bytes()
	switch {
	case errors.Is(err, redis.Nil):
		return nil, false, nil
	case err != nil:
		return nil, false, err
	}
	return b, true, nil
}
