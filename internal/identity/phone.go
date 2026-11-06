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
//   - A number is added (or changed) with startPhoneVerification (R: it is a
//     sign-in factor and a reset channel) and becomes the account's once
//     its holder types the SMS code (confirmPhoneVerification). Whether
//     another account holds the number is told only then, to the holder.
//   - While the SMS second step is on and the proof is the password, a change
//     of the phone factor (a new number, removing it, turning the step off)
//     also takes a code of the current number (sendPhoneChangeCode; BE-186).
//     A TOTP proof is enough on its own. Without the phone, an admin's MFA
//     reset (k2) turns the step off.
//   - A new number ends what belonged to the old one: its open codes and the
//     password reset by SMS (BE-186).
//   - Turkish mobile numbers only (sms.NormalizeNumber), stored in E.164,
//     shown masked (MaskPhone, the BE-108 mask) everywhere.
//   - The SMS second step is the user's choice next to TOTP. It is refused
//     to a user holding a role that requires MFA (409
//     identity.sms_mfa_not_allowed: their factor is TOTP alone), and an SMS
//     sign-in never makes a session MFA-verified, so it can never open a role
//     path (03 §6.4, k-mfa). The TOTP rules do not change.
//   - Every change sends a security notice by e-mail (identity.phone_notice).

// PhoneView is the caller's number as answers show it.
type PhoneView struct {
	// Hint is the masked number ("***67").
	Hint       string
	VerifiedAt time.Time
	// SMSMFAEnabled: the SMS second step of sign-in is on.
	SMSMFAEnabled bool
	// SMSResetAllowed: the caller may turn the password reset by SMS on: no
	// role refuses it and the SMS second step is off (BE-186).
	SMSResetAllowed bool
	// SMSResetEnabled: the caller's choice is on.
	SMSResetEnabled bool
	// SMSResetActiveFrom: when the choice starts working; nil while it is off.
	SMSResetActiveFrom *time.Time
}

// phoneView is the answer form of p for its owner a.
func phoneView(p Phone, a authz.Actor) PhoneView {
	v := PhoneView{Hint: MaskPhone(p.Number), VerifiedAt: p.VerifiedAt, SMSMFAEnabled: p.SMSMFAEnabledAt != nil,
		SMSResetAllowed: p.SMSMFAEnabledAt == nil && !smsResetBarred(a.PlatformRoles, a.Memberships)}
	if v.SMSResetAllowed && p.SMSResetEnabledAt != nil {
		from := p.SMSResetEnabledAt.Add(SMSResetWait)
		v.SMSResetEnabled, v.SMSResetActiveFrom = true, &from
	}
	return v
}

// Notice kinds of identity.phone_notice (BE-172).
const (
	NoticePhoneVerified  = "phone_verified"
	NoticePhoneRemoved   = "phone_removed"
	NoticeSMSMFAEnabled  = "sms_mfa_enabled"
	NoticeSMSMFADisabled = "sms_mfa_disabled"
	// NoticeSMSResetLimit: the SMS password reset of the account reached its
	// daily ceiling and goes by e-mail for now (BE-181).
	NoticeSMSResetLimit = "sms_reset_limit"
)

// stepPhoneChangeCode names sendPhoneChangeCode in the records of the shared
// action identity.phone.change.
const stepPhoneChangeCode = "phone_change_code"

// asksChangeCode reports whether a change of p proved with a proof of kind
// also takes a code of the current number.
func (p Phone) asksChangeCode(kind string) bool {
	return p.SMSMFAEnabledAt != nil && kind != proofTOTP
}

// checkChangeCode checks the code of the current number that a change of p
// by act asks for, after the proof of kind. It returns the code's ID to
// consume in the change's transaction, the nil UUID when none is asked for.
// 403 identity.phone_code_required without one; a wrong one is 422
// identity.invalid_sms_code and counts like a wrong TOTP code (the account
// lockout, the reauth class, a denied record by a).
func (s *service) checkChangeCode(ctx context.Context, a authz.Actor, act authz.Action, p Phone, kind, raw string) (uuid.UUID, error) {
	if !p.asksChangeCode(kind) {
		return uuid.Nil(), nil
	}
	if raw == "" {
		return uuid.Nil(), ErrPhoneCodeRequired
	}
	c, ok, err := s.matchSMSCode(ctx, a.UserID, SMSPhoneChange, raw)
	if err != nil {
		return uuid.Nil(), err
	}
	if ok && c.Number == p.Number {
		return c.ID, nil
	}
	denied := event(act, audit.Denied, resourceUser, a.UserID, map[string]any{"sms_purpose": string(SMSPhoneChange)}).By(a)
	denied.Reason = reasonInvalidSMSCode
	if err := s.recordFailure(ctx, a.UserID, denied); err != nil {
		return uuid.Nil(), err
	}
	if err := s.allow(ctx, ruleReauth, "user:"+a.UserID.String()+":failed"); err != nil {
		return uuid.Nil(), err
	}
	return uuid.Nil(), invalid(ErrInvalidSMSCode, "body.phone_code", "is wrong, used or expired")
}

// consumeChangeCode uses the code checkChangeCode returned in the
// transaction carried by ctx (single use); the nil UUID: none was asked for.
func (s *service) consumeChangeCode(ctx context.Context, id uuid.UUID) error {
	if id == uuid.Nil() {
		return nil
	}
	consumed, err := s.phones().ConsumeSMSCode(ctx, id, s.now())
	if err != nil {
		return err
	}
	if !consumed {
		return invalid(ErrInvalidSMSCode, "body.phone_code", "is wrong, used or expired")
	}
	return nil
}

// withChangeCode names the code of the current number in an audit record's
// metadata, when one was used.
func withChangeCode(meta map[string]any, id uuid.UUID) map[string]any {
	if id != uuid.Nil() {
		meta["phone_change_code_id"] = id.String()
	}
	return meta
}

// closeAllSMSCodes closes the open codes of every purpose of userID in the
// transaction carried by ctx.
func (s *service) closeAllSMSCodes(ctx context.Context, userID uuid.UUID, at time.Time) error {
	for _, purpose := range smsPurposes {
		if err := s.phones().CloseOpenSMSCodes(ctx, userID, purpose, at); err != nil {
			return err
		}
	}
	return nil
}

// SendPhoneChangeCode implements Service (sendPhoneChangeCode, R): a code goes
// by SMS to the current number (10 minutes) for a change of the phone factor.
// 409 identity.phone_not_verified without a number,
// identity.phone_code_not_required when the change would not ask for it.
func (s *service) SendPhoneChangeCode(ctx context.Context, a authz.Actor, proof ReauthProof) (SMSCodeSent, error) {
	if err := authz.Can(ctx, a, authz.IdentityPhoneChange, ownUser(a)); err != nil {
		return SMSCodeSent{}, err
	}
	if err := s.smsReady(); err != nil {
		return SMSCodeSent{}, err
	}
	p, found, err := s.phones().Phone(ctx, a.UserID)
	if err != nil {
		return SMSCodeSent{}, err
	}
	if !found {
		return SMSCodeSent{}, ErrPhoneNotVerified
	}
	step := map[string]any{"step": stepPhoneChangeCode}
	kind, err := s.reauth(ctx, a, proof, reauthDenied(authz.IdentityPhoneChange, resourceUser, &a.UserID, step))
	if err != nil {
		return SMSCodeSent{}, err
	}
	if !p.asksChangeCode(kind) {
		return SMSCodeSent{}, ErrPhoneCodeNotRequired
	}
	if err := s.limitSMS(ctx, a.UserID, SMSPhoneChange, p.Number); err != nil {
		return SMSCodeSent{}, err
	}
	var sent SMSCodeSent
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		c, err := s.issueSMSCode(ctx, a.UserID, SMSPhoneChange, p.Number, time.Time{})
		if err != nil {
			return err
		}
		sent = SMSCodeSent{ExpiresAt: c.ExpiresAt, PhoneHint: MaskPhone(p.Number)}
		return s.d.Audit.Log(ctx, event(authz.IdentityPhoneChange, audit.Success, resourceUser, a.UserID, map[string]any{
			"step": stepPhoneChangeCode, audit.MetaReauth: kind, "phone_hint": sent.PhoneHint, "sms_code_id": c.ID.String(),
		}).By(a))
	})
	if err != nil {
		return SMSCodeSent{}, err
	}
	return sent, nil
}

// StartPhoneVerification implements Service (startPhoneVerification, R): a
// code goes by SMS to number (10 minutes). The answer is the same whether
// or not another account holds the number. phoneCode is the code of the
// current number, when the change asks for one (checkChangeCode).
func (s *service) StartPhoneVerification(ctx context.Context, a authz.Actor, raw string, proof ReauthProof, phoneCode string) (SMSCodeSent, error) {
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
	cur, _, err := s.phones().Phone(ctx, a.UserID)
	if err != nil {
		return SMSCodeSent{}, err
	}
	changeCode, err := s.checkChangeCode(ctx, a, authz.IdentityPhoneChange, cur, kind, phoneCode)
	if err != nil {
		return SMSCodeSent{}, err
	}
	if err := s.limitSMS(ctx, a.UserID, SMSPhoneVerify, number); err != nil {
		return SMSCodeSent{}, err
	}
	var sent SMSCodeSent
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		if err := s.consumeChangeCode(ctx, changeCode); err != nil {
			return err
		}
		c, err := s.issueSMSCode(ctx, a.UserID, SMSPhoneVerify, number, time.Time{})
		if err != nil {
			return err
		}
		sent = SMSCodeSent{ExpiresAt: c.ExpiresAt, PhoneHint: MaskPhone(number)}
		return s.d.Audit.Log(ctx, event(authz.IdentityPhoneChange, audit.Success, resourceUser, a.UserID, withChangeCode(
			map[string]any{audit.MetaReauth: kind, "phone_hint": sent.PhoneHint, "sms_code_id": c.ID.String()}, changeCode)).By(a))
	})
	if err != nil {
		return SMSCodeSent{}, err
	}
	return sent, nil
}

// ConfirmPhoneVerification implements Service (confirmPhoneVerification):
// the SMS code makes its number the caller's (an earlier number is
// replaced; the SMS second step stays as it was, the open codes and the
// password reset by SMS of the earlier number end). 422
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
		replaced := had && cur.Number != c.Number
		p := Phone{UserID: a.UserID, Number: c.Number, VerifiedAt: now}
		if had {
			p.SMSMFAEnabledAt, p.SMSResetEnabledAt = cur.SMSMFAEnabledAt, cur.SMSResetEnabledAt
		}
		if replaced {
			if err := s.closeAllSMSCodes(ctx, a.UserID, now); err != nil {
				return err
			}
			if err := s.closeSMSReset(ctx, cur, a, smsResetClosedNumber); err != nil {
				return err
			}
			p.SMSResetEnabledAt = nil
		}
		if err := s.phones().PutPhone(ctx, p); err != nil {
			return err
		}
		view = phoneView(p, a)
		s.notifyPhone(ctx, a.UserID, NoticePhoneVerified, view.Hint)
		return s.d.Audit.Log(ctx, event(authz.IdentityPhoneVerify, audit.Success, resourceUser, a.UserID, map[string]any{
			"phone_hint": view.Hint, "sms_code_id": c.ID.String(), "replaced": replaced,
		}).By(a))
	})
	if err != nil {
		return PhoneView{}, err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "phone number verified", slog.String("user_id", a.UserID.String()))
	return view, nil
}

// RemovePhone implements Service (removeMyPhone, R): the number goes, and
// with it the SMS second step, the password reset by SMS and the open SMS
// codes. phoneCode as in StartPhoneVerification.
func (s *service) RemovePhone(ctx context.Context, a authz.Actor, proof ReauthProof, phoneCode string) error {
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
	changeCode, err := s.checkChangeCode(ctx, a, authz.IdentityPhoneChange, p, kind, phoneCode)
	if err != nil {
		return err
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		if err := s.consumeChangeCode(ctx, changeCode); err != nil {
			return err
		}
		if _, err := s.phones().DeletePhone(ctx, a.UserID); err != nil {
			return err
		}
		if err := s.closeAllSMSCodes(ctx, a.UserID, s.now()); err != nil {
			return err
		}
		s.notifyPhone(ctx, a.UserID, NoticePhoneRemoved, MaskPhone(p.Number))
		return s.d.Audit.Log(ctx, event(authz.IdentityPhoneChange, audit.Success, resourceUser, a.UserID, withChangeCode(map[string]any{
			audit.MetaReauth: kind, "phone_hint": MaskPhone(p.Number), "removed": true, "sms_mfa_was_on": p.SMSMFAEnabledAt != nil,
			"sms_reset_was_on": p.SMSResetEnabledAt != nil,
		}, changeCode)).By(a))
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
	return s.setSMSMFA(ctx, a, true, kind, p, uuid.Nil())
}

// DisableSMSMFA implements Service (disableSmsMfa, R): the SMS second step
// goes off (the number stays); already off: nothing changes. phoneCode as in
// StartPhoneVerification.
func (s *service) DisableSMSMFA(ctx context.Context, a authz.Actor, proof ReauthProof, phoneCode string) error {
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
	changeCode, err := s.checkChangeCode(ctx, a, authz.IdentitySmsMfaDisable, p, kind, phoneCode)
	if err != nil {
		return err
	}
	return s.setSMSMFA(ctx, a, false, kind, p, changeCode)
}

// setSMSMFA turns the SMS second step of the caller (number p) on or off,
// notifies and audits it. Turning it on closes the password reset by SMS and
// an open verification of a new number, which was started without the code
// of the current number. changeCode is consumed, the nil UUID: none.
func (s *service) setSMSMFA(ctx context.Context, a authz.Actor, on bool, kind string, p Phone, changeCode uuid.UUID) error {
	act, notice := authz.IdentitySmsMfaDisable, NoticeSMSMFADisabled
	now := s.now()
	var at *time.Time
	if on {
		act, notice, at = authz.IdentitySmsMfaEnable, NoticeSMSMFAEnabled, &now
	}
	hint := MaskPhone(p.Number)
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		if err := s.consumeChangeCode(ctx, changeCode); err != nil {
			return err
		}
		if on {
			if err := s.closeSMSReset(ctx, p, a, smsResetClosedSMSMFA); err != nil {
				return err
			}
			if err := s.phones().CloseOpenSMSCodes(ctx, a.UserID, SMSPhoneVerify, now); err != nil {
				return err
			}
		}
		changed, err := s.phones().SetSMSMFA(ctx, a.UserID, at)
		if err != nil {
			return err
		}
		if !changed {
			return ErrPhoneNotVerified // removed meanwhile
		}
		s.notifyPhone(ctx, a.UserID, notice, hint)
		return s.d.Audit.Log(ctx, event(act, audit.Success, resourceUser, a.UserID, withChangeCode(
			map[string]any{audit.MetaReauth: kind, "phone_hint": hint}, changeCode)).By(a))
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

// myPhone returns the number of a's account (getMe); nil when none.
func (s *service) myPhone(ctx context.Context, a authz.Actor, userID uuid.UUID) (*PhoneView, error) {
	p, found, err := s.phones().Phone(ctx, userID)
	if err != nil || !found {
		return nil, err
	}
	v := phoneView(p, a)
	return &v, nil
}
