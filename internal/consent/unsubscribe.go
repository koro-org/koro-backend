package consent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/crypto"
	"koro/internal/platform/db"
	"koro/internal/platform/queue"
	"koro/internal/platform/token"
)

// One-click unsubscribe (BE-062, BE-153; 6563 and the commercial
// communication regulation: an easy, free way to refuse in every commercial
// message; RFC 8058 List-Unsubscribe-Post, which Gmail and Yahoo require of
// bulk senders).
//
// A link is <CONSENT_UNSUBSCRIBE_BASE_URL>/<token>. Since BE-153 (user
// decision Q-36: the mailbox industry's practice) the token is opaque: 32
// bytes from crypto/rand in base64url (platform/token, 43 characters),
// minted for every commercial e-mail. unsubscribe_links (000162) keeps its
// SHA-256 with the recipient, the purpose and the expiry; the token itself
// is nowhere but in that e-mail. Nothing about the recipient can be read
// from the URL (an access log, the browser history, a forwarded mail), a
// leaked link is one message's, and the erasure of the account retires
// every link at once (privacy.go).
//
// Signed links of the BE-062 format, which carry the user ID in clear, are
// still in the mailboxes: they are accepted until their own expiry
// (unsubscribe_legacy.go). The two never collide: an opaque token is
// exactly 43 characters, a signed one at least 80.
//
// The endpoints answer every unusable token — malformed, unknown, tampered,
// signed by an unknown key, expired, of an erased account — with the same
// ErrInvalidUnsubscribeLink, and a valid one never reveals the subject or
// the consent state: getUnsubscribeLink shows the purpose only and
// confirmUnsubscribe is 204 whether a consent was withdrawn or nothing was
// in force (idempotent). A link is not single use: until it expires every
// click withdraws the consent in force (a mailbox provider retries; a user
// who granted again since must still be able to refuse from the older
// mail).

// DefaultUnsubscribeTTL is the lifetime of a link when UnsubscribeConfig
// sets none (CONSENT_UNSUBSCRIBE_TTL).
const DefaultUnsubscribeTTL = 365 * 24 * time.Hour

const (
	// MaxUnsubscribeToken bounds the token text (the path parameter): the
	// longest signed token is 164 characters.
	MaxUnsubscribeToken = 200

	// PolicyUnsubscribeLink is the retention policy of unsubscribe_links
	// (000162): a job policy, applied by consent.unsubscribe_links_cleanup.
	PolicyUnsubscribeLink = "unsubscribe_link"
	// UnsubscribeLinkRetention is how long an expired link is kept when the
	// policy gives no period (000162's seed: 7 days).
	UnsubscribeLinkRetention = 7 * 24 * time.Hour
	// UnsubscribeCleanupJobName deletes the expired links (01 §7: daily
	// 03:50, low).
	UnsubscribeCleanupJobName = "consent.unsubscribe_links_cleanup"

	unsubscribeCleanupCron = "50 3 * * *"
)

// ErrUnsubscribeUnavailable is UnsubscribeURL's answer when no link can be
// minted, whatever a retry does: CONSENT_UNSUBSCRIBE_BASE_URL is unset
// (commercial e-mail cannot be sent; outreach refuses it at
// createDispatch), or the purpose or recipient has no link.
var ErrUnsubscribeUnavailable = errors.New("consent: no unsubscribe link can be minted")

// UnsubscribeConfig configures the one-click unsubscribe links.
type UnsubscribeConfig struct {
	// BaseURL is CONSENT_UNSUBSCRIBE_BASE_URL (https, no query or fragment).
	// "" → no link is minted (Module.Links is nil); links minted earlier
	// still work.
	BaseURL string
	// TTL is CONSENT_UNSUBSCRIBE_TTL; 0 → DefaultUnsubscribeTTL.
	TTL time.Duration
	// Deriver verifies the signed links of the BE-062 format still in the
	// mailboxes (CRYPTO_TOKEN_KEYS). nil → they are refused (fail closed).
	// Opaque links need no key.
	Deriver *crypto.Deriver
}

func (c UnsubscribeConfig) validate() error {
	if c.TTL < 0 {
		return errors.New("consent.Deps.Unsubscribe.TTL is negative")
	}
	if c.BaseURL == "" {
		return nil
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return errors.New("consent.Deps.Unsubscribe.BaseURL must be an absolute https URL without query or fragment")
	}
	return nil
}

func (c UnsubscribeConfig) enabled() bool { return c.BaseURL != "" }

func (c UnsubscribeConfig) ttl() time.Duration {
	if c.TTL == 0 {
		return DefaultUnsubscribeTTL
	}
	return c.TTL
}

// UnsubscribeLinks mints the one-click unsubscribe links of commercial
// e-mail (BE-062, BE-153). outreach puts one per recipient in
// notify.Message (List-Unsubscribe, List-Unsubscribe-Post and the footer).
type UnsubscribeLinks interface {
	// UnsubscribeURL mints a new https link that withdraws userID's
	// consent to purpose: the base URL, "/" and a fresh opaque token whose
	// digest it stores with the recipient, expiring after the configured
	// TTL. One call per e-mail. ErrUnsubscribeUnavailable (wrapped) without
	// a base URL or for a purpose that has no link (only commercial_email
	// has); any other error is the store's (transient).
	UnsubscribeURL(ctx context.Context, userID uuid.UUID, purpose Purpose) (string, error)
}

// UnsubscribeLink is what getUnsubscribeLink shows: the purpose the link
// withdraws, nothing about the subject or the consent state.
type UnsubscribeLink struct {
	Purpose Purpose
}

// UnsubscribeToken is one opaque link of unsubscribe_links (000162): the
// digest of its token, never the token.
type UnsubscribeToken struct {
	ID        uuid.UUID
	TokenHash []byte // SHA-256 of the token (token.Hash)
	UserID    uuid.UUID
	Purpose   Purpose
	CreatedAt time.Time
	ExpiresAt time.Time
}

// unsubscribeLinks is the store view of the opaque links.
func (s *service) unsubscribeLinks() UnsubscribeLinkStore { return s.d.Store }

// UnsubscribeURL implements UnsubscribeLinks.
func (s *service) UnsubscribeURL(ctx context.Context, userID uuid.UUID, purpose Purpose) (string, error) {
	u := s.d.Unsubscribe
	switch {
	case !u.enabled():
		return "", fmt.Errorf("%w (CONSENT_UNSUBSCRIBE_BASE_URL is not set)", ErrUnsubscribeUnavailable)
	case purpose != PurposeCommercialEmail:
		return "", fmt.Errorf("%w: purpose %s has no unsubscribe link", ErrUnsubscribeUnavailable, purpose)
	case userID == uuid.Nil():
		return "", fmt.Errorf("%w: a link needs a user", ErrUnsubscribeUnavailable)
	}
	raw, hash, err := token.New()
	if err != nil {
		return "", err
	}
	now := s.now()
	l := UnsubscribeToken{ID: uuid.NewV7(), TokenHash: hash, UserID: userID, Purpose: purpose, CreatedAt: now, ExpiresAt: now.Add(u.ttl())}
	if err := s.unsubscribeLinks().InsertUnsubscribeLink(ctx, l); err != nil {
		return "", fmt.Errorf("consent: store unsubscribe link: %w", err)
	}
	return strings.TrimRight(u.BaseURL, "/") + "/" + raw, nil
}

// unsubscribeClaim is what a usable link says.
type unsubscribeClaim struct {
	UserID  uuid.UUID
	Purpose Purpose
	signed  bool // the BE-062 format
}

// resolveUnsubscribe returns the claim of raw, or the reason it is refused
// ("" on success). The reason is for the log only: every refusal is the
// same error to the caller. err is a store failure, not a verdict.
func (s *service) resolveUnsubscribe(ctx context.Context, raw string) (unsubscribeClaim, string, error) {
	// Even Strict base64 decoding ignores CR/LF. A path credential must
	// use the minted base64url spelling, without embedded line breaks.
	if raw == "" || len(raw) > MaxUnsubscribeToken || strings.ContainsAny(raw, "\r\n") {
		return unsubscribeClaim{}, "malformed", nil
	}
	if len(raw) != token.EncodedLen {
		c, reason := verifySigned(s.d.Unsubscribe.Deriver, raw, s.now())
		return c, reason, nil
	}
	if !token.WellFormed(raw) {
		return unsubscribeClaim{}, "malformed", nil
	}
	l, err := s.unsubscribeLinks().UnsubscribeLinkByHash(ctx, token.Hash(raw))
	switch {
	case errors.Is(err, ErrNotFound):
		return unsubscribeClaim{}, "unknown", nil
	case err != nil:
		return unsubscribeClaim{}, "", err
	case !token.Verify(raw, l.TokenHash):
		// The store looked the digest up; it is compared again in
		// constant time, whatever the store's comparison was.
		return unsubscribeClaim{}, "unknown", nil
	case !s.now().Before(l.ExpiresAt):
		return unsubscribeClaim{}, "expired", nil
	}
	return unsubscribeClaim{UserID: l.UserID, Purpose: l.Purpose}, "", nil
}

// unsubscribeSubject resolves raw and authorises its subject: the link is
// the subject's proof of identity for the withdrawal of its own consent
// (consent.record.create, O cell; the HTTP caller — anonymous, a mailbox
// provider or another signed-in user — is not the actor).
func (s *service) unsubscribeSubject(ctx context.Context, raw string) (unsubscribeClaim, authz.Actor, error) {
	c, reason, err := s.resolveUnsubscribe(ctx, raw)
	switch {
	case err != nil:
		return unsubscribeClaim{}, authz.Actor{}, err
	case reason != "":
		s.d.Log.LogAttrs(ctx, slog.LevelInfo, "unsubscribe link refused", slog.String("reason", reason))
		return unsubscribeClaim{}, authz.Actor{}, ErrInvalidUnsubscribeLink
	case c.signed:
		// How long the signed format is still used (00 T-395: its code
		// goes once no such link can be valid).
		s.d.Log.LogAttrs(ctx, slog.LevelInfo, "unsubscribe link of the signed format accepted")
	}
	subject, uid := authz.Actor{Kind: authz.KindUser, UserID: c.UserID}, c.UserID
	err = authz.Can(ctx, subject, authz.ConsentRecordCreate, authz.Resource{Type: authz.TypeConsentRecord, OwnerUserID: &uid})
	if err != nil {
		return unsubscribeClaim{}, authz.Actor{}, err
	}
	return c, subject, nil
}

// GetUnsubscribe implements Service (getUnsubscribeLink): the purpose a
// valid link withdraws. It writes nothing (link scanners and prefetchers
// open GET links).
func (s *service) GetUnsubscribe(ctx context.Context, raw string) (UnsubscribeLink, error) {
	c, _, err := s.unsubscribeSubject(ctx, raw)
	if err != nil {
		return UnsubscribeLink{}, err
	}
	return UnsubscribeLink{Purpose: c.Purpose}, nil
}

// Unsubscribe implements Service (confirmUnsubscribe, RFC 8058 one-click):
// the subject's consent to the link's purpose is withdrawn when it is in
// force — a withdrawn row through the email_link channel, its audit row,
// the İYS submission (RET) and the consent.recorded event, in one
// transaction. Otherwise nothing is written. A transaction-scoped lock on
// (user, purpose) makes concurrent clicks (a mailbox provider's retries)
// withdraw once. The answer is the same in both cases.
func (s *service) Unsubscribe(ctx context.Context, raw string) error {
	c, subject, err := s.unsubscribeSubject(ctx, raw)
	if err != nil {
		return err
	}
	withdrawn := false
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		if err := s.d.Store.LockSubject(ctx, c.UserID, c.Purpose); err != nil {
			return err
		}
		cur, found, err := s.d.Store.Latest(ctx, c.UserID, c.Purpose, nil)
		if err != nil || !found || cur.Action != Granted {
			return err // nothing in force: idempotent no-op
		}
		if _, err := s.record(ctx, RecordInput{UserID: c.UserID, Purpose: c.Purpose, Action: Withdrawn, Channel: ChannelEmailLink}, subject); err != nil {
			return err
		}
		withdrawn = true
		return nil
	})
	if err != nil {
		return err
	}
	if !withdrawn {
		s.d.Log.LogAttrs(ctx, slog.LevelInfo, "unsubscribe: no consent in force", slog.String("user_id", c.UserID.String()),
			slog.String("purpose", string(c.Purpose)))
	}
	return nil
}

// UnsubscribeCleanupJob is consent.unsubscribe_links_cleanup (01 §7; daily
// 03:50, low): the links that expired before now − the period go, at most
// batchSize per statement (idempotent). period gives the unsubscribe_link
// policy's period at run time (hooks.retention); nil or a period ≤ 0 →
// UnsubscribeLinkRetention. The job runs whether links are minted or not:
// the links of an earlier configuration expire too.
func (m Module) UnsubscribeCleanupJob(period func(context.Context) (time.Duration, error)) queue.Job {
	return queue.Job{
		Name: UnsubscribeCleanupJobName, Cron: unsubscribeCleanupCron, Queue: queue.Low, Unique: 23 * time.Hour,
		Handle: func(ctx context.Context) error { return m.svc.cleanupUnsubscribeLinks(ctx, period) },
	}
}

func (s *service) cleanupUnsubscribeLinks(ctx context.Context, period func(context.Context) (time.Duration, error)) error {
	keep := UnsubscribeLinkRetention
	if period != nil {
		d, err := period(ctx)
		if err != nil {
			return fmt.Errorf("%s: %w", UnsubscribeCleanupJobName, err)
		}
		if d > 0 {
			keep = d
		}
	}
	cutoff, links := s.now().Add(-keep), s.unsubscribeLinks()
	n, err := db.Drain(ctx, batchSize, func(ctx context.Context) (int, error) {
		return links.DeleteExpiredUnsubscribeLinks(ctx, cutoff, batchSize)
	})
	if err != nil {
		return fmt.Errorf("%s: %w", UnsubscribeCleanupJobName, err)
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "cleanup done", slog.String("job", UnsubscribeCleanupJobName), slog.Int("deleted", n))
	return nil
}
