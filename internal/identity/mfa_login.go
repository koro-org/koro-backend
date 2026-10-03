package identity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/clock"
	"koro/internal/platform/problem"
)

// Two-step login of an MFA account (03 §6.4-1, 06 H-16): the password step
// opens no session; it stores a challenge (Redis, 5 minutes, single use)
// and answers its ID. completeMfaLogin takes the challenge (GETDEL) and
// opens the session with mfa_verified_at only after a TOTP or recovery
// code. A half-authenticated session never exists.
const (
	// ChallengeTTL is the lifetime of a login challenge.
	ChallengeTTL = 5 * time.Minute
	// maxChallengeAttempts wrong codes end a challenge; the user signs in
	// again (the password step counts towards the lockout as well).
	maxChallengeAttempts = 5
	challengeKeyPrefix   = "identity:mfa_challenge:"
)

// MFALogin is the body of completeMfaLogin: exactly one of TOTPCode and
// RecoveryCode.
type MFALogin struct {
	ChallengeID  uuid.UUID
	TOTPCode     string
	RecoveryCode string
}

// String keeps the codes out of %v.
func (MFALogin) String() string { return "identity.MFALogin([REDACTED])" }

// challenge is what the store keeps for a challenge (no secret: the user,
// the client of the password step and the attempt count).
type challenge struct {
	UserID      uuid.UUID  `json:"user_id"`
	Client      ClientKind `json:"client"`
	DeviceLabel string     `json:"device_label,omitempty"`
	ExpiresAt   time.Time  `json:"expires_at"`
	Attempts    int        `json:"attempts"`
}

// challengeKey is the store key of a challenge ID: its SHA-256, so the
// store (Redis) never holds a usable challenge ID.
func challengeKey(id uuid.UUID) string {
	sum := sha256.Sum256(id[:])
	return challengeKeyPrefix + hex.EncodeToString(sum[:])
}

func (s *service) challengesReady() error {
	if s.d.Challenges == nil || s.d.Secrets == nil {
		return ErrMFAUnavailable
	}
	return nil
}

// beginMFA answers the password step of an MFA account: a challenge, no
// session (03 §6.4-1). The lockout counter is NOT reset here: only a
// completed second factor resets it, so a password holder cannot guess
// codes without the account locking up (05 §14.1).
func (s *service) beginMFA(ctx context.Context, u User, client ClientKind, label, newHash string) (LoginResult, error) {
	if err := s.challengesReady(); err != nil {
		return LoginResult{}, err
	}
	if newHash != "" {
		err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
			cur, err := s.d.Store.LockUser(ctx, u.ID)
			if err != nil || cur.PasswordHash != u.PasswordHash {
				return err
			}
			cur.PasswordHash = newHash
			return s.d.Store.UpdateUser(ctx, cur)
		})
		if err != nil {
			return LoginResult{}, err
		}
	}
	// A random (v4) ID: the challenge is a short-lived bearer secret, so it
	// must not be a time-ordered UUIDv7 (05 §9.1).
	id := uuid.NewV4()
	c := challenge{UserID: u.ID, Client: client, DeviceLabel: label, ExpiresAt: s.now().Add(ChallengeTTL)}
	b, err := json.Marshal(c)
	if err != nil {
		return LoginResult{}, fmt.Errorf("identity: challenge: %w", err)
	}
	if err := s.d.Challenges.Put(ctx, challengeKey(id), b, ChallengeTTL); err != nil {
		return LoginResult{}, fmt.Errorf("identity: store challenge: %w", err)
	}
	return LoginResult{MFARequired: true, Challenge: MFAChallenge{ID: id, ExpiresAt: c.ExpiresAt}}, nil
}

// CompleteMFALogin implements Service (completeMfaLogin). An unknown,
// expired or used challenge and a wrong code give the same 401
// identity.mfa_login_failed. A wrong code counts towards the account
// lockout (checked before every verification and never reset by the
// password step, so guessing locks the account with exponential back-off)
// and the per-user reauth class; the challenge survives up to
// maxChallengeAttempts wrong codes within its lifetime.
func (s *service) CompleteMFALogin(ctx context.Context, a authz.Actor, in MFALogin) (LoginResult, error) {
	if err := authz.Can(ctx, a, authz.IdentitySessionCreate, publicUser); err != nil {
		return LoginResult{}, err
	}
	if err := s.challengesReady(); err != nil {
		return LoginResult{}, err
	}
	if (in.TOTPCode == "") == (in.RecoveryCode == "") {
		return LoginResult{}, problem.ErrValidation.WithFields(problem.FieldError{
			Location: "body", Message: "exactly one of totp_code and recovery_code is required"})
	}
	key := challengeKey(in.ChallengeID)
	raw, found, err := s.d.Challenges.Take(ctx, key)
	if err != nil {
		return LoginResult{}, fmt.Errorf("identity: take challenge: %w", err)
	}
	if !found {
		return LoginResult{}, ErrMFALoginFailed
	}
	var c challenge
	if err := json.Unmarshal(raw, &c); err != nil {
		return LoginResult{}, fmt.Errorf("identity: challenge: %w", err)
	}
	now := s.now()
	if !now.Before(c.ExpiresAt) {
		return LoginResult{}, ErrMFALoginFailed
	}
	u, err := s.d.Store.User(ctx, c.UserID)
	switch {
	case isNotFound(err):
		return LoginResult{}, ErrMFALoginFailed
	case err != nil:
		return LoginResult{}, err
	}
	if u.Status != StatusActive || !u.MFAEnabled || u.locked(now) {
		return LoginResult{}, ErrMFALoginFailed
	}
	kind := proofTOTP
	var ok bool
	if in.TOTPCode != "" {
		ok, err = s.verifyTOTP(ctx, u.ID, in.TOTPCode)
	} else {
		kind = "recovery_code"
		ok, err = s.useRecoveryCode(ctx, u.ID, in.RecoveryCode)
	}
	if err != nil {
		return LoginResult{}, err
	}
	if !ok {
		return LoginResult{}, s.mfaLoginFailed(ctx, u, key, c)
	}
	res, err := s.openSession(ctx, u, c.Client, c.DeviceLabel, "", map[string]any{"mfa": kind})
	if err != nil {
		return LoginResult{}, err
	}
	if kind == "recovery_code" {
		left, err := s.d.Store.RecoveryCodesLeft(ctx, u.ID)
		if err != nil {
			s.d.Log.LogAttrs(ctx, slog.LevelWarn, "recovery codes not counted", slog.String("err", err.Error()))
		}
		s.notifyUser(ctx, u.ID, NoticeRecoveryCodeUsed)
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "recovery code used for sign-in", slog.String("user_id", u.ID.String()),
			slog.Int("recovery_codes_left", left))
	}
	return res, nil
}

// mfaLoginFailed records a wrong second factor and gives the challenge back
// (same expiry) unless its attempts are used up.
func (s *service) mfaLoginFailed(ctx context.Context, u User, key string, c challenge) error {
	if err := s.recordFailure(ctx, u.ID, authz.IdentitySessionCreate, "invalid_mfa_code"); err != nil {
		return err
	}
	if err := s.allow(ctx, ruleReauth, "user:"+u.ID.String()+":mfa_login"); err != nil {
		return err
	}
	c.Attempts++
	if ttl := c.ExpiresAt.Sub(s.now()); c.Attempts < maxChallengeAttempts && ttl > 0 {
		b, err := json.Marshal(c)
		if err == nil {
			err = s.d.Challenges.Put(ctx, key, b, ttl)
		}
		if err != nil {
			s.d.Log.LogAttrs(ctx, slog.LevelWarn, "mfa challenge not restored", slog.String("err", err.Error()))
		}
	}
	return ErrMFALoginFailed // the denied login is audited by recordFailure
}

// SeedChallenge stores a login challenge with a chosen ID, as the password
// step would. Only fixtures use it (the authz matrix seeds a challenge it
// can name in request bodies); production challenges come from
// createSession with random IDs.
func SeedChallenge(ctx context.Context, store ChallengeStore, id, userID uuid.UUID, client ClientKind, expiresAt, now time.Time) error {
	b, err := json.Marshal(challenge{UserID: userID, Client: client, ExpiresAt: expiresAt})
	if err != nil {
		return err
	}
	return store.Put(ctx, challengeKey(id), b, expiresAt.Sub(now))
}

// MemChallenges is the in-memory ChallengeStore of tests and wiring tests:
// expiry follows the given clock.
type MemChallenges struct {
	mu    sync.Mutex
	clock clock.Clock
	items map[string]memChallenge
}

type memChallenge struct {
	value   []byte
	expires time.Time
}

// NewMemChallenges returns an empty in-memory challenge store.
func NewMemChallenges(c clock.Clock) *MemChallenges {
	return &MemChallenges{clock: c, items: map[string]memChallenge{}}
}

var _ ChallengeStore = (*MemChallenges)(nil)

// Put implements ChallengeStore.
func (m *MemChallenges) Put(_ context.Context, key string, value []byte, ttl time.Duration) error {
	if ttl <= 0 {
		return fmt.Errorf("identity: challenge ttl %s", ttl)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[key] = memChallenge{value: append([]byte(nil), value...), expires: m.clock.Now().Add(ttl)}
	return nil
}

// Take implements ChallengeStore.
func (m *MemChallenges) Take(_ context.Context, key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	it, ok := m.items[key]
	delete(m.items, key)
	if !ok || !m.clock.Now().Before(it.expires) {
		return nil, false, nil
	}
	return it.value, true, nil
}

// Len returns how many challenges the store holds (tests).
func (m *MemChallenges) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.items)
}
