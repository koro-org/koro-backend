package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"uuid"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/problem"
)

// TOTP (RFC 6238; 05 §14.1): SHA1, 6 digits, 30-second steps, one step of
// tolerance either way (Skew 1). pquerna/otp computes the codes; it does
// not stop a code from being used twice, so the last accepted step of each
// user is stored (mfa_totp.last_used_step) and a code of that step or an
// earlier one is refused (UseTOTPStep: one conditional UPDATE).
const (
	totpIssuer     = "Koro"
	totpPeriod     = 30
	totpDigits     = otp.DigitsSix
	totpSkew       = 1
	totpSecretSize = 20 // bytes (160 bits, RFC 4226 §4)

	// secretPurpose is the envelope purpose of TOTP secrets; the user ID is
	// appended, so a sealed secret copied onto another user's row does not
	// open (AAD binding).
	secretPurpose = "identity.mfa_secret/" //nolint:gosec // an envelope purpose (AAD label), not a credential

	// Recovery codes (03 §6.4-4): ten single-use codes of 80 random bits
	// (16 base32 characters, shown as xxxx-xxxx-xxxx-xxxx). Only
	// SHA-256(domain ‖ user ID ‖ code) is stored: at 80 bits a fast hash is
	// as strong as a slow hash of a password, and the user ID salts it.
	recoveryCodeCount = 10
	recoveryCodeBytes = 10
	recoveryCodeLen   = 16
	recoveryDomain    = "koro/identity/recovery-code/v1"
)

var (
	totpOpts      = totp.ValidateOpts{Period: totpPeriod, Skew: 0, Digits: totpDigits, Algorithm: otp.AlgorithmSHA1}
	recoveryCoder = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)
)

// TOTPEnrollment is the answer of enrollTotp: the only place the secret is
// ever shown (04 §2.1). String hides it.
type TOTPEnrollment struct {
	Secret string // base32
	URI    string // otpauth://totp/Koro:<e-mail>?secret=…&issuer=Koro…
}

// String implements fmt.Stringer without revealing the secret.
func (TOTPEnrollment) String() string { return "identity.TOTPEnrollment([REDACTED])" }

// GoString keeps %#v from printing the secret.
func (TOTPEnrollment) GoString() string { return "identity.TOTPEnrollment([REDACTED])" }

// RecoveryCodes are freshly generated recovery codes, shown once.
type RecoveryCodes []string

// String implements fmt.Stringer without revealing the codes.
func (RecoveryCodes) String() string { return "identity.RecoveryCodes([REDACTED])" }

// GoString keeps %#v from printing the codes.
func (RecoveryCodes) GoString() string { return "identity.RecoveryCodes([REDACTED])" }

// mfaReady fails closed where the secret envelope is not wired (worker).
func (s *service) mfaReady() error {
	if s.d.Secrets == nil {
		return ErrMFAUnavailable
	}
	return nil
}

func totpStep(t time.Time) int64 { return t.Unix() / totpPeriod }

// normalizeTOTP keeps the digits of a typed code ("123 456" → "123456");
// ok=false when it cannot be a code.
func normalizeTOTP(raw string) (string, bool) {
	c := strings.Join(strings.Fields(raw), "")
	if len(c) != int(totpDigits) {
		return "", false
	}
	for _, r := range c {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	return c, true
}

// matchStep returns the step within ±totpSkew of now whose code equals
// code. Every candidate is computed and compared in constant time.
func matchStep(secret, code string, now time.Time) (int64, bool, error) {
	cur := totpStep(now)
	var match int64
	found := 0
	for d := int64(-totpSkew); d <= totpSkew; d++ {
		step := cur + d
		want, err := totp.GenerateCodeCustom(secret, time.Unix(step*totpPeriod, 0).UTC(), totpOpts)
		if err != nil {
			return 0, false, fmt.Errorf("identity: totp code: %w", err)
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			match, found = step, found+1
		}
	}
	return match, found > 0, nil
}

// TOTPSecretPurposePrefix is the envelope purpose of TOTP secrets without
// the user ID: crypto.rewrap rebuilds a row's purpose as this prefix plus
// mfa_totp.user_id (cmd/koro declares the column, BE-055).
const TOTPSecretPurposePrefix = secretPurpose

// TOTPSecretPurpose is the envelope purpose of the TOTP secret of userID
// (seeders that write mfa_totp rows directly seal with it).
func TOTPSecretPurpose(userID uuid.UUID) string { return secretPurpose + userID.String() }

// openSecret decrypts the TOTP secret of a row.
func (s *service) openSecret(ctx context.Context, t TOTP) (string, error) {
	pt, err := s.d.Secrets.Open(ctx, TOTPSecretPurpose(t.UserID), t.SecretEnc)
	if err != nil {
		return "", fmt.Errorf("identity: open totp secret: %w", err)
	}
	return string(pt), nil
}

// verifyTOTP checks code against the confirmed TOTP of userID and, on a
// match, records its step (replay guard). ok=false: wrong, expired or
// replayed code, or no confirmed TOTP.
func (s *service) verifyTOTP(ctx context.Context, userID uuid.UUID, raw string) (bool, error) {
	if err := s.mfaReady(); err != nil {
		return false, err
	}
	code, ok := normalizeTOTP(raw)
	if !ok {
		return false, nil
	}
	t, found, err := s.d.Store.TOTP(ctx, userID)
	if err != nil || !found || t.ConfirmedAt == nil {
		return false, err
	}
	secret, err := s.openSecret(ctx, t)
	if err != nil {
		return false, err
	}
	step, ok, err := matchStep(secret, code, s.now())
	if err != nil || !ok {
		return false, err
	}
	used, err := s.d.Store.UseTOTPStep(ctx, userID, step)
	if err != nil {
		return false, err
	}
	if !used {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "replayed totp code refused", slog.String("user_id", userID.String()))
	}
	return used, nil
}

// reasonInvalidMFACode is the audit reason of a refused second-factor code
// (wrong, expired or replayed TOTP code, unknown or used recovery code) of
// confirmTotp, verifyMfa and completeMfaLogin (BE-076).
const reasonInvalidMFACode = "invalid_mfa_code"

// codeDenied is the denied record of a refused second-factor code of act on
// a resource, by the request's actor (a.Kind anonymous: no actor). The
// reason is invalid_mfa_code; meta names the factor where several are
// accepted, never the code: nothing happened. recordFailure adds the
// lockout count.
func codeDenied(act authz.Action, resType string, resID uuid.UUID, a authz.Actor, meta map[string]any) audit.Event {
	e := event(act, audit.Denied, resType, resID, meta).By(a)
	e.Reason = reasonInvalidMFACode
	return e
}

// mfaCodeFailed records a wrong code of a signed-in user: the lockout
// counter with denied, the operation's record by the user (codeDenied: it
// is among the owner's security events, listMySecurityEvents), and the
// reauth class on failures (5 / 15 min), so a stolen session cannot guess
// codes.
func (s *service) mfaCodeFailed(ctx context.Context, u User, denied audit.Event) error {
	if err := s.recordFailure(ctx, u.ID, denied); err != nil {
		return err
	}
	if err := s.allow(ctx, ruleReauth, "user:"+u.ID.String()+":failed"); err != nil {
		return err
	}
	return ErrInvalidMFACode
}

// newRecoveryCodes returns ten codes and their stored hashes.
func newRecoveryCodes(userID uuid.UUID) (RecoveryCodes, [][]byte, error) {
	codes := make(RecoveryCodes, recoveryCodeCount)
	hashes := make([][]byte, recoveryCodeCount)
	for i := range codes {
		b := make([]byte, recoveryCodeBytes)
		if _, err := rand.Read(b); err != nil {
			return nil, nil, fmt.Errorf("identity: recovery code: %w", err)
		}
		c := recoveryCoder.EncodeToString(b)
		codes[i] = c[0:4] + "-" + c[4:8] + "-" + c[8:12] + "-" + c[12:16]
		hashes[i] = recoveryHash(userID, c)
	}
	return codes, hashes, nil
}

// normalizeRecovery accepts a code with or without dashes and spaces, in
// any case; ok=false when it cannot be a code.
func normalizeRecovery(raw string) (string, bool) {
	c := strings.ToLower(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(raw)))
	if len(c) != recoveryCodeLen {
		return "", false
	}
	if _, err := recoveryCoder.DecodeString(c); err != nil {
		return "", false
	}
	return c, true
}

func recoveryHash(userID uuid.UUID, code string) []byte {
	h := sha256.New()
	h.Write([]byte(recoveryDomain))
	h.Write(userID[:])
	h.Write([]byte(code))
	return h.Sum(nil)
}

// useRecoveryCode consumes one recovery code of userID (single use).
func (s *service) useRecoveryCode(ctx context.Context, userID uuid.UUID, raw string) (bool, error) {
	code, ok := normalizeRecovery(raw)
	if !ok {
		return false, nil
	}
	return s.d.Store.UseRecoveryCode(ctx, userID, recoveryHash(userID, code), s.now())
}

// requiresMFA reports whether the user holds an MFA role (03 §6.4, k1):
// from the actor (resolved from the database on this request) and, as a
// second look, from the stored roles and the membership source.
func (s *service) requiresMFA(ctx context.Context, a authz.Actor) (bool, error) {
	if privileged(a.PlatformRoles, a.Memberships) {
		return true, nil
	}
	roles, err := s.d.Store.PlatformRoles(ctx, a.UserID)
	if err != nil {
		return false, err
	}
	ms, err := s.memberships(ctx, a.UserID)
	if err != nil {
		return false, err
	}
	return privileged(roles, ms), nil
}

// EnrollTOTP implements Service (enrollTotp, R by 06 H-46): a new secret
// replaces an unconfirmed earlier one; the secret leaves the server only
// in this answer and is stored sealed. The account has no TOTP yet, so the
// proof is the password: a stolen session cannot put the owner's second
// factor on the thief's device.
func (s *service) EnrollTOTP(ctx context.Context, a authz.Actor, proof ReauthProof) (TOTPEnrollment, error) {
	if err := authz.Can(ctx, a, authz.IdentityMfaEnroll, ownUser(a)); err != nil {
		return TOTPEnrollment{}, err
	}
	if err := s.mfaReady(); err != nil {
		return TOTPEnrollment{}, err
	}
	u, err := s.d.Store.User(ctx, a.UserID)
	if err != nil {
		return TOTPEnrollment{}, err
	}
	if u.MFAEnabled {
		return TOTPEnrollment{}, ErrMFAAlreadyEnabled
	}
	kind, err := s.reauth(ctx, a, proof, reauthDenied(authz.IdentityMfaEnroll, resourceUser, &a.UserID, nil))
	if err != nil {
		return TOTPEnrollment{}, err
	}
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer: totpIssuer, AccountName: u.Email, Period: totpPeriod, SecretSize: totpSecretSize,
		Digits: totpDigits, Algorithm: otp.AlgorithmSHA1, Rand: rand.Reader,
	})
	if err != nil {
		return TOTPEnrollment{}, fmt.Errorf("identity: totp key: %w", err)
	}
	sealed, err := s.d.Secrets.Seal(ctx, TOTPSecretPurpose(u.ID), []byte(key.Secret()))
	if err != nil {
		return TOTPEnrollment{}, fmt.Errorf("identity: seal totp secret: %w", err)
	}
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		if err := s.d.Store.PutTOTP(ctx, TOTP{UserID: u.ID, SecretEnc: sealed}); err != nil {
			return err
		}
		return s.d.Audit.Log(ctx, event(authz.IdentityMfaEnroll, audit.Success, resourceUser, u.ID,
			map[string]any{audit.MetaReauth: kind}).By(a))
	})
	if err != nil {
		return TOTPEnrollment{}, err
	}
	return TOTPEnrollment{Secret: key.Secret(), URI: key.URL()}, nil
}

// ConfirmTOTP implements Service (confirmTotp, R by 06 H-46): the password
// proves the owner, the first code proves the authenticator; MFA turns on,
// ten recovery codes are issued and the current session becomes MFA
// verified (04 §2.1).
func (s *service) ConfirmTOTP(ctx context.Context, a authz.Actor, raw string, proof ReauthProof) (RecoveryCodes, error) {
	if err := authz.Can(ctx, a, authz.IdentityMfaConfirm, ownUser(a)); err != nil {
		return nil, err
	}
	if err := s.mfaReady(); err != nil {
		return nil, err
	}
	u, err := s.d.Store.User(ctx, a.UserID)
	if err != nil {
		return nil, err
	}
	if u.MFAEnabled {
		return nil, ErrMFAAlreadyEnabled
	}
	t, found, err := s.d.Store.TOTP(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	if !found || t.ConfirmedAt != nil {
		return nil, ErrMFANotEnrolled
	}
	kind, err := s.reauth(ctx, a, proof, reauthDenied(authz.IdentityMfaConfirm, resourceUser, &a.UserID, nil))
	if err != nil {
		return nil, err
	}
	if u.locked(s.now()) {
		return nil, problem.ErrRateLimited
	}
	secret, err := s.openSecret(ctx, t)
	if err != nil {
		return nil, err
	}
	step, ok, err := func() (int64, bool, error) {
		code, ok := normalizeTOTP(raw)
		if !ok {
			return 0, false, nil
		}
		return matchStep(secret, code, s.now())
	}()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, s.mfaCodeFailed(ctx, u, codeDenied(authz.IdentityMfaConfirm, resourceUser, u.ID, a, nil))
	}
	codes, hashes, err := newRecoveryCodes(u.ID)
	if err != nil {
		return nil, err
	}
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		now := s.now()
		confirmed, err := s.d.Store.ConfirmTOTP(ctx, u.ID, step, now)
		if err != nil {
			return err
		}
		if !confirmed {
			return ErrMFANotEnrolled // a concurrent confirmation won
		}
		cur, err := s.d.Store.LockUser(ctx, u.ID)
		if err != nil {
			return err
		}
		cur.MFAEnabled, cur.FailedLogins, cur.LockedUntil = true, 0, nil
		if err := s.d.Store.UpdateUser(ctx, cur); err != nil {
			return err
		}
		if err := s.d.Store.ReplaceRecoveryCodes(ctx, u.ID, hashes, now); err != nil {
			return err
		}
		if _, err := s.d.Store.MarkSessionMFA(ctx, u.ID, a.SessionID, now); err != nil {
			return err
		}
		s.notifyUser(ctx, u.ID, NoticeMFAEnabled)
		return s.d.Audit.Log(ctx, event(authz.IdentityMfaConfirm, audit.Success, resourceUser, u.ID,
			map[string]any{audit.MetaReauth: kind, "recovery_code_count": recoveryCodeCount}).By(a))
	})
	if err != nil {
		return nil, err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "mfa enabled", slog.String("user_id", u.ID.String()))
	return codes, nil
}

// VerifyMFA implements Service (verifyMfa): step-up of the current session
// after a role arrived (03 §6.4-2). Recovery codes are not accepted here.
func (s *service) VerifyMFA(ctx context.Context, a authz.Actor, raw string) error {
	if err := authz.Can(ctx, a, authz.IdentityMfaVerify, ownUser(a)); err != nil {
		return err
	}
	if err := s.mfaReady(); err != nil {
		return err
	}
	u, err := s.d.Store.User(ctx, a.UserID)
	if err != nil {
		return err
	}
	if !u.MFAEnabled {
		return ErrMFANotEnabled
	}
	if u.locked(s.now()) {
		return problem.ErrRateLimited
	}
	ok, err := s.verifyTOTP(ctx, u.ID, raw)
	if err != nil {
		return err
	}
	if !ok {
		return s.mfaCodeFailed(ctx, u, codeDenied(authz.IdentityMfaVerify, resourceSession, a.SessionID, a, nil))
	}
	if err := s.resetFailures(ctx, u); err != nil {
		return err
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		marked, err := s.d.Store.MarkSessionMFA(ctx, u.ID, a.SessionID, s.now())
		if err != nil {
			return err
		}
		if !marked {
			return ErrUnauthenticated // the session ended meanwhile
		}
		return s.d.Audit.Log(ctx, event(authz.IdentityMfaVerify, audit.Success, resourceSession, a.SessionID, nil).By(a))
	})
}

// DisableTOTP implements Service (disableTotp, R): refused for MFA roles
// (k1); the TOTP secret and the recovery codes are deleted.
func (s *service) DisableTOTP(ctx context.Context, a authz.Actor, proof ReauthProof) error {
	if err := authz.Can(ctx, a, authz.IdentityMfaDisable, ownUser(a)); err != nil {
		return err
	}
	u, err := s.d.Store.User(ctx, a.UserID)
	if err != nil {
		return err
	}
	if !u.MFAEnabled {
		return ErrMFANotEnabled
	}
	// k1 before the proof: it depends only on the caller's roles, and a
	// TOTP code is not spent on a request that cannot succeed.
	required, err := s.requiresMFA(ctx, a)
	if err != nil {
		return err
	}
	if required {
		return ErrMFARequiredByRole
	}
	kind, err := s.reauth(ctx, a, proof, reauthDenied(authz.IdentityMfaDisable, resourceUser, &a.UserID, nil))
	if err != nil {
		return err
	}
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		if err := s.dropMFA(ctx, u.ID); err != nil {
			return err
		}
		s.notifyUser(ctx, u.ID, NoticeMFADisabled)
		return s.d.Audit.Log(ctx, event(authz.IdentityMfaDisable, audit.Success, resourceUser, u.ID,
			map[string]any{audit.MetaReauth: kind}).By(a))
	})
	if err != nil {
		return err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "mfa disabled", slog.String("user_id", u.ID.String()))
	return nil
}

// dropMFA deletes the TOTP secret and recovery codes of userID and turns
// mfa_enabled off, in the transaction carried by ctx.
func (s *service) dropMFA(ctx context.Context, userID uuid.UUID) error {
	if _, err := s.d.Store.DeleteTOTP(ctx, userID); err != nil {
		return err
	}
	if _, err := s.d.Store.DeleteRecoveryCodes(ctx, userID); err != nil {
		return err
	}
	cur, err := s.d.Store.LockUser(ctx, userID)
	if err != nil {
		return err
	}
	cur.MFAEnabled = false
	return s.d.Store.UpdateUser(ctx, cur)
}

// RegenerateRecoveryCodes implements Service (regenerateRecoveryCodes, R):
// ten new codes, the old ones stop working.
func (s *service) RegenerateRecoveryCodes(ctx context.Context, a authz.Actor, proof ReauthProof) (RecoveryCodes, error) {
	if err := authz.Can(ctx, a, authz.IdentityMfaRecoveryRegenerate, ownUser(a)); err != nil {
		return nil, err
	}
	u, err := s.d.Store.User(ctx, a.UserID)
	if err != nil {
		return nil, err
	}
	if !u.MFAEnabled {
		return nil, ErrMFANotEnabled
	}
	kind, err := s.reauth(ctx, a, proof, reauthDenied(authz.IdentityMfaRecoveryRegenerate, resourceUser, &a.UserID, nil))
	if err != nil {
		return nil, err
	}
	codes, hashes, err := newRecoveryCodes(u.ID)
	if err != nil {
		return nil, err
	}
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		if err := s.d.Store.ReplaceRecoveryCodes(ctx, u.ID, hashes, s.now()); err != nil {
			return err
		}
		s.notifyUser(ctx, u.ID, NoticeRecoveryCodesRegenerated)
		return s.d.Audit.Log(ctx, event(authz.IdentityMfaRecoveryRegenerate, audit.Success, resourceUser, u.ID,
			map[string]any{audit.MetaReauth: kind, "recovery_code_count": recoveryCodeCount}).By(a))
	})
	if err != nil {
		return nil, err
	}
	return codes, nil
}
