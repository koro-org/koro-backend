package identity

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"uuid"

	"golang.org/x/text/unicode/norm"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/problem"
	"koro/internal/platform/token"
)

// PasswordChange is the body of changeMyPassword: current_password always,
// the TOTP code too on an MFA account (03 §6.2).
type PasswordChange struct {
	CurrentPassword string
	NewPassword     string
	TOTPCode        string
}

// String keeps the passwords out of %v.
func (PasswordChange) String() string { return "identity.PasswordChange([REDACTED])" }

// Proof kinds written to audit metadata.reauth (03 §6.2).
const (
	proofPassword = "password"
	proofTOTP     = "totp"
)

// Verify implements Reauthenticator: the R operations of the other modules
// check their proof here (bound in cmd/koro). It audits nothing: the
// calling module writes the denied record of its own action
// (audit.ReauthFailed, BE-074), so a refused proof is one row, never two.
// Failures still count towards the lockout here.
func (s *service) Verify(ctx context.Context, userID uuid.UUID, proof ReauthProof) (string, error) {
	return s.verify(ctx, userID, proof, false)
}

// verify checks a re-authentication proof of userID: the TOTP code on an
// MFA account, the password otherwise; withPassword (identity.password.change,
// 03 §6.2) asks for the password always and the TOTP code on top on an MFA
// account. It audits nothing: identity's own R operations hand a failure
// to audit.ReauthFailed (reauth, ChangePassword), the other modules do the
// same with Verify's (BE-075).
func (s *service) verify(ctx context.Context, userID uuid.UUID, proof ReauthProof, withPassword bool) (string, error) {
	u, err := s.d.Store.User(ctx, userID)
	switch {
	case isNotFound(err):
		return "", ErrReauthFailed
	case err != nil:
		return "", err
	}
	if u.Status != StatusActive {
		return "", ErrReauthFailed
	}
	if withPassword {
		if proof.Password == "" {
			return "", ErrReauthRequired
		}
		if err := s.checkPassword(ctx, u, proof.Password); err != nil || !u.MFAEnabled {
			return proofPassword, err
		}
		return proofTOTP, s.checkTOTP(ctx, u, proof.TOTPCode)
	}
	if u.MFAEnabled {
		return proofTOTP, s.checkTOTP(ctx, u, proof.TOTPCode)
	}
	if proof.Password == "" {
		return "", ErrReauthRequired
	}
	return proofPassword, s.checkPassword(ctx, u, proof.Password)
}

// reauth checks the proof of one of identity's own R operations after
// authz.Can, before any business data (03 §6.2), and returns its kind for
// the audit record. A refused proof (403 reauth_required / reauth_failed,
// 429) is the operation's denied reauth_failed record by a: denied is the
// resource of its success record, nothing more (reauthDenied). An error
// without a verdict (an overloaded hasher, a store down) writes nothing
// and goes back unchanged (audit.ReauthFailed; BE-070, BE-072, BE-075).
func (s *service) reauth(ctx context.Context, a authz.Actor, proof ReauthProof, denied audit.Event) (string, error) {
	kind, err := s.verify(ctx, a.UserID, proof, false)
	if err != nil {
		return "", audit.ReauthFailed(ctx, s.d.Audit, s.d.Log, denied.By(a), err)
	}
	return kind, nil
}

// reauthDenied is the denied record of a refused proof of act on a
// resource of resType (resID nil before it exists). meta names the step of
// a shared action, never business data: nothing happened.
func reauthDenied(act authz.Action, resType string, resID *uuid.UUID, meta map[string]any) audit.Event {
	return audit.Event{Action: string(act), ResourceType: resType, ResourceID: resID, Metadata: meta}
}

// checkPassword verifies the password of a signed-in user (re-authentication,
// current_password). A locked account is refused before any comparison; a
// wrong password counts towards the lockout and the reauth class (failures
// only, 04 §1.9) and becomes 429 once either is exhausted. An overloaded
// hasher is 503 and no verdict: nothing is counted or reset (BE-070).
func (s *service) checkPassword(ctx context.Context, u User, pw string) error {
	if s.d.Hasher == nil {
		return ErrUnavailable
	}
	if u.locked(s.now()) {
		if err := s.d.Hasher.VerifyDummy(ctx, pw); err != nil {
			return err
		}
		return problem.ErrRateLimited
	}
	ok := false
	if u.PasswordHash != "" {
		var err error
		ok, _, err = s.d.Hasher.Verify(ctx, pw, u.PasswordHash)
		switch {
		case hasherBusy(err):
			return err
		case err != nil:
			s.d.Log.LogAttrs(ctx, slog.LevelError, "stored password hash is unusable", slog.String("user_id", u.ID.String()),
				slog.String("err", err.Error()))
			ok = false
		}
	} else if err := s.d.Hasher.VerifyDummy(ctx, pw); err != nil {
		return err
	}
	if ok {
		return s.resetFailures(ctx, u)
	}
	return s.reauthFailed(ctx, u)
}

// checkTOTP verifies the TOTP code of an MFA account (03 §6.2: recovery
// codes are not accepted for re-authentication). A code is accepted once
// (replay guard); a wrong one counts like a wrong password.
func (s *service) checkTOTP(ctx context.Context, u User, code string) error {
	if code == "" {
		return ErrReauthRequired
	}
	if err := s.mfaReady(); err != nil {
		return err
	}
	if u.locked(s.now()) {
		return problem.ErrRateLimited
	}
	ok, err := s.verifyTOTP(ctx, u.ID, code)
	if err != nil {
		return err
	}
	if ok {
		return s.resetFailures(ctx, u)
	}
	return s.reauthFailed(ctx, u)
}

// reauthFailed records a wrong proof: the lockout counter and the reauth
// class keyed on failures of this user. The denied record is the caller's
// (audit.ReauthFailed with the refusal this returns).
func (s *service) reauthFailed(ctx context.Context, u User) error {
	if err := s.recordFailure(ctx, u.ID, audit.Event{}); err != nil {
		return err
	}
	if err := s.allow(ctx, ruleReauth, "user:"+u.ID.String()+":failed"); err != nil {
		return err
	}
	return ErrReauthFailed
}

// ChangePassword implements Service (changeMyPassword, R): every other
// session closes (password_change) and the owner gets a security notice.
// The proof is current_password, plus the TOTP code on an MFA account
// (03 §6.2); a refused one is the denied record of the action, like every
// R operation (audit.ReauthFailed, BE-075).
func (s *service) ChangePassword(ctx context.Context, a authz.Actor, in PasswordChange) error {
	if err := authz.Can(ctx, a, authz.IdentityPasswordChange, ownUser(a)); err != nil {
		return err
	}
	if s.d.Hasher == nil {
		return ErrUnavailable
	}
	kind, err := s.verify(ctx, a.UserID, ReauthProof{Password: in.CurrentPassword, TOTPCode: in.TOTPCode}, true)
	if err != nil {
		denied := reauthDenied(authz.IdentityPasswordChange, resourceUser, &a.UserID, nil).By(a)
		return audit.ReauthFailed(ctx, s.d.Audit, s.d.Log, denied, err)
	}
	u, err := s.d.Store.User(ctx, a.UserID)
	if err != nil {
		return err
	}
	if err := checkPassword(in.NewPassword, u.MFAEnabled, passwordContext{Email: u.Email, FirstName: u.FirstName, LastName: u.LastName}, "body.new_password"); err != nil {
		return err
	}
	if subtle.ConstantTimeCompare([]byte(norm.NFKC.String(in.NewPassword)), []byte(norm.NFKC.String(in.CurrentPassword))) == 1 {
		return invalid(ErrPasswordUnchanged, "body.new_password", "must differ from the current password")
	}
	hash, err := s.d.Hasher.Hash(ctx, in.NewPassword)
	if err != nil {
		return err // before the transaction: nothing is written
	}
	keep := a.SessionID
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		cur, err := s.d.Store.LockUser(ctx, u.ID)
		if err != nil {
			return err
		}
		cur.PasswordHash, cur.FailedLogins, cur.LockedUntil = hash, 0, nil
		if err := s.d.Store.UpdateUser(ctx, cur); err != nil {
			return err
		}
		n, err := s.d.Store.RevokeUserSessions(ctx, u.ID, &keep, s.now(), RevokePasswordChange)
		if err != nil {
			return err
		}
		s.notifyUser(ctx, u.ID, NoticePasswordChanged)
		return s.d.Audit.Log(ctx, event(authz.IdentityPasswordChange, audit.Success, resourceUser, u.ID,
			map[string]any{audit.MetaReauth: kind, "sessions_revoked": n}).By(a))
	})
	if err != nil {
		return err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "password changed", slog.String("user_id", u.ID.String()))
	return nil
}

// RequestPasswordReset implements Service (requestPasswordReset): 202 for
// any well-formed address; a token goes only to an active or unverified
// account (02 §5.3: 30 minutes).
func (s *service) RequestPasswordReset(ctx context.Context, a authz.Actor, email string) error {
	if err := authz.Can(ctx, a, authz.IdentityPasswordResetRequest, publicUser); err != nil {
		return err
	}
	_, normalized, err := parseEmail(email, "body.email")
	if err != nil {
		return err
	}
	if err := s.limitEmail(ctx, rulePasswordReset, normalized); err != nil {
		return err
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		u, found, err := s.d.Store.UserByEmail(ctx, normalized)
		if err != nil || !found || (u.Status != StatusActive && u.Status != StatusPendingEmail) {
			return err
		}
		if _, err := s.issueToken(ctx, u, PurposePasswordReset, ""); err != nil {
			return err
		}
		return s.d.Audit.Log(ctx, event(authz.IdentityPasswordResetRequest, audit.Success, resourceUser, u.ID, nil))
	})
}

// ConfirmPasswordReset implements Service (confirmPasswordReset): the
// token is single use; every session closes (password_change); a reset
// also proves the address, so an unverified account becomes active.
func (s *service) ConfirmPasswordReset(ctx context.Context, a authz.Actor, raw, newPassword string) error {
	if err := authz.Can(ctx, a, authz.IdentityPasswordReset, publicUser); err != nil {
		return err
	}
	if s.d.Hasher == nil {
		return ErrUnavailable
	}
	t, u, err := s.findToken(ctx, raw, PurposePasswordReset)
	if err != nil {
		return err
	}
	if err := checkPassword(newPassword, u.MFAEnabled, passwordContext{Email: u.Email, FirstName: u.FirstName, LastName: u.LastName}, "body.new_password"); err != nil {
		return err
	}
	hash, err := s.d.Hasher.Hash(ctx, newPassword)
	if err != nil {
		return err // before the transaction: the token stays unused
	}
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		cur, err := s.consumeToken(ctx, t.ID, PurposePasswordReset)
		if err != nil {
			return err
		}
		now := s.now()
		cur.PasswordHash, cur.FailedLogins, cur.LockedUntil = hash, 0, nil
		if cur.Status == StatusPendingEmail {
			cur.Status, cur.EmailVerifiedAt = StatusActive, &now
		}
		if err := s.d.Store.UpdateUser(ctx, cur); err != nil {
			return err
		}
		n, err := s.d.Store.RevokeUserSessions(ctx, cur.ID, nil, now, RevokePasswordChange)
		if err != nil {
			return err
		}
		s.notifyUser(ctx, cur.ID, NoticePasswordChanged)
		return s.d.Audit.Log(ctx, byUser(event(authz.IdentityPasswordReset, audit.Success, resourceUser, cur.ID,
			map[string]any{"token_id": t.ID.String(), "sessions_revoked": n}), cur.ID))
	})
	if err != nil {
		return err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "password reset", slog.String("user_id", u.ID.String()))
	return nil
}

// ConfirmEmail implements Service (confirmEmail): the account_email token
// activates a pending_email account (a verified one just consumes it).
func (s *service) ConfirmEmail(ctx context.Context, a authz.Actor, raw string) error {
	if err := authz.Can(ctx, a, authz.IdentityEmailVerify, publicUser); err != nil {
		return err
	}
	t, _, err := s.findToken(ctx, raw, PurposeAccountEmail)
	if err != nil {
		return err
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		u, err := s.consumeToken(ctx, t.ID, PurposeAccountEmail)
		if err != nil {
			return err
		}
		if u.Status == StatusPendingEmail {
			now := s.now()
			u.Status, u.EmailVerifiedAt = StatusActive, &now
			if err := s.d.Store.UpdateUser(ctx, u); err != nil {
				return err
			}
		}
		return s.d.Audit.Log(ctx, byUser(event(authz.IdentityEmailVerify, audit.Success, resourceUser, u.ID,
			map[string]any{"token_id": t.ID.String()}), u.ID))
	})
}

// ResendVerification implements Service (resendEmailVerification): 202
// for any well-formed address; only an unverified account gets a new
// token (the previous one closes).
func (s *service) ResendVerification(ctx context.Context, a authz.Actor, email string) error {
	if err := authz.Can(ctx, a, authz.IdentityEmailResendVerification, publicUser); err != nil {
		return err
	}
	_, normalized, err := parseEmail(email, "body.email")
	if err != nil {
		return err
	}
	if err := s.limitEmail(ctx, ruleAuthEmail, normalized); err != nil {
		return err
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		u, found, err := s.d.Store.UserByEmail(ctx, normalized)
		if err != nil || !found || u.Status != StatusPendingEmail {
			return err
		}
		_, err = s.issueToken(ctx, u, PurposeAccountEmail, "")
		return err
	})
}

// RequestEmailChange implements Service (requestEmailChange, R): a token
// to the new address (24 h), a notice to the current one. A taken address
// is answered like a free one (202) and gets nothing.
func (s *service) RequestEmailChange(ctx context.Context, a authz.Actor, newEmail string, proof ReauthProof) error {
	if err := authz.Can(ctx, a, authz.IdentityEmailChangeRequest, ownUser(a)); err != nil {
		return err
	}
	target, normalized, err := parseEmail(newEmail, "body.new_email")
	if err != nil {
		return err
	}
	kind, err := s.reauth(ctx, a, proof, reauthDenied(authz.IdentityEmailChangeRequest, resourceUser, &a.UserID, nil))
	if err != nil {
		return err
	}
	if err := s.limitEmail(ctx, ruleAuthEmail, normalized); err != nil {
		return err
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		u, err := s.d.Store.User(ctx, a.UserID)
		if err != nil {
			return err
		}
		if u.EmailNormalized == normalized {
			return invalid(ErrInvalidEmail, "body.new_email", "is the current address")
		}
		meta := map[string]any{audit.MetaReauth: kind}
		other, taken, err := s.d.Store.UserByEmail(ctx, normalized)
		if err != nil {
			return err
		}
		if taken && other.ID != u.ID {
			meta["result"] = "address_taken"
		} else {
			t, err := s.issueToken(ctx, u, PurposeEmailChange, target)
			if err != nil {
				return err
			}
			meta["token_id"] = t.ID.String()
			s.notifyUser(ctx, u.ID, NoticeEmailChangeRequested)
		}
		return s.d.Audit.Log(ctx, event(authz.IdentityEmailChangeRequest, audit.Success, resourceUser, u.ID, meta).By(a))
	})
}

// ConfirmEmailChange implements Service (confirmEmailChange).
func (s *service) ConfirmEmailChange(ctx context.Context, a authz.Actor, raw string) error {
	if err := authz.Can(ctx, a, authz.IdentityEmailChangeConfirm, publicUser); err != nil {
		return err
	}
	t, _, err := s.findToken(ctx, raw, PurposeEmailChange)
	if err != nil {
		return err
	}
	_, normalized, err := parseEmail(t.TargetEmail, "body.token")
	if err != nil {
		return ErrInvalidToken
	}
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		u, err := s.consumeToken(ctx, t.ID, PurposeEmailChange)
		if err != nil {
			return err
		}
		if other, taken, err := s.d.Store.UserByEmail(ctx, normalized); err != nil {
			return err
		} else if taken && other.ID != u.ID {
			return ErrEmailTaken
		}
		now := s.now()
		u.Email, u.EmailNormalized, u.EmailVerifiedAt = t.TargetEmail, normalized, &now
		if err := s.d.Store.UpdateUser(ctx, u); err != nil {
			return err
		}
		if err := s.d.Store.CloseOpenTokens(ctx, u.ID, PurposeEmailChange, now); err != nil {
			return err
		}
		s.notifyUser(ctx, u.ID, NoticeEmailChanged)
		return s.d.Audit.Log(ctx, byUser(event(authz.IdentityEmailChangeConfirm, audit.Success, resourceUser, u.ID,
			map[string]any{"token_id": t.ID.String()}), u.ID))
	})
	if err != nil {
		return err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "e-mail address changed", slog.String("user_id", t.UserID.String()))
	return nil
}

// findToken resolves a raw token of purpose to its open row and its live
// account. Unknown, used, expired, wrong-purpose tokens and tokens of a
// suspended or erased account are all ErrInvalidToken.
func (s *service) findToken(ctx context.Context, raw string, purpose TokenPurpose) (EmailToken, User, error) {
	hash, ok := tokenHash(raw)
	if !ok {
		return EmailToken{}, User{}, ErrInvalidToken
	}
	t, found, err := s.d.Store.EmailTokenByHash(ctx, hash)
	switch {
	case err != nil:
		return EmailToken{}, User{}, err
	case !found || !token.Equal(t.TokenHash, hash) || t.Purpose != purpose || t.ConsumedAt != nil || !s.now().Before(t.ExpiresAt):
		return EmailToken{}, User{}, ErrInvalidToken
	}
	u, err := s.d.Store.User(ctx, t.UserID)
	switch {
	case isNotFound(err):
		return EmailToken{}, User{}, ErrInvalidToken
	case err != nil:
		return EmailToken{}, User{}, err
	}
	if u.Status != StatusActive && u.Status != StatusPendingEmail {
		return EmailToken{}, User{}, ErrInvalidToken
	}
	return t, u, nil
}

// consumeToken locks the token and its account in the transaction
// carried by ctx, re-checks them and marks the token used (single use: a
// concurrent second confirmation finds it consumed).
func (s *service) consumeToken(ctx context.Context, id uuid.UUID, purpose TokenPurpose) (User, error) {
	t, err := s.d.Store.LockEmailToken(ctx, id)
	switch {
	case isNotFound(err):
		return User{}, ErrInvalidToken
	case err != nil:
		return User{}, err
	}
	now := s.now()
	if t.Purpose != purpose || t.ConsumedAt != nil || !now.Before(t.ExpiresAt) {
		return User{}, ErrInvalidToken
	}
	consumed, err := s.d.Store.ConsumeEmailToken(ctx, id, now)
	if err != nil {
		return User{}, err
	}
	if !consumed {
		return User{}, ErrInvalidToken
	}
	u, err := s.d.Store.LockUser(ctx, t.UserID)
	if err != nil {
		return User{}, err
	}
	if u.Status != StatusActive && u.Status != StatusPendingEmail {
		return User{}, ErrInvalidToken
	}
	return u, nil
}
