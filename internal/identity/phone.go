package identity

import (
	"context"
	"log/slog"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/sms"
)

// Phone numbers and the SMS second step (BE-172; user decision 2026-11-05).
//
//   - A number is added (or changed) with startPhoneVerification (R: a
//     stolen session must not put the thief's phone on the account, it is a
//     sign-in factor and a reset channel) and becomes the account's once
//     its holder types the SMS code (confirmPhoneVerification). Whether
//     another account holds the number is told only then, to the holder.
//   - Turkish mobile numbers only (sms.NormalizeNumber), stored in E.164,
//     shown masked (MaskPhone, the BE-108 mask) everywhere.
//   - The SMS second step is the user's choice next to TOTP. It is refused
//     to a user holding a role that requires MFA (409
//     identity.sms_mfa_not_allowed: SIM swap; their factor is TOTP alone),
//     and an SMS sign-in never makes a session MFA-verified, so it can never
//     open a role path (03 §6.4, k-mfa). The TOTP rules do not change.
//   - Every change sends a security notice by e-mail (identity.phone_notice).

// PhoneView is the caller's number as answers show it.
type PhoneView struct {
	// Hint is the masked number ("***67").
	Hint       string
	VerifiedAt time.Time
	// SMSMFAEnabled: the SMS second step of sign-in is on.
	SMSMFAEnabled bool
}

// phoneView is the answer form of p.
func phoneView(p Phone) PhoneView {
	return PhoneView{Hint: MaskPhone(p.Number), VerifiedAt: p.VerifiedAt, SMSMFAEnabled: p.SMSMFAEnabledAt != nil}
}

// Notice kinds of identity.phone_notice (BE-172).
const (
	NoticePhoneVerified  = "phone_verified"
	NoticePhoneRemoved   = "phone_removed"
	NoticeSMSMFAEnabled  = "sms_mfa_enabled"
	NoticeSMSMFADisabled = "sms_mfa_disabled"
)

// StartPhoneVerification implements Service (startPhoneVerification, R): a
// code goes by SMS to number (10 minutes). The answer is the same whether
// or not another account holds the number.
func (s *service) StartPhoneVerification(ctx context.Context, a authz.Actor, raw string, proof ReauthProof) (SMSCodeSent, error) {
	if err := authz.Can(ctx, a, authz.IdentityPhoneChange, ownUser(a)); err != nil {
		return SMSCodeSent{}, err
	}
	if err := s.smsReady(); err != nil {
		return SMSCodeSent{}, err
	}
	number, err := sms.NormalizeNumber(raw)
	if err != nil {
		return SMSCodeSent{}, invalid(ErrInvalidPhone, "body.phone", "must be a Turkish mobile number (+90 5XX XXX XX XX)")
	}
	kind, err := s.reauth(ctx, a, proof, reauthDenied(authz.IdentityPhoneChange, resourceUser, &a.UserID, nil))
	if err != nil {
		return SMSCodeSent{}, err
	}
	if err := s.limitSMS(ctx, a.UserID, number); err != nil {
		return SMSCodeSent{}, err
	}
	var sent SMSCodeSent
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		c, err := s.issueSMSCode(ctx, a.UserID, SMSPhoneVerify, number, time.Time{})
		if err != nil {
			return err
		}
		sent = SMSCodeSent{ExpiresAt: c.ExpiresAt, PhoneHint: MaskPhone(number)}
		return s.d.Audit.Log(ctx, event(authz.IdentityPhoneChange, audit.Success, resourceUser, a.UserID,
			map[string]any{audit.MetaReauth: kind, "phone_hint": sent.PhoneHint, "sms_code_id": c.ID.String()}).By(a))
	})
	if err != nil {
		return SMSCodeSent{}, err
	}
	return sent, nil
}

// ConfirmPhoneVerification implements Service (confirmPhoneVerification):
// the SMS code makes its number the caller's (an earlier number is
// replaced; the SMS second step stays as it was). 422
// identity.invalid_sms_code for any wrong, used or expired code; 409
// identity.phone_taken when another account holds the number.
func (s *service) ConfirmPhoneVerification(ctx context.Context, a authz.Actor, raw string) (PhoneView, error) {
	if err := authz.Can(ctx, a, authz.IdentityPhoneVerify, ownUser(a)); err != nil {
		return PhoneView{}, err
	}
	c, ok, err := s.matchSMSCode(ctx, a.UserID, SMSPhoneVerify, raw)
	if err != nil {
		return PhoneView{}, err
	}
	if !ok {
		if err := s.smsCodeDenied(ctx, authz.IdentityPhoneVerify, a.UserID, a, SMSPhoneVerify); err != nil {
			return PhoneView{}, err
		}
		return PhoneView{}, invalid(ErrInvalidSMSCode, "body.code", "is wrong, used or expired")
	}
	var view PhoneView
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		if owner, taken, err := s.phones().PhoneOwner(ctx, c.Number); err != nil {
			return err
		} else if taken && owner != a.UserID {
			return ErrPhoneTaken
		}
		now := s.now()
		consumed, err := s.phones().ConsumeSMSCode(ctx, c.ID, now)
		if err != nil {
			return err
		}
		if !consumed {
			return invalid(ErrInvalidSMSCode, "body.code", "is wrong, used or expired")
		}
		cur, had, err := s.phones().Phone(ctx, a.UserID)
		if err != nil {
			return err
		}
		p := Phone{UserID: a.UserID, Number: c.Number, VerifiedAt: now}
		if had {
			p.SMSMFAEnabledAt = cur.SMSMFAEnabledAt
		}
		if err := s.phones().PutPhone(ctx, p); err != nil {
			return err
		}
		view = phoneView(p)
		s.notifyPhone(ctx, a.UserID, NoticePhoneVerified, view.Hint)
		return s.d.Audit.Log(ctx, event(authz.IdentityPhoneVerify, audit.Success, resourceUser, a.UserID, map[string]any{
			"phone_hint": view.Hint, "sms_code_id": c.ID.String(), "replaced": had && cur.Number != c.Number,
		}).By(a))
	})
	if err != nil {
		return PhoneView{}, err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "phone number verified", slog.String("user_id", a.UserID.String()))
	return view, nil
}

// RemovePhone implements Service (removeMyPhone, R): the number goes, and
// with it the SMS second step and the open SMS codes.
func (s *service) RemovePhone(ctx context.Context, a authz.Actor, proof ReauthProof) error {
	if err := authz.Can(ctx, a, authz.IdentityPhoneChange, ownUser(a)); err != nil {
		return err
	}
	p, found, err := s.phones().Phone(ctx, a.UserID)
	if err != nil {
		return err
	}
	if !found {
		return ErrPhoneNotVerified
	}
	kind, err := s.reauth(ctx, a, proof, reauthDenied(authz.IdentityPhoneChange, resourceUser, &a.UserID, nil))
	if err != nil {
		return err
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		if _, err := s.phones().DeletePhone(ctx, a.UserID); err != nil {
			return err
		}
		now := s.now()
		for _, purpose := range []SMSPurpose{SMSPhoneVerify, SMSLogin, SMSPasswordReset} {
			if err := s.phones().CloseOpenSMSCodes(ctx, a.UserID, purpose, now); err != nil {
				return err
			}
		}
		s.notifyPhone(ctx, a.UserID, NoticePhoneRemoved, MaskPhone(p.Number))
		return s.d.Audit.Log(ctx, event(authz.IdentityPhoneChange, audit.Success, resourceUser, a.UserID, map[string]any{
			audit.MetaReauth: kind, "phone_hint": MaskPhone(p.Number), "removed": true, "sms_mfa_was_on": p.SMSMFAEnabledAt != nil,
		}).By(a))
	})
}

// EnableSMSMFA implements Service (enableSmsMfa, R): the SMS code becomes a
// second step of the caller's sign-in. 409 identity.phone_not_verified
// without a number, identity.sms_mfa_not_allowed for a holder of an MFA
// role (k-sms); already on: nothing changes.
func (s *service) EnableSMSMFA(ctx context.Context, a authz.Actor, proof ReauthProof) error {
	if err := authz.Can(ctx, a, authz.IdentitySmsMfaEnable, ownUser(a)); err != nil {
		return err
	}
	if err := s.smsReady(); err != nil {
		return err
	}
	p, found, err := s.phones().Phone(ctx, a.UserID)
	if err != nil {
		return err
	}
	if !found {
		return ErrPhoneNotVerified
	}
	// k-sms before the proof: it depends only on the caller's roles (like k1).
	required, err := s.requiresMFA(ctx, a)
	if err != nil {
		return err
	}
	if required {
		return ErrSMSMFANotAllowed
	}
	if p.SMSMFAEnabledAt != nil {
		return nil
	}
	kind, err := s.reauth(ctx, a, proof, reauthDenied(authz.IdentitySmsMfaEnable, resourceUser, &a.UserID, nil))
	if err != nil {
		return err
	}
	return s.setSMSMFA(ctx, a, true, kind, MaskPhone(p.Number))
}

// DisableSMSMFA implements Service (disableSmsMfa, R): the SMS second step
// goes off (the number stays); already off: nothing changes.
func (s *service) DisableSMSMFA(ctx context.Context, a authz.Actor, proof ReauthProof) error {
	if err := authz.Can(ctx, a, authz.IdentitySmsMfaDisable, ownUser(a)); err != nil {
		return err
	}
	p, found, err := s.phones().Phone(ctx, a.UserID)
	if err != nil {
		return err
	}
	if !found || p.SMSMFAEnabledAt == nil {
		return nil
	}
	kind, err := s.reauth(ctx, a, proof, reauthDenied(authz.IdentitySmsMfaDisable, resourceUser, &a.UserID, nil))
	if err != nil {
		return err
	}
	return s.setSMSMFA(ctx, a, false, kind, MaskPhone(p.Number))
}

// setSMSMFA turns the SMS second step of the caller on or off, notifies and
// audits it.
func (s *service) setSMSMFA(ctx context.Context, a authz.Actor, on bool, kind, hint string) error {
	act, notice := authz.IdentitySmsMfaDisable, NoticeSMSMFADisabled
	var at *time.Time
	if on {
		now := s.now()
		act, notice, at = authz.IdentitySmsMfaEnable, NoticeSMSMFAEnabled, &now
	}
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		changed, err := s.phones().SetSMSMFA(ctx, a.UserID, at)
		if err != nil {
			return err
		}
		if !changed {
			return ErrPhoneNotVerified // removed meanwhile
		}
		s.notifyPhone(ctx, a.UserID, notice, hint)
		return s.d.Audit.Log(ctx, event(act, audit.Success, resourceUser, a.UserID,
			map[string]any{audit.MetaReauth: kind, "phone_hint": hint}).By(a))
	})
	if err != nil {
		return err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "SMS second step changed", slog.String("user_id", a.UserID.String()), slog.Bool("on", on))
	return nil
}

// notifyPhone enqueues a phone notice after the transaction commits
// (mechanism A, like notifyUser): the masked number travels with it (the
// number may be gone when the worker runs).
func (s *service) notifyPhone(ctx context.Context, userID uuid.UUID, kind, hint string) {
	n := securityNotice{UserID: userID, Kind: kind, Ref: uuid.NewV7(), PhoneHint: hint}
	s.d.Queue.AfterCommit(ctx, SendSecurityNoticeTaskName, n, SendSecurityNoticeTaskName+":"+n.Ref.String())
}

// myPhone returns the caller's number (getMe); found=false when none.
func (s *service) myPhone(ctx context.Context, userID uuid.UUID) (*PhoneView, error) {
	p, found, err := s.phones().Phone(ctx, userID)
	if err != nil || !found {
		return nil, err
	}
	v := phoneView(p)
	return &v, nil
}
