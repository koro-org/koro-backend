package identity

import (
	"context"
	"log/slog"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/queue"
)

// The password reset by SMS is the choice of the account's holder (BE-186,
// user decision Q-51): off by default, e-mail stays the standard way.
//
//   - enableSmsPasswordReset (R) turns it on for the verified number and it
//     works from SMSResetWait later; disableSmsPasswordReset (R) turns it off.
//     Both send a security notice by e-mail and write an audit record.
//   - It is for accounts without a role: a platform role or any org role
//     beyond plain membership refuses it (smsResetBarred). It is also never
//     next to the SMS second step (user_phones_sms_reset_sms_mfa_chk).
//   - It closes by itself, and stays closed until the holder turns it on
//     again, when the number changes, the SMS second step goes on, a role
//     arrives or an admin resets the account's MFA.
//   - Only the holder sees it (getMe, the KVKK export). requestPasswordReset
//     answers the same whatever it is.
const (
	// SMSResetWait is how long after the holder's choice the password reset
	// by SMS starts working.
	SMSResetWait = 7 * 24 * time.Hour

	// CloseSMSResetTaskName closes the choice of a user whose new membership
	// role refuses it (membership.started).
	CloseSMSResetTaskName = "identity.close_sms_reset"
)

// Notice kinds of identity.phone_notice (BE-186).
const (
	NoticeSMSResetEnabled  = "sms_reset_enabled"
	NoticeSMSResetDisabled = "sms_reset_disabled"
)

// Why the choice closed by itself (audit metadata.closed_by).
const (
	smsResetClosedNumber   = "number_changed"
	smsResetClosedSMSMFA   = "sms_mfa_on"
	smsResetClosedRole     = "role"
	smsResetClosedMFAReset = "mfa_reset"
)

// smsResetActive reports whether a reset code may go to the number at now by
// the holder's choice (the roles are smsResetBarred's).
func (p Phone) smsResetActive(now time.Time) bool {
	return p.SMSResetEnabledAt != nil && p.SMSMFAEnabledAt == nil && !now.Before(p.SMSResetEnabledAt.Add(SMSResetWait))
}

// smsResetBarredFor reports whether a holds a role that refuses the password
// reset by SMS: from the actor and, as a second look, from the stored roles
// and the membership source (like requiresMFA).
func (s *service) smsResetBarredFor(ctx context.Context, a authz.Actor) (bool, error) {
	if smsResetBarred(a.PlatformRoles, a.Memberships) {
		return true, nil
	}
	roles, ms, err := s.heldRoles(ctx, a.UserID)
	if err != nil {
		return false, err
	}
	return smsResetBarred(roles, ms), nil
}

// EnableSMSReset implements Service (enableSmsPasswordReset, R). 409
// identity.phone_not_verified without a number, identity.sms_reset_not_allowed
// for a role holder or with the SMS second step on (both before the proof:
// they depend only on the caller's account, like k-sms); already on: nothing
// changes.
func (s *service) EnableSMSReset(ctx context.Context, a authz.Actor, proof ReauthProof) error {
	if err := authz.Can(ctx, a, authz.IdentitySmsResetEnable, ownUser(a)); err != nil {
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
	barred, err := s.smsResetBarredFor(ctx, a)
	if err != nil {
		return err
	}
	if barred || p.SMSMFAEnabledAt != nil {
		return ErrSMSResetNotAllowed
	}
	if p.SMSResetEnabledAt != nil {
		return nil
	}
	kind, err := s.reauth(ctx, a, proof, reauthDenied(authz.IdentitySmsResetEnable, resourceUser, &a.UserID, nil))
	if err != nil {
		return err
	}
	return s.setSMSReset(ctx, a, true, kind, MaskPhone(p.Number))
}

// DisableSMSReset implements Service (disableSmsPasswordReset, R): the reset
// goes by e-mail again and an open reset code closes (the number stays);
// already off: nothing changes.
func (s *service) DisableSMSReset(ctx context.Context, a authz.Actor, proof ReauthProof) error {
	if err := authz.Can(ctx, a, authz.IdentitySmsResetDisable, ownUser(a)); err != nil {
		return err
	}
	p, found, err := s.phones().Phone(ctx, a.UserID)
	if err != nil {
		return err
	}
	if !found || p.SMSResetEnabledAt == nil {
		return nil
	}
	kind, err := s.reauth(ctx, a, proof, reauthDenied(authz.IdentitySmsResetDisable, resourceUser, &a.UserID, nil))
	if err != nil {
		return err
	}
	return s.setSMSReset(ctx, a, false, kind, MaskPhone(p.Number))
}

// setSMSReset turns the choice of the caller on or off, notifies and audits it.
func (s *service) setSMSReset(ctx context.Context, a authz.Actor, on bool, kind, hint string) error {
	act, notice := authz.IdentitySmsResetDisable, NoticeSMSResetDisabled
	now := s.now()
	var at *time.Time
	if on {
		act, notice, at = authz.IdentitySmsResetEnable, NoticeSMSResetEnabled, &now
	}
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		changed, err := s.phones().SetSMSReset(ctx, a.UserID, at)
		if err != nil {
			return err
		}
		if !changed {
			return ErrPhoneNotVerified // removed meanwhile
		}
		if !on {
			if err := s.phones().CloseOpenSMSCodes(ctx, a.UserID, SMSPasswordReset, now); err != nil {
				return err
			}
		}
		s.notifyPhone(ctx, a.UserID, notice, hint)
		return s.d.Audit.Log(ctx, event(act, audit.Success, resourceUser, a.UserID,
			map[string]any{audit.MetaReauth: kind, "phone_hint": hint}).By(a))
	})
	if err != nil {
		return err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "SMS password reset changed", slog.String("user_id", a.UserID.String()), slog.Bool("on", on))
	return nil
}

// closeSMSReset closes the choice of p, when it is on, in the transaction
// carried by ctx: the open reset code closes, the holder is told by e-mail,
// the record names the cause. by is who caused it.
func (s *service) closeSMSReset(ctx context.Context, p Phone, by authz.Actor, cause string) error {
	if p.SMSResetEnabledAt == nil {
		return nil
	}
	if _, err := s.phones().SetSMSReset(ctx, p.UserID, nil); err != nil {
		return err
	}
	if err := s.phones().CloseOpenSMSCodes(ctx, p.UserID, SMSPasswordReset, s.now()); err != nil {
		return err
	}
	hint := MaskPhone(p.Number)
	s.notifyPhone(ctx, p.UserID, NoticeSMSResetDisabled, hint)
	return s.d.Audit.Log(ctx, event(authz.IdentitySmsResetDisable, audit.Success, resourceUser, p.UserID,
		map[string]any{"closed_by": cause, "phone_hint": hint}).By(by))
}

// closeSMSResetOf is closeSMSReset for an account known by its ID.
func (s *service) closeSMSResetOf(ctx context.Context, userID uuid.UUID, by authz.Actor, cause string) error {
	p, found, err := s.phones().Phone(ctx, userID)
	if err != nil || !found {
		return err
	}
	return s.closeSMSReset(ctx, p, by, cause)
}

// handleMembershipStarted is identity.close_sms_reset (membership.started,
// mechanism A): a membership role that refuses the password reset by SMS
// closes the choice for good. Delivery is at most once: smsResetNumber never
// honours the choice next to such a role and closes it when it meets one.
func (s *service) handleMembershipStarted(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.MembershipStarted](e)
	if err != nil {
		return queue.Permanent(err)
	}
	if !smsResetBarred(nil, []authz.Membership{{Role: authz.MemberRole(p.Role)}}) {
		return nil
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		return s.closeSMSResetOf(ctx, p.UserID, authz.SystemActor(CloseSMSResetTaskName), smsResetClosedRole)
	})
}
