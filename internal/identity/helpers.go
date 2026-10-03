package identity

import (
	"context"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"net/netip"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/civil"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/httpx"
	"koro/internal/platform/problem"
	"koro/internal/platform/ratelimit"
	"koro/internal/platform/textx"
	"koro/internal/platform/token"
)

// Field limits (02 §5.1, §5.2).
const (
	maxEmailLen       = 254
	maxNameLen        = 100
	maxUserAgent      = 400
	maxDeviceLabel    = 100
	resourceUser      = "user"
	resourceSession   = "session"
	resourceNotice    = "notice_version"
	resourceRole      = "platform_role_assignment"
	auditNoticeShown  = "notice.shown" // 02 §4.2: a privacy notice is shown, never consented to
	tokenDerivePurpo  = "identity.email_token"
	emailDigestPurpo  = "identity.rate_limit_email"
	emailTokenLen     = 52 // base32 (no padding) of 32 bytes
	emailTokenAggType = "email_token"
)

// invalid builds a 422 naming the offending request member.
func invalid(sentinel *problem.Error, location, message string) error {
	return sentinel.WithFields(problem.FieldError{Location: location, Message: message})
}

// parseEmail validates a plain address ("ada@example.com": no display name,
// no comments, a dotted domain, at most 254 characters) and returns it
// trimmed with its normalized form.
func parseEmail(raw, location string) (email, normalized string, err error) {
	e := strings.TrimSpace(raw)
	bad := invalid(ErrInvalidEmail, location, "must be a plain e-mail address")
	if e == "" || len(e) > maxEmailLen || !utf8.ValidString(e) || strings.ContainsFunc(e, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return "", "", bad
	}
	a, perr := mail.ParseAddress(e)
	if perr != nil || a.Name != "" || a.Address != e {
		return "", "", bad
	}
	local, domain, ok := strings.Cut(e, "@")
	if !ok || local == "" || strings.Count(e, "@") != 1 || !strings.Contains(domain, ".") ||
		strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return "", "", bad
	}
	return e, textx.NormalizeEmail(e), nil
}

// cleanName normalizes a first or last name (05 §9.5) and checks 1–100
// characters.
func cleanName(raw, location string) (string, error) {
	n, err := textx.NormalizeText(raw)
	if err != nil || n == "" || utf8.RuneCountInString(n) > maxNameLen {
		return "", invalid(ErrInvalidName, location, "must be 1-100 characters of plain text")
	}
	return n, nil
}

// checkLocale accepts tr-TR and en-US.
func checkLocale(l, location string) error {
	if l != LocaleTR && l != LocaleEN {
		return invalid(problem.ErrValidation, location, "must be tr-TR or en-US")
	}
	return nil
}

// checkBirthDate applies the age gate's plausibility rule: on or after
// 1900-01-01 (users_birth_date_chk) and not after today (Europe/Istanbul).
func checkBirthDate(d civil.Date, now time.Time) error {
	first, _ := civil.New(1900, time.January, 1)
	if !d.IsValid() || d.Before(first) || d.After(civil.Today(now)) {
		return invalid(ErrInvalidBirthDate, "body.birth_date", "must be a past date on or after 1900-01-01")
	}
	return nil
}

// canonicalIP returns the canonical text of an IP address ("" if s is not
// one), as PostgreSQL's host(inet) prints a host address.
func canonicalIP(s string) string {
	a, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil {
		return ""
	}
	return a.Unmap().WithZone("").String()
}

func cut(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// ownUser is the authz view of the actor's own account (03 §3.5: user,
// OwnerUserID = the account).
func ownUser(a authz.Actor) authz.Resource {
	id := a.UserID
	return authz.Resource{Type: authz.TypeUser, ID: id, OwnerUserID: &id}
}

// publicUser is the resource of the public account operations: the target
// account is unknown (and must stay unknown) when authz decides.
var publicUser = authz.Resource{Type: authz.TypeUser}

// sessionResource is the authz view of a session (owner = its user).
func sessionResource(s Session) authz.Resource {
	owner := s.UserID
	return authz.Resource{Type: authz.TypeSession, ID: s.ID, OwnerUserID: &owner}
}

// event builds an audit event of action on a resource.
func event(act authz.Action, outcome audit.Outcome, resType string, resID uuid.UUID, meta map[string]any) audit.Event {
	id := resID
	return audit.Event{Action: string(act), Outcome: outcome, ResourceType: resType, ResourceID: &id, Metadata: meta}
}

// byUser sets the actor of an audit event to a user that has no session
// actor yet (registration, token confirmations, login).
func byUser(e audit.Event, userID uuid.UUID) audit.Event {
	return e.By(authz.Actor{Kind: authz.KindUser, UserID: userID})
}

// deriveToken re-creates the e-mail token of a token row: base32 of
// DerivedToken("identity.email_token", keyID, id). Only the worker (for
// the e-mail) and the issuer (for the stored hash) call it.
func (s *service) deriveToken(keyID string, id uuid.UUID) (string, error) {
	b, err := s.d.Deriver.DerivedToken(tokenDerivePurpo, keyID, id[:])
	if err != nil {
		return "", fmt.Errorf("identity: derive token: %w", err)
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b), nil
}

// tokenHash normalizes a token typed or pasted by a user (spaces removed,
// upper case) and returns its SHA-256; ok=false when it cannot be a token.
func tokenHash(raw string) (hash []byte, ok bool) {
	t := strings.ToUpper(strings.Join(strings.Fields(raw), ""))
	if len(t) != emailTokenLen {
		return nil, false
	}
	for _, r := range t {
		if (r < 'A' || r > 'Z') && (r < '2' || r > '7') {
			return nil, false
		}
	}
	return token.Hash(t), true
}

// emailDigest is the pseudonymous rate limit key of an address: an HMAC
// under the token keys, so Redis never holds a guessable hash of an
// e-mail address.
func (s *service) emailDigest(normalized string) (string, error) {
	b, err := s.d.Deriver.DerivedToken(emailDigestPurpo, s.d.Deriver.ActiveKeyID(), []byte(normalized))
	if err != nil {
		return "", fmt.Errorf("identity: e-mail digest: %w", err)
	}
	return hex.EncodeToString(b[:12]), nil
}

// allow applies a strict rate limit rule in the service (06 H-6): an
// unavailable limiter fails closed (503), an exhausted one is 429.
func (s *service) allow(ctx context.Context, rule ratelimit.Rule, key string) error {
	res, err := s.d.Limiter.Allow(ctx, rule, key)
	switch {
	case err != nil:
		s.d.Log.LogAttrs(ctx, slog.LevelError, "rate limiter unavailable (fail closed)",
			slog.String("rule", rule.Name), slog.String("err", err.Error()))
		return problem.ErrUnavailable.Wrap(err)
	case !res.Allowed:
		return problem.ErrRateLimited
	}
	return nil
}

// limitEmail applies auth_email (or another rule) per address and per
// (client IP, address): a burst against one victim from many IPs and a
// burst from one IP both stop (04 §1.9 "ip + e-posta özeti").
func (s *service) limitEmail(ctx context.Context, rule ratelimit.Rule, normalized string) error {
	digest, err := s.emailDigest(normalized)
	if err != nil {
		return err
	}
	if err := s.allow(ctx, rule, "email:"+digest); err != nil {
		return err
	}
	ip := canonicalIP(httpx.ClientIPFrom(ctx))
	if ip == "" {
		ip = "unknown"
	}
	return s.allow(ctx, rule, "ip:"+ip+"|email:"+digest)
}

// markActiveDay emits user.active_day (A) on the first activity of a
// Europe/Istanbul day (01 §6.2; talent counts login_active_day). prev is
// the previous activity (zero: none).
func (s *service) markActiveDay(ctx context.Context, userID uuid.UUID, prev, now time.Time) {
	today := civil.Today(now)
	if !prev.IsZero() && civil.Today(prev) == today {
		return
	}
	env, err := domainevent.New(domainevent.UserActiveDay{UserID: userID, Day: today}, resourceUser, userID)
	if err == nil {
		err = s.d.Outbox.AfterCommit(ctx, env)
	}
	if err != nil {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "user.active_day not emitted",
			slog.String("user_id", userID.String()), slog.String("err", err.Error()))
	}
}

// isNotFound reports a not-found answer of the store.
func isNotFound(err error) bool { return errors.Is(err, ErrNotFound) }
