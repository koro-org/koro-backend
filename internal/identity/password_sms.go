package identity

import (
	"context"
	"log/slog"
	"time"

	"koro/internal/audit"
	"koro/internal/authz"
)

// Password reset by SMS (BE-172, BE-186; optional, e-mail stays the default).
// The request answers 202 whatever happens (no enumeration): an account that
// may reset by SMS gets a code to its verified number (10 minutes), any other
// account the e-mail link as before. An account may reset by SMS when:
//
//   - it is active and has a verified number;
//   - its holder turned the reset by SMS on, at least SMSResetWait ago
//     (sms_reset.go; off by default);
//   - its SMS second step is off (the phone is never both the reset channel
//     and the second factor);
//   - it holds no role that refuses it (smsResetBarred);
//   - it is below the daily ceiling of the SMS reset (BE-181: codes and tries
//     per account in a rolling day, sms_limits.go).
//
// The confirmation takes the address, the code and the new password; a
// wrong code, an unknown address, an account that lost its eligibility or
// one above the ceiling of tries give the same 422
// identity.invalid_sms_code. Every session closes, as with the e-mail link.

// smsResetNumber returns the number a password reset of u goes to by SMS,
// "" when u must reset by e-mail. A choice found next to a role that refuses
// it closes here (its membership event may have been lost).
func (s *service) smsResetNumber(ctx context.Context, u User) (string, error) {
	if s.d.SMS == nil || u.Status != StatusActive {
		return "", nil
	}
	p, found, err := s.phones().Phone(ctx, u.ID)
	if err != nil || !found || p.SMSResetEnabledAt == nil {
		return "", err
	}
	roles, ms, err := s.heldRoles(ctx, u.ID)
	if err != nil {
		return "", err
	}
	if smsResetBarred(roles, ms) {
		return "", s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
			return s.closeSMSReset(ctx, p, authz.SystemActor(CloseSMSResetTaskName), smsResetClosedRole)
		})
	}
	if !p.smsResetActive(s.now()) {
		return "", nil
	}
	return p.Number, nil
}

// RequestPasswordResetSMS implements Service (requestPasswordReset with
// channel sms): 202 for any well-formed address; a code by SMS when the
// account may reset by SMS and the SMS limits allow, the e-mail link
// otherwise (the same answer).
func (s *service) RequestPasswordResetSMS(ctx context.Context, a authz.Actor, email string) error {
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
	u, found, err := s.users().UserByEmail(ctx, normalized)
	if err != nil || !found || (u.Status != StatusActive && u.Status != StatusPendingEmail) {
		return err
	}
	number, err := s.smsResetNumber(ctx, u)
	if err != nil {
		return err
	}
	if number != "" {
		ok, err := s.mayOpenSMSReset(ctx, u.ID, number)
		if err != nil {
			return err
		}
		if !ok {
			number = "" // the e-mail link instead, same answer
		}
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		meta := map[string]any{"channel": "email"}
		if number == "" {
			if _, err := s.issueToken(ctx, u, PurposePasswordReset, ""); err != nil {
				return err
			}
		} else {
			c, err := s.issueSMSCode(ctx, u.ID, SMSPasswordReset, number, time.Time{})
			if err != nil {
				return err
			}
			meta = map[string]any{"channel": "sms", "phone_hint": MaskPhone(number), "sms_code_id": c.ID.String()}
		}
		return s.d.Audit.Log(ctx, event(authz.IdentityPasswordResetRequest, audit.Success, resourceUser, u.ID, meta))
	})
}

// PasswordResetSMS is the body of confirmPasswordResetSms.
type PasswordResetSMS struct {
	Email       string
	Code        string
	NewPassword string
}

// String keeps the code and the password out of %v.
func (PasswordResetSMS) String() string { return "identity.PasswordResetSMS([REDACTED])" }

// ConfirmPasswordResetSMS implements Service (confirmPasswordResetSms): the
// SMS code of the address's account sets the new password; every session
// closes.
func (s *service) ConfirmPasswordResetSMS(ctx context.Context, a authz.Actor, in PasswordResetSMS) error {
	if err := authz.Can(ctx, a, authz.IdentityPasswordReset, publicUser); err != nil {
		return err
	}
	if s.d.Hasher == nil {
		return ErrUnavailable
	}
	bad := invalid(ErrInvalidSMSCode, "body.code", "is wrong, used or expired")
	_, normalized, err := parseEmail(in.Email, "body.email")
	if err != nil {
		return err
	}
	u, found, err := s.users().UserByEmail(ctx, normalized)
	if err != nil {
		return err
	}
	if !found || u.Status != StatusActive {
		return bad
	}
	use, err := s.smsResetUse(ctx, u.ID)
	if err != nil {
		return err
	}
	if use.Attempts >= maxSMSResetAttempts {
		s.smsCounts.inc(SMSPasswordReset, SMSOutcomeResetClosed)
		s.warnSMSResetLimit(ctx, u.ID)
		if err := s.smsDenied(ctx, authz.IdentityPasswordReset, u.ID, a, SMSPasswordReset, reasonSMSResetLimit); err != nil {
			return err
		}
		return bad
	}
	c, ok, err := s.matchSMSCode(ctx, u.ID, SMSPasswordReset, in.Code)
	if err != nil {
		return err
	}
	if ok {
		// Still eligible, to the same number (the SMS second step may have
		// gone on, or the number changed, since the code was sent).
		number, err := s.smsResetNumber(ctx, u)
		if err != nil {
			return err
		}
		ok = number != "" && number == c.Number
	}
	if !ok {
		if use.Attempts+1 >= maxSMSResetAttempts {
			s.warnSMSResetLimit(ctx, u.ID)
		}
		if err := s.smsCodeDenied(ctx, authz.IdentityPasswordReset, u.ID, a, SMSPasswordReset); err != nil {
			return err
		}
		return bad
	}
	if err := checkPassword(in.NewPassword, u.MFAEnabled, passwordContext{Email: u.Email, FirstName: u.FirstName, LastName: u.LastName}, "body.new_password"); err != nil {
		return err
	}
	hash, err := s.d.Hasher.Hash(ctx, in.NewPassword)
	if err != nil {
		return err // before the transaction: the code stays unused
	}
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		now := s.now()
		consumed, err := s.phones().ConsumeSMSCode(ctx, c.ID, now)
		if err != nil {
			return err
		}
		if !consumed {
			return bad
		}
		cur, err := s.users().LockUser(ctx, u.ID)
		if err != nil {
			return err
		}
		cur.PasswordHash, cur.FailedLogins, cur.LockedUntil = hash, 0, nil
		if err := s.users().UpdateUser(ctx, cur); err != nil {
			return err
		}
		n, err := s.sessions().RevokeUserSessions(ctx, cur.ID, nil, now, RevokePasswordChange)
		if err != nil {
			return err
		}
		s.notifyUser(ctx, cur.ID, NoticePasswordChanged)
		return s.d.Audit.Log(ctx, byUser(event(authz.IdentityPasswordReset, audit.Success, resourceUser, cur.ID,
			map[string]any{"channel": "sms", "sms_code_id": c.ID.String(), "sessions_revoked": n}), cur.ID))
	})
	if err != nil {
		return err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "password reset by SMS", slog.String("user_id", u.ID.String()))
	return nil
}
