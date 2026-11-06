package identity

import (
	"context"
	"crypto/hmac"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/crypto"
	"koro/internal/platform/obs"
	"koro/internal/platform/queue"
	"koro/internal/platform/sms"
)

// One-time SMS codes (BE-172). A code is six digits derived (HMAC under
// CRYPTO_TOKEN_KEYS, crypto.Deriver) from its sms_codes row ID, like the
// e-mail tokens: it is never stored or queued. The row keeps a keyed MAC of
// the code (SMSCodeMAC), so the database alone cannot brute-force the six
// digits; the worker re-derives the code, checks it against the MAC and
// sends it (identity.send_sms_code, mechanism A: the payload is the row
// ID). A code is single use, expires (smsCodeTTL) and allows
// maxSMSAttempts verifications; a new code of the same purpose closes the
// open one. Numbers and codes never reach a log line, an audit record
// (the masked number only: obs.MaskPhone) or an error.
const (
	// SendSMSCodeTaskName sends one SMS code (mechanism A, no topic).
	SendSMSCodeTaskName = "identity.send_sms_code"
	// SMSCodesCleanupJobName deletes ended codes (retention sms_code).
	SMSCodesCleanupJobName = "identity.sms_codes_cleanup" // 03:40, low
	// SMSCodeRetention is the default of retention sms_code (000177).
	SMSCodeRetention = 7 * 24 * time.Hour

	smsCodeDigits        = 6
	smsCodePurpo         = "identity.sms_code"
	smsCodeMACPurpo      = "identity.sms_code_mac"
	phoneDigestPurpo     = "identity.rate_limit_phone"
	maxSMSAttempts       = 5  // verifications of one code
	maxSMSAttemptsStored = 10 // sms_codes_attempts_chk
	reasonInvalidSMSCode = "invalid_sms_code"
	reasonSMSResetLimit  = "sms_reset_limit"
)

// smsCodeTTL is the lifetime of a code of purpose (a sign-in code also ends
// with its challenge).
func smsCodeTTL(p SMSPurpose) time.Duration {
	if p == SMSLogin {
		return 5 * time.Minute
	}
	return 10 * time.Minute // phone_verify, password_reset, phone_change
}

// smsTexts are the messages (plain ASCII: Netgsm's OTP route takes no
// Turkish characters), Turkish and English: code and validity in minutes.
var smsTexts = map[SMSPurpose][2]string{
	SMSPhoneVerify: {"Koro telefon dogrulama kodunuz: %s. Kod %d dakika gecerlidir; kimseyle paylasmayin.",
		"Your Koro phone verification code: %s. Valid for %d minutes; do not share it."},
	SMSLogin: {"Koro giris kodunuz: %s. Kod %d dakika gecerlidir; kimseyle paylasmayin.",
		"Your Koro sign-in code: %s. Valid for %d minutes; do not share it."},
	SMSPasswordReset: {"Koro parola sifirlama kodunuz: %s. Kod %d dakika gecerlidir. Siz istemediyseniz dikkate almayin.",
		"Your Koro password reset code: %s. Valid for %d minutes. Ignore it if you did not ask for it."},
	SMSPhoneChange: {"Koro telefon degisikligi onay kodunuz: %s. Kod %d dakika gecerlidir. Siz istemediyseniz parolanizi degistirin.",
		"Your Koro phone change code: %s. Valid for %d minutes. If this was not you, change your password."},
}

// SMSCodeSent is the answer of an operation that sent a code: when it
// expires and the masked number it went to.
type SMSCodeSent struct {
	ExpiresAt time.Time
	PhoneHint string
}

// MaskPhone is the masked form of a number in answers, audit records and
// e-mails (the BE-108 log mask: the last two digits).
func MaskPhone(number string) string { return obs.MaskPhone(number) }

// smsReady fails closed where no SMS adapter is wired.
func (s *service) smsReady() error {
	if s.d.SMS == nil {
		return ErrSMSUnavailable
	}
	return nil
}

// SMSCodeMAC is the stored MAC of code of the row id under keyID (32
// bytes). Fixtures that seed a code with a chosen value use it.
func SMSCodeMAC(d *crypto.Deriver, keyID string, id uuid.UUID, code string) ([]byte, error) {
	b, err := d.DerivedToken(smsCodeMACPurpo, keyID, id[:], []byte(code))
	if err != nil {
		return nil, fmt.Errorf("identity: SMS code MAC: %w", err)
	}
	return b, nil
}

// deriveSMSCode re-creates the code of a row: six decimal digits of
// DerivedToken("identity.sms_code", keyID, id).
func (s *service) deriveSMSCode(keyID string, id uuid.UUID) (string, error) {
	b, err := s.d.Deriver.DerivedToken(smsCodePurpo, keyID, id[:])
	if err != nil {
		return "", fmt.Errorf("identity: derive SMS code: %w", err)
	}
	return fmt.Sprintf("%06d", binary.BigEndian.Uint64(b[:8])%1_000_000), nil
}

// normalizeSMSCode keeps the digits of a typed code ("123 456" → "123456");
// ok=false when it cannot be a code.
func normalizeSMSCode(raw string) (string, bool) {
	c := strings.Join(strings.Fields(raw), "")
	if len(c) != smsCodeDigits {
		return "", false
	}
	for _, r := range c {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	return c, true
}

// issueSMSCode opens a new code of purpose for userID to number in the
// transaction carried by ctx (the open codes of the purpose close) and
// queues its sending after the commit. expiresAt zero: now + smsCodeTTL.
func (s *service) issueSMSCode(ctx context.Context, userID uuid.UUID, purpose SMSPurpose, number string, expiresAt time.Time) (SMSCode, error) {
	now := s.now()
	if expiresAt.IsZero() {
		expiresAt = now.Add(smsCodeTTL(purpose))
	}
	c := SMSCode{ID: uuid.NewV7(), UserID: userID, Purpose: purpose, Number: number, KeyID: s.d.Deriver.ActiveKeyID(),
		ExpiresAt: expiresAt, CreatedAt: now}
	code, err := s.deriveSMSCode(c.KeyID, c.ID)
	if err != nil {
		return SMSCode{}, err
	}
	if c.CodeHash, err = SMSCodeMAC(s.d.Deriver, c.KeyID, c.ID, code); err != nil {
		return SMSCode{}, err
	}
	if err := s.phones().CloseOpenSMSCodes(ctx, userID, purpose, now); err != nil {
		return SMSCode{}, err
	}
	if err := s.phones().InsertSMSCode(ctx, c); err != nil {
		return SMSCode{}, err
	}
	p := smsCodeTask{CodeID: c.ID}
	s.d.Queue.AfterCommit(ctx, SendSMSCodeTaskName, p, SendSMSCodeTaskName+":"+c.ID.String())
	return c, nil
}

// matchSMSCode checks raw against the open code of (userID, purpose): the
// attempt is counted first (maxSMSAttempts per code, the right one
// included), then the MAC is compared in constant time. ok=false for a
// wrong, expired, used or exhausted code, or none; the caller consumes a
// matching code in its transaction (ConsumeSMSCode).
func (s *service) matchSMSCode(ctx context.Context, userID uuid.UUID, purpose SMSPurpose, raw string) (SMSCode, bool, error) {
	code, ok := normalizeSMSCode(raw)
	if !ok {
		return SMSCode{}, false, nil
	}
	c, found, err := s.phones().OpenSMSCode(ctx, userID, purpose)
	if err != nil || !found || !s.now().Before(c.ExpiresAt) || c.Attempts >= maxSMSAttempts {
		return SMSCode{}, false, err
	}
	n, err := s.phones().AddSMSCodeAttempt(ctx, c.ID)
	if err != nil {
		if isNotFound(err) {
			return SMSCode{}, false, nil
		}
		return SMSCode{}, false, err
	}
	if n > maxSMSAttempts {
		return SMSCode{}, false, nil
	}
	want, err := SMSCodeMAC(s.d.Deriver, c.KeyID, c.ID, code)
	if errors.Is(err, crypto.ErrUnknownKey) {
		return SMSCode{}, false, nil // the key left CRYPTO_TOKEN_KEYS: the code is dead
	}
	if err != nil {
		return SMSCode{}, false, err
	}
	return c, hmac.Equal(want, c.CodeHash), nil
}

// smsCodeDenied writes the denied record of a refused SMS code of act on
// the account userID by a (anonymous on the public operations): the reason
// is invalid_sms_code; the code and the number are never in it.
func (s *service) smsCodeDenied(ctx context.Context, act authz.Action, userID uuid.UUID, a authz.Actor, purpose SMSPurpose) error {
	return s.smsDenied(ctx, act, userID, a, purpose, reasonInvalidSMSCode)
}

// smsDenied writes a denied record of an SMS code with reason.
func (s *service) smsDenied(ctx context.Context, act authz.Action, userID uuid.UUID, a authz.Actor, purpose SMSPurpose, reason string) error {
	e := event(act, audit.Denied, resourceUser, userID, map[string]any{"sms_purpose": string(purpose)}).By(a)
	e.Reason = reason
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error { return s.d.Audit.Log(ctx, e) })
}

// phoneDigest is the pseudonymous rate limit key of a number (an HMAC
// under the token keys, like the e-mail digest).
func (s *service) phoneDigest(number string) (string, error) {
	b, err := s.d.Deriver.DerivedToken(phoneDigestPurpo, s.d.Deriver.ActiveKeyID(), []byte(number))
	if err != nil {
		return "", fmt.Errorf("identity: phone digest: %w", err)
	}
	return hex.EncodeToString(b[:12]), nil
}

// smsCodeTask is the payload of identity.send_sms_code: the row ID only
// (05 §8.4-1).
type smsCodeTask struct {
	CodeID uuid.UUID `json:"code_id"`
}

// handleSendSMSCode is the identity.send_sms_code handler: it re-derives
// the code of the row, checks it against the stored MAC and sends it once.
// A used, expired or already sent code is skipped; a refusal of the
// provider (sms.ErrRejected) is permanent, anything else is retried while
// the code lives.
func (s *service) handleSendSMSCode(ctx context.Context, payload []byte) error {
	var p smsCodeTask
	if err := json.Unmarshal(payload, &p); err != nil || p.CodeID == uuid.Nil() {
		return queue.Permanent(fmt.Errorf("identity: SMS code payload: %w", errors.Join(err, errors.New("missing code_id"))))
	}
	c, err := s.phones().SMSCode(ctx, p.CodeID)
	switch {
	case isNotFound(err):
		return nil // erased or cleaned up meanwhile
	case err != nil:
		return err
	}
	if c.ConsumedAt != nil || c.SentAt != nil || !s.now().Before(c.ExpiresAt) {
		return nil // replaced, used, sent or expired: nothing to send
	}
	if s.d.SMS == nil {
		s.d.Log.LogAttrs(ctx, slog.LevelError, "SMS code not sent: no SMS adapter in the worker (SMS_PROVIDER)",
			slog.String("sms_code_id", c.ID.String()))
		return queue.Permanent(ErrSMSUnavailable)
	}
	u, err := s.users().User(ctx, c.UserID)
	switch {
	case isNotFound(err):
		return nil
	case err != nil:
		return err
	}
	if u.Status == StatusErased || u.Status == StatusSuspended {
		return nil
	}
	code, err := s.deriveSMSCode(c.KeyID, c.ID)
	if err != nil {
		return queue.Permanent(err) // the key was removed from CRYPTO_TOKEN_KEYS
	}
	want, err := SMSCodeMAC(s.d.Deriver, c.KeyID, c.ID, code)
	if err != nil || !hmac.Equal(want, c.CodeHash) {
		return queue.Permanent(errors.New("identity: derived SMS code does not match the stored MAC (token keys changed)"))
	}
	msg := sms.Message{To: c.Number, Text: smsText(c.Purpose, u.Locale, code), Ref: "sms_code:" + c.ID.String()}
	if _, err := s.d.SMS.Send(ctx, msg); err != nil {
		if errors.Is(err, sms.ErrRejected) {
			s.smsCounts.inc(c.Purpose, SMSOutcomeRejected)
			s.d.Log.LogAttrs(ctx, slog.LevelWarn, "SMS code refused by the provider", slog.String("sms_code_id", c.ID.String()),
				slog.String("purpose", string(c.Purpose)), slog.Bool("invalid_number", errors.Is(err, sms.ErrInvalidNumber)),
				slog.String("err", err.Error()))
			return queue.Permanent(err)
		}
		s.smsCounts.inc(c.Purpose, SMSOutcomeRetried)
		return err
	}
	s.smsCounts.inc(c.Purpose, SMSOutcomeSent)
	if _, err := s.phones().MarkSMSCodeSent(ctx, c.ID, s.now()); err != nil {
		// Sent already: a retry would send the same code again; log only.
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "SMS code sent but not marked", slog.String("sms_code_id", c.ID.String()),
			slog.String("err", err.Error()))
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "SMS code sent", slog.String("sms_code_id", c.ID.String()),
		slog.String("purpose", string(c.Purpose)))
	return nil
}

// smsText is the message of a code of purpose in locale (Turkish unless
// en-US).
func smsText(p SMSPurpose, locale, code string) string {
	t := smsTexts[p]
	i := 0
	if locale == LocaleEN {
		i = 1
	}
	return fmt.Sprintf(t[i], code, int(smsCodeTTL(p).Minutes()))
}
