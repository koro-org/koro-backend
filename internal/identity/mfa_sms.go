package identity

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/problem"
)

// The SMS second step of sign-in (BE-172). An account signs in with a second
// factor when TOTP is on or the SMS second step is on (secondFactors). The
// password step answers a challenge naming the factors; for sms the client
// asks for the code (sendMfaLoginSms: at most maxChallengeSMS per
// challenge, within the SMS send limits) and completes with sms_code. A
// wrong SMS code counts like a wrong TOTP code (account lockout, the reauth
// class, the challenge's attempts). An SMS sign-in is never MFA-verified
// (openSession).

// Second factors of a sign-in (audit metadata.mfa, MFAChallenge.Methods).
const (
	factorRecoveryCode = "recovery_code"
	factorSMS          = "sms"
	// maxChallengeSMS is how many SMS codes one challenge may send.
	maxChallengeSMS = 3
)

// loginFactors are the second factors an account signs in with.
type loginFactors struct {
	TOTP  bool   // TOTP and recovery codes
	SMS   bool   // an SMS code to Phone
	Phone string // E.164
}

func (f loginFactors) any() bool { return f.TOTP || f.SMS }

// methods lists the factors in the order clients offer them.
func (f loginFactors) methods() []string {
	var out []string
	if f.TOTP {
		out = append(out, proofTOTP, factorRecoveryCode)
	}
	if f.SMS {
		out = append(out, factorSMS)
	}
	return out
}

// secondFactors returns the second factors of u: TOTP when it is on; the
// SMS code when the SMS second step is on, except next to TOTP for a holder
// of an MFA role (03 §6.4, k-sms: their factor is TOTP alone, SIM swap). An
// MFA role holder whose only second step is SMS (turned on before the role
// arrived) keeps it as the sign-in's guard; the session it opens is not
// MFA-verified, so the role paths still need TOTP (03 §6.4-2).
func (s *service) secondFactors(ctx context.Context, u User) (loginFactors, error) {
	f := loginFactors{TOTP: u.MFAEnabled}
	p, found, err := s.phones().Phone(ctx, u.ID)
	if err != nil || !found || p.SMSMFAEnabledAt == nil {
		return f, err
	}
	if f.TOTP {
		roles, err := s.roles().PlatformRoles(ctx, u.ID)
		if err != nil {
			return f, err
		}
		ms, err := s.memberships(ctx, u.ID)
		if err != nil {
			return f, err
		}
		if privileged(roles, ms) {
			return f, nil
		}
	}
	f.SMS, f.Phone = true, p.Number
	return f, nil
}

// useLoginSMSCode verifies and consumes the open sign-in code of userID.
func (s *service) useLoginSMSCode(ctx context.Context, userID uuid.UUID, raw string) (bool, error) {
	c, ok, err := s.matchSMSCode(ctx, userID, SMSLogin, raw)
	if err != nil || !ok {
		return false, err
	}
	return s.phones().ConsumeSMSCode(ctx, c.ID, s.now())
}

// SendMFALoginSMS implements Service (sendMfaLoginSms): an SMS code for the
// challenge of the password step (5 minutes, never past the challenge). An
// unknown or expired challenge and an account without the SMS second step
// give the same 401 identity.mfa_login_failed as completeMfaLogin; the
// challenge stays usable.
func (s *service) SendMFALoginSMS(ctx context.Context, a authz.Actor, challengeID uuid.UUID) (SMSCodeSent, error) {
	if err := authz.Can(ctx, a, authz.IdentitySessionCreate, publicUser); err != nil {
		return SMSCodeSent{}, err
	}
	if err := s.challengesReady(); err != nil {
		return SMSCodeSent{}, err
	}
	if err := s.smsReady(); err != nil {
		return SMSCodeSent{}, err
	}
	key := challengeKey(challengeID)
	raw, found, err := s.d.Challenges.Take(ctx, key)
	if err != nil {
		return SMSCodeSent{}, fmt.Errorf("identity: take challenge: %w", err)
	}
	if !found {
		return SMSCodeSent{}, ErrMFALoginFailed
	}
	var c challenge
	if err := json.Unmarshal(raw, &c); err != nil {
		return SMSCodeSent{}, fmt.Errorf("identity: challenge: %w", err)
	}
	now := s.now()
	if !now.Before(c.ExpiresAt) {
		return SMSCodeSent{}, ErrMFALoginFailed
	}
	sent, err := s.sendLoginCode(ctx, &c, now)
	s.restoreChallenge(ctx, key, c)
	return sent, err
}

// sendLoginCode sends the sign-in code of challenge c (counted in c).
func (s *service) sendLoginCode(ctx context.Context, c *challenge, now time.Time) (SMSCodeSent, error) {
	u, err := s.users().User(ctx, c.UserID)
	switch {
	case isNotFound(err):
		return SMSCodeSent{}, ErrMFALoginFailed
	case err != nil:
		return SMSCodeSent{}, err
	}
	if u.Status != StatusActive || u.locked(now) {
		return SMSCodeSent{}, ErrMFALoginFailed
	}
	f, err := s.secondFactors(ctx, u)
	if err != nil {
		return SMSCodeSent{}, err
	}
	if !f.SMS {
		return SMSCodeSent{}, ErrMFALoginFailed
	}
	if c.SMSSent >= maxChallengeSMS {
		return SMSCodeSent{}, problem.ErrRateLimited
	}
	if err := s.limitSMS(ctx, u.ID, SMSLogin, f.Phone); err != nil {
		return SMSCodeSent{}, err
	}
	var code SMSCode
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		code, err = s.issueSMSCode(ctx, u.ID, SMSLogin, f.Phone, minTime(now.Add(smsCodeTTL(SMSLogin)), c.ExpiresAt))
		return err
	})
	if err != nil {
		return SMSCodeSent{}, err
	}
	c.SMSSent++
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "sign-in SMS code issued", slog.String("user_id", u.ID.String()),
		slog.String("sms_code_id", code.ID.String()))
	return SMSCodeSent{ExpiresAt: code.ExpiresAt, PhoneHint: MaskPhone(f.Phone)}, nil
}

// restoreChallenge puts a taken challenge back for its remaining lifetime.
func (s *service) restoreChallenge(ctx context.Context, key string, c challenge) {
	ttl := c.ExpiresAt.Sub(s.now())
	if ttl <= 0 {
		return
	}
	b, err := json.Marshal(c)
	if err == nil {
		err = s.d.Challenges.Put(ctx, key, b, ttl)
	}
	if err != nil {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "mfa challenge not restored", slog.String("err", err.Error()))
	}
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
