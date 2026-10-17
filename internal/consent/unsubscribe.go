package consent

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"strings"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/crypto"
)

// One-click unsubscribe (BE-062; 6563 and the commercial communication
// regulation: an easy, free way to refuse in every commercial message;
// RFC 8058 List-Unsubscribe-Post, which Gmail and Yahoo require of bulk
// senders).
//
// A link is <CONSENT_UNSUBSCRIBE_BASE_URL>/<token>. The token is stateless:
// no row is written per message. It is an HMAC (crypto.Deriver over
// CRYPTO_TOKEN_KEYS, subkey of the purpose consent.unsubscribe, so it never
// verifies as another kind of derived token) over
//
//	version (1) | purpose tag (1) | user ID (16) | expiry, Unix seconds (8) | key ID length (1) | key ID
//
// followed by the 32-byte MAC, base64url without padding. The token proves
// who the subject is and which purpose it withdraws; it carries no address
// and grants nothing else. Rotating CRYPTO_TOKEN_KEYS keeps older links
// valid while their key stays in the keyring.
//
// The endpoints answer every unusable token — malformed, tampered, signed by
// an unknown key, of an unknown purpose, expired — with the same
// ErrInvalidUnsubscribeLink, and a valid one never reveals the subject or
// the consent state: getUnsubscribeLink shows the purpose only and
// confirmUnsubscribe is 204 whether a consent was withdrawn or nothing was
// in force (idempotent).

// DefaultUnsubscribeTTL is the lifetime of a link when UnsubscribeConfig
// sets none (CONSENT_UNSUBSCRIBE_TTL).
const DefaultUnsubscribeTTL = 365 * 24 * time.Hour

const (
	// unsubscribeDerive is the crypto.Deriver purpose of the MAC subkey.
	unsubscribeDerive = "consent.unsubscribe"
	// unsubscribeVersion is the token layout.
	unsubscribeVersion byte = 1
	// unsubscribeFixed is the fixed head: version, tag, user ID, expiry.
	unsubscribeFixed = 1 + 1 + 16 + 8
	unsubscribeMAC   = 32
	maxKeyID         = 64 // crypto.ParseKeyring IDs
	// MaxUnsubscribeToken bounds the token text (the path parameter).
	MaxUnsubscribeToken = 200
)

// unsubscribeTags are the purposes a link can withdraw. Only commercial
// e-mail has a link: push is refused in the app, SMS is not used.
var unsubscribeTags = map[Purpose]byte{PurposeCommercialEmail: 1}

func purposeOfTag(tag byte) (Purpose, bool) {
	for p, t := range unsubscribeTags {
		if t == tag {
			return p, true
		}
	}
	return "", false
}

// ErrUnsubscribeUnavailable is UnsubscribeURL's answer while no link can be
// minted (CONSENT_UNSUBSCRIBE_BASE_URL unset): commercial e-mail cannot be
// sent (outreach refuses it at createDispatch).
var ErrUnsubscribeUnavailable = errors.New("consent: unsubscribe links are not configured (CONSENT_UNSUBSCRIBE_BASE_URL)")

// UnsubscribeConfig configures the one-click unsubscribe links.
type UnsubscribeConfig struct {
	// BaseURL is CONSENT_UNSUBSCRIBE_BASE_URL (https, no query or fragment).
	// "" → no link is minted (Module.Links is nil).
	BaseURL string
	// TTL is CONSENT_UNSUBSCRIBE_TTL; 0 → DefaultUnsubscribeTTL.
	TTL time.Duration
	// Deriver signs and verifies the tokens (CRYPTO_TOKEN_KEYS). nil → no
	// link is minted and every link is refused (fail closed).
	Deriver *crypto.Deriver
}

func (c UnsubscribeConfig) validate() error {
	if c.TTL < 0 {
		return errors.New("consent.Deps.Unsubscribe.TTL is negative")
	}
	if c.BaseURL == "" {
		return nil
	}
	if c.Deriver == nil {
		return errors.New("consent.Deps.Unsubscribe.BaseURL needs a Deriver (CRYPTO_TOKEN_KEYS)")
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return errors.New("consent.Deps.Unsubscribe.BaseURL must be an absolute https URL without query or fragment")
	}
	if n := len(c.Deriver.ActiveKeyID()); n == 0 || n > maxKeyID {
		return fmt.Errorf("consent: unsubscribe key ID must be 1 to %d characters", maxKeyID)
	}
	return nil
}

func (c UnsubscribeConfig) enabled() bool { return c.BaseURL != "" && c.Deriver != nil }

func (c UnsubscribeConfig) ttl() time.Duration {
	if c.TTL == 0 {
		return DefaultUnsubscribeTTL
	}
	return c.TTL
}

// UnsubscribeLinks mints the one-click unsubscribe links of commercial
// e-mail (BE-062). outreach puts one per recipient in notify.Message
// (List-Unsubscribe, List-Unsubscribe-Post and the footer).
type UnsubscribeLinks interface {
	// UnsubscribeURL returns the https link that withdraws userID's consent
	// to purpose: the base URL, "/" and a signed token that expires after
	// the configured TTL. ErrUnsubscribeUnavailable without a base URL; an
	// error for a purpose that has no link (only commercial_email has).
	UnsubscribeURL(userID uuid.UUID, purpose Purpose) (string, error)
}

// UnsubscribeLink is what getUnsubscribeLink shows: the purpose the link
// withdraws, nothing about the subject or the consent state.
type UnsubscribeLink struct {
	Purpose Purpose
}

// UnsubscribeURL implements UnsubscribeLinks.
func (s *service) UnsubscribeURL(userID uuid.UUID, purpose Purpose) (string, error) {
	u := s.d.Unsubscribe
	if !u.enabled() {
		return "", ErrUnsubscribeUnavailable
	}
	tok, err := mintUnsubscribe(u.Deriver, userID, purpose, s.now().Add(u.ttl()))
	if err != nil {
		return "", err
	}
	return strings.TrimRight(u.BaseURL, "/") + "/" + tok, nil
}

// mintUnsubscribe signs the token of (userID, purpose) expiring at exp with
// the active key.
func mintUnsubscribe(d *crypto.Deriver, userID uuid.UUID, purpose Purpose, exp time.Time) (string, error) {
	tag, ok := unsubscribeTags[purpose]
	switch {
	case !ok:
		return "", fmt.Errorf("consent: purpose %s has no unsubscribe link", purpose)
	case userID == uuid.Nil():
		return "", errors.New("consent: unsubscribe link without a user")
	case exp.Unix() <= 0:
		return "", errors.New("consent: unsubscribe link expiry before the epoch")
	}
	kid := d.ActiveKeyID()
	if len(kid) == 0 || len(kid) > maxKeyID {
		return "", fmt.Errorf("consent: unsubscribe key ID must be 1 to %d characters", maxKeyID)
	}
	head := make([]byte, 0, unsubscribeFixed+1+len(kid)+unsubscribeMAC)
	head = append(head, unsubscribeVersion, tag)
	head = append(head, userID[:]...)
	head = binary.BigEndian.AppendUint64(head, uint64(exp.Unix())) //nolint:gosec // G115: positive, checked above
	head = append(head, byte(len(kid)))                            //nolint:gosec // G115: 1..64, checked above
	head = append(head, kid...)
	mac, err := d.DerivedToken(unsubscribeDerive, kid, head)
	if err != nil {
		return "", fmt.Errorf("consent: unsubscribe link: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(append(head, mac...)), nil
}

// unsubscribeClaim is what a verified token says.
type unsubscribeClaim struct {
	UserID  uuid.UUID
	Purpose Purpose
}

// verifyUnsubscribe checks raw and returns its claim, or the reason it is
// refused ("" on success). The reason is for the log only: every refusal
// is the same error to the caller. The MAC is checked before anything in
// the token is trusted, in constant time.
func verifyUnsubscribe(d *crypto.Deriver, raw string, now time.Time) (unsubscribeClaim, string) {
	if d == nil {
		return unsubscribeClaim{}, "not_configured"
	}
	// Even Strict base64 decoding ignores CR/LF. A path credential must
	// use the minted base64url spelling, without embedded line breaks.
	if raw == "" || len(raw) > MaxUnsubscribeToken || strings.ContainsAny(raw, "\r\n") {
		return unsubscribeClaim{}, "malformed"
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	if err != nil || len(b) < unsubscribeFixed+2+unsubscribeMAC {
		return unsubscribeClaim{}, "malformed"
	}
	n := int(b[unsubscribeFixed])
	if n == 0 || n > maxKeyID || len(b) != unsubscribeFixed+1+n+unsubscribeMAC {
		return unsubscribeClaim{}, "malformed"
	}
	head, mac := b[:unsubscribeFixed+1+n], b[unsubscribeFixed+1+n:]
	want, err := d.DerivedToken(unsubscribeDerive, string(head[unsubscribeFixed+1:]), head)
	if err != nil {
		return unsubscribeClaim{}, "unknown_key"
	}
	if subtle.ConstantTimeCompare(mac, want) != 1 {
		return unsubscribeClaim{}, "bad_mac"
	}
	// Signed by us from here on.
	if head[0] != unsubscribeVersion {
		return unsubscribeClaim{}, "version"
	}
	purpose, ok := purposeOfTag(head[1])
	if !ok {
		return unsubscribeClaim{}, "purpose"
	}
	var uid uuid.UUID
	copy(uid[:], head[2:18])
	exp := binary.BigEndian.Uint64(head[18:unsubscribeFixed])
	switch {
	case uid == uuid.Nil():
		return unsubscribeClaim{}, "malformed"
	case exp > math.MaxInt64 || !now.Before(time.Unix(int64(exp), 0)):
		return unsubscribeClaim{}, "expired"
	}
	return unsubscribeClaim{UserID: uid, Purpose: purpose}, ""
}

// unsubscribeSubject verifies raw and authorises its subject: the token is
// the subject's proof of identity for the withdrawal of its own consent
// (consent.record.create, O cell; the HTTP caller — anonymous, a mailbox
// provider or another signed-in user — is not the actor).
func (s *service) unsubscribeSubject(ctx context.Context, raw string) (unsubscribeClaim, authz.Actor, error) {
	c, reason := verifyUnsubscribe(s.d.Unsubscribe.Deriver, raw, s.now())
	if reason != "" {
		s.d.Log.LogAttrs(ctx, slog.LevelInfo, "unsubscribe link refused", slog.String("reason", reason))
		return unsubscribeClaim{}, authz.Actor{}, ErrInvalidUnsubscribeLink
	}
	subject, uid := authz.Actor{Kind: authz.KindUser, UserID: c.UserID}, c.UserID
	err := authz.Can(ctx, subject, authz.ConsentRecordCreate, authz.Resource{Type: authz.TypeConsentRecord, OwnerUserID: &uid})
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
