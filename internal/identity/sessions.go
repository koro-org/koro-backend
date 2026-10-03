package identity

import (
	"context"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/httpx"
	"koro/internal/platform/page"
	"koro/internal/platform/textx"
	"koro/internal/platform/token"
)

// Login is the body of createSession.
type Login struct {
	Email       string
	Password    string
	Client      ClientKind // X-Koro-Client (default web)
	DeviceLabel string
}

// String keeps the password out of %v.
func (l Login) String() string { return "identity.Login{" + string(l.Client) + "}" }

// LoginResult is a successful login: either a session (Token is the raw
// opaque token, returned exactly once) or, for an MFA account, a challenge
// (03 §6.4-1; completed by completeMfaLogin, BE-016).
type LoginResult struct {
	Session     Session
	Token       string
	MFARequired bool
	Challenge   MFAChallenge
}

// String keeps the token out of %v.
func (LoginResult) String() string { return "identity.LoginResult([REDACTED])" }

// SessionQuery is one page of listMySessions.
type SessionQuery struct {
	Limit int
	After *uuid.UUID // the last session ID of the previous page
}

// SessionPage is one page of the caller's live sessions, newest first.
type SessionPage struct {
	Items   []Session
	Current uuid.UUID // the caller's session
	More    bool
}

// CreateSession implements Service (createSession). Every outcome that
// does not prove the password costs one argon2id verification and gives
// the same 401: unknown address, wrong password, locked, suspended or
// passwordless account (05 §14.1, ASVS 6.3.8).
func (s *service) CreateSession(ctx context.Context, a authz.Actor, in Login) (LoginResult, error) {
	if err := authz.Can(ctx, a, authz.IdentitySessionCreate, publicUser); err != nil {
		return LoginResult{}, err
	}
	if s.d.Hasher == nil {
		return LoginResult{}, ErrUnavailable
	}
	if in.Client != ClientMobile {
		in.Client = ClientWeb
	}
	label, err := deviceLabel(in.DeviceLabel)
	if err != nil {
		return LoginResult{}, err
	}
	now := s.now()
	u, found, err := s.d.Store.UserByEmail(ctx, textx.NormalizeEmail(in.Email))
	if err != nil {
		return LoginResult{}, err
	}
	if !found || u.PasswordHash == "" || u.Status == StatusErased {
		if err := s.d.Hasher.VerifyDummy(ctx, in.Password); err != nil {
			return LoginResult{}, err
		}
		return LoginResult{}, ErrInvalidCredentials
	}
	if u.locked(now) {
		if err := s.d.Hasher.VerifyDummy(ctx, in.Password); err != nil {
			return LoginResult{}, err
		}
		if err := s.recordFailure(ctx, u.ID, authz.IdentitySessionCreate, "locked"); err != nil {
			return LoginResult{}, err
		}
		return LoginResult{}, ErrInvalidCredentials
	}
	ok, rehash, err := s.d.Hasher.Verify(ctx, in.Password, u.PasswordHash)
	if err != nil {
		s.d.Log.LogAttrs(ctx, slog.LevelError, "stored password hash is unusable", slog.String("user_id", u.ID.String()),
			slog.String("err", err.Error()))
		ok = false
	}
	if !ok {
		if err := s.recordFailure(ctx, u.ID, authz.IdentitySessionCreate, "invalid_credentials"); err != nil {
			return LoginResult{}, err
		}
		return LoginResult{}, ErrInvalidCredentials
	}
	switch u.Status {
	case StatusActive:
	case StatusPendingEmail:
		// The password is right: 403 tells nothing the caller does not know.
		if err := s.resetFailures(ctx, u); err != nil {
			return LoginResult{}, err
		}
		return LoginResult{}, ErrEmailNotVerified
	default: // suspended (03 §2.2-3)
		return LoginResult{}, ErrInvalidCredentials
	}
	newHash := ""
	if rehash {
		if newHash, err = s.d.Hasher.Hash(ctx, in.Password); err != nil {
			return LoginResult{}, err
		}
	}
	if u.MFAEnabled {
		return s.beginMFA(ctx, u, in.Client, label)
	}

	roles, err := s.d.Store.PlatformRoles(ctx, u.ID)
	if err != nil {
		return LoginResult{}, err
	}
	ms, err := s.memberships(ctx, u.ID)
	if err != nil {
		return LoginResult{}, err
	}
	pol := policyFor(in.Client, privileged(roles, ms))
	raw, hash, err := token.New()
	if err != nil {
		return LoginResult{}, err
	}
	sess := Session{
		ID: uuid.NewV7(), UserID: u.ID, TokenHash: hash, ClientKind: in.Client, CreatedAt: now, LastSeenAt: now,
		IdleExpiresAt: now.Add(pol.Idle), AbsoluteExpiresAt: now.Add(pol.Absolute),
		IP: canonicalIP(httpx.ClientIPFrom(ctx)), UserAgent: cut(httpx.UserAgentFrom(ctx), maxUserAgent), DeviceLabel: label,
	}
	prevLogin := time.Time{}
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		cur, err := s.d.Store.LockUser(ctx, u.ID)
		if err != nil {
			return err
		}
		if cur.LastLoginAt != nil {
			prevLogin = *cur.LastLoginAt
		}
		cur.FailedLogins, cur.LockedUntil, cur.LastLoginAt = 0, nil, &now
		if newHash != "" && cur.PasswordHash == u.PasswordHash {
			cur.PasswordHash = newHash // parameters changed (05 §14.1 needsRehash)
		}
		if err := s.d.Store.UpdateUser(ctx, cur); err != nil {
			return err
		}
		if err := s.d.Store.InsertSession(ctx, sess); err != nil {
			return err
		}
		return s.d.Audit.Log(ctx, byUser(event(authz.IdentitySessionCreate, audit.Success, resourceSession, sess.ID,
			map[string]any{"client_kind": string(sess.ClientKind)}), u.ID))
	})
	if err != nil {
		return LoginResult{}, err
	}
	s.markActiveDay(ctx, u.ID, prevLogin, now)
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "session created", slog.String("user_id", u.ID.String()),
		slog.String("client_kind", string(sess.ClientKind)))
	return LoginResult{Session: sess, Token: raw}, nil
}

// beginMFA answers the first step of an MFA login: no session yet
// (03 §6.4-1).
func (s *service) beginMFA(ctx context.Context, u User, client ClientKind, label string) (LoginResult, error) {
	if s.d.MFA == nil {
		return LoginResult{}, ErrMFAUnavailable
	}
	if err := s.resetFailures(ctx, u); err != nil {
		return LoginResult{}, err
	}
	ch, err := s.d.MFA.Begin(ctx, u.ID, client, label)
	if err != nil {
		return LoginResult{}, err
	}
	return LoginResult{MFARequired: true, Challenge: ch}, nil
}

// deviceLabel normalizes the optional device label (≤ 100 characters).
func deviceLabel(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	l, err := textx.NormalizeText(raw)
	if err != nil || len([]rune(l)) > maxDeviceLabel {
		return "", invalid(ErrInvalidName, "body.device_label", "must be at most 100 characters of plain text")
	}
	return l, nil
}

// recordFailure counts a failed login or re-authentication against the
// account and locks it with exponential back-off (05 §14.1), in its own
// transaction so that it commits although the request fails. action is
// audited as denied (empty: the caller audits, e.g. consent's R operations).
func (s *service) recordFailure(ctx context.Context, userID uuid.UUID, action authz.Action, reason string) error {
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		u, err := s.d.Store.LockUser(ctx, userID)
		if err != nil {
			return err
		}
		now := s.now()
		u.FailedLogins = min(u.FailedLogins+1, maxFailedLogins)
		if d := lockFor(u.FailedLogins); d > 0 {
			until := now.Add(d)
			u.LockedUntil = &until
		}
		if err := s.d.Store.UpdateUser(ctx, u); err != nil {
			return err
		}
		if u.LockedUntil != nil && u.FailedLogins == lockoutThreshold {
			s.d.Log.LogAttrs(ctx, slog.LevelWarn, "account locked after repeated failures", slog.String("user_id", u.ID.String()))
		}
		if action == "" {
			return nil
		}
		return s.d.Audit.Log(ctx, event(action, audit.Denied, resourceUser, u.ID,
			map[string]any{"result": reason, "failed_attempts": u.FailedLogins}))
	})
}

// resetFailures clears the lockout counter after a proven password.
func (s *service) resetFailures(ctx context.Context, u User) error {
	if u.FailedLogins == 0 && u.LockedUntil == nil {
		return nil
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		cur, err := s.d.Store.LockUser(ctx, u.ID)
		if err != nil {
			return err
		}
		cur.FailedLogins, cur.LockedUntil = 0, nil
		return s.d.Store.UpdateUser(ctx, cur)
	})
}

// memberships calls the late-bound membership source (none without one).
func (s *service) memberships(ctx context.Context, userID uuid.UUID) ([]authz.Membership, error) {
	if s.d.Memberships == nil {
		return nil, nil
	}
	ms, err := s.d.Memberships(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("identity: memberships: %w", err)
	}
	return ms, nil
}

// Authenticate implements Authenticator (03 §2.2). Rejections are all the
// same ErrUnauthenticated; only server failures differ.
func (s *service) Authenticate(ctx context.Context, cred httpx.Credential) (Principal, error) {
	var zero [32]byte
	if cred.TokenHash == zero || (cred.Transport != httpx.TransportCookie && cred.Transport != httpx.TransportBearer) {
		return Principal{}, ErrUnauthenticated
	}
	sess, found, err := s.d.Store.SessionByTokenHash(ctx, cred.TokenHash[:])
	if err != nil {
		return Principal{}, err
	}
	if !found {
		return Principal{}, ErrUnauthenticated
	}
	// 03 §2.2-1: a web session travels only as the cookie, a mobile one
	// only as a bearer token (a stolen cookie is not a bearer token).
	if (sess.ClientKind == ClientWeb) != (cred.Transport == httpx.TransportCookie) {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "session presented over the wrong transport", slog.String("user_id", sess.UserID.String()))
		return Principal{}, ErrUnauthenticated
	}
	now := s.now()
	if !sess.live(now) {
		return Principal{}, ErrUnauthenticated
	}
	u, err := s.d.Store.User(ctx, sess.UserID)
	switch {
	case isNotFound(err):
		return Principal{}, ErrUnauthenticated
	case err != nil:
		return Principal{}, err
	}
	if u.Status != StatusActive {
		return Principal{}, ErrUnauthenticated
	}
	roles, err := s.d.Store.PlatformRoles(ctx, u.ID)
	if err != nil {
		return Principal{}, err
	}
	ms, err := s.memberships(ctx, u.ID)
	if err != nil {
		return Principal{}, err
	}
	// The policy of the user's current roles caps the stored windows: a
	// session opened before an MFA role arrived lives 30 min / 12 h at most
	// even if a role_change revocation was missed (03 §6.4).
	pol := policyFor(sess.ClientKind, privileged(roles, ms))
	if !now.Before(sess.LastSeenAt.Add(pol.Idle)) || !now.Before(sess.CreatedAt.Add(pol.Absolute)) {
		return Principal{}, ErrUnauthenticated
	}
	if now.Sub(sess.LastSeenAt) >= touchEvery {
		idle := now.Add(pol.Idle)
		if abs := sess.AbsoluteExpiresAt; idle.After(abs) {
			idle = abs
		}
		if err := s.d.Store.TouchSession(ctx, sess.ID, now, idle); err != nil {
			return Principal{}, err
		}
		s.markActiveDay(ctx, u.ID, sess.LastSeenAt, now)
	}
	return Principal{
		UserID: u.ID, SessionID: sess.ID, ClientKind: sess.ClientKind, MFAVerified: sess.MFAVerifiedAt != nil,
		PlatformRoles: roles, Memberships: ms, BirthDate: u.clone().BirthDate,
	}, nil
}

// DeleteCurrentSession implements Service (logout).
func (s *service) DeleteCurrentSession(ctx context.Context, a authz.Actor) error {
	me := a.UserID
	res := authz.Resource{Type: authz.TypeSession, ID: a.SessionID, OwnerUserID: &me}
	if err := authz.Can(ctx, a, authz.IdentitySessionDeleteCurrent, res); err != nil {
		return err
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		revoked, err := s.d.Store.RevokeSession(ctx, a.UserID, a.SessionID, s.now(), RevokeLogout)
		if err != nil || !revoked {
			return err
		}
		return s.d.Audit.Log(ctx, event(authz.IdentitySessionDeleteCurrent, audit.Success, resourceSession, a.SessionID, nil).By(a))
	})
}

// ListMySessions implements Service (listMySessions): live sessions only;
// IP and user agent stay in the database (04 §2.1).
func (s *service) ListMySessions(ctx context.Context, a authz.Actor, q SessionQuery) (SessionPage, error) {
	me := a.UserID
	if err := authz.Can(ctx, a, authz.IdentitySessionListOwn, authz.Resource{Type: authz.TypeSession, OwnerUserID: &me}); err != nil {
		return SessionPage{}, err
	}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.ActiveSessions(ctx, a.UserID, s.now(), q.After, limit+1)
	if err != nil {
		return SessionPage{}, err
	}
	items, more := page.Cut(rows, limit)
	return SessionPage{Items: items, Current: a.SessionID, More: more}, nil
}

// RevokeMySession implements Service (revokeMySession). Another user's
// session is 404 (O cell, 03 §5.0); revoking the current one is a logout.
func (s *service) RevokeMySession(ctx context.Context, a authz.Actor, id uuid.UUID) error {
	sess, err := s.d.Store.Session(ctx, id)
	switch {
	case isNotFound(err):
		if err := authz.Can(ctx, a, authz.IdentitySessionRevokeOwn, authz.Resource{Type: authz.TypeSession, ID: id}); err != nil {
			return err // anonymous → 401 first; everyone else → 404
		}
		return ErrNotFound
	case err != nil:
		return err
	}
	if err := authz.Can(ctx, a, authz.IdentitySessionRevokeOwn, sessionResource(sess)); err != nil {
		return err
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		revoked, err := s.d.Store.RevokeSession(ctx, a.UserID, id, s.now(), RevokeLogout)
		if err != nil || !revoked {
			return err
		}
		return s.d.Audit.Log(ctx, event(authz.IdentitySessionRevokeOwn, audit.Success, resourceSession, id, nil).By(a))
	})
}

// RevokeAll implements Sessions.
func (s *service) RevokeAll(ctx context.Context, userID uuid.UUID, reason RevokeReason) (int, error) {
	if !reason.valid() {
		return 0, fmt.Errorf("identity: revoke reason %q", reason)
	}
	var n int
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		var err error
		n, err = s.d.Store.RevokeUserSessions(ctx, userID, nil, s.now(), reason)
		return err
	})
	return n, err
}
