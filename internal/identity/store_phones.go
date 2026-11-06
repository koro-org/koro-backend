package identity

import (
	"bytes"
	"context"
	"time"
	"uuid"
)

// Phone is the verified mobile number of an account (user_phones, 000177;
// BE-172): at most one per account, one account per number.
type Phone struct {
	UserID uuid.UUID
	// Number is E.164 (+905XXXXXXXXX).
	Number     string
	VerifiedAt time.Time
	// SMSMFAEnabledAt: the SMS second step of sign-in is on since then.
	SMSMFAEnabledAt *time.Time
	CreatedAt       time.Time
}

// SMSPurpose is what an SMS code proves (sms_codes_purpose_chk).
type SMSPurpose string

// SMS code purposes.
const (
	SMSPhoneVerify   SMSPurpose = "phone_verify"   // the holder of a new number
	SMSLogin         SMSPurpose = "login"          // the second step of a sign-in
	SMSPasswordReset SMSPurpose = "password_reset" // a password reset by SMS
)

// SMSCode is one SMS code sent (sms_codes, 000177). The code itself is
// derived from ID under KeyID and never stored; CodeHash is a keyed MAC of
// it (SMSCodeMAC).
type SMSCode struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	Purpose    SMSPurpose
	Number     string // E.164; the number being verified for phone_verify
	KeyID      string
	CodeHash   []byte
	Attempts   int
	ExpiresAt  time.Time
	ConsumedAt *time.Time
	SentAt     *time.Time
	CreatedAt  time.Time
}

// SMSCodeUse is how much a purpose of an account was used in a window: the
// codes opened and the verification attempts they took.
type SMSCodeUse struct {
	Codes    int
	Attempts int
}

func (c SMSCode) clone() SMSCode {
	out := c
	out.CodeHash = bytes.Clone(c.CodeHash)
	out.ConsumedAt = cloneTime(c.ConsumedAt)
	out.SentAt = cloneTime(c.SentAt)
	return out
}

// phoneStore is the user_phones and sms_codes rows (000177; BE-172): the
// verified number of an account, its SMS second step, and the one-time
// codes of phone verification, SMS sign-in and SMS password reset.
type phoneStore interface {
	// Phone returns the verified number of userID; found=false when none.
	Phone(ctx context.Context, userID uuid.UUID) (p Phone, found bool, err error)
	// PhoneOwner returns the account holding number; found=false when none.
	PhoneOwner(ctx context.Context, number string) (userID uuid.UUID, found bool, err error)
	// PutPhone stores the verified number of p.UserID, replacing an earlier
	// one (number, verified_at and sms_mfa_enabled_at as given).
	// ErrPhoneTaken when another account holds the number.
	PutPhone(ctx context.Context, p Phone) error
	// SetSMSMFA turns the SMS second step of userID on (at) or off (nil);
	// changed=false when the account has no number.
	SetSMSMFA(ctx context.Context, userID uuid.UUID, at *time.Time) (changed bool, err error)
	// DeletePhone deletes the number of userID and returns how many rows
	// it deleted.
	DeletePhone(ctx context.Context, userID uuid.UUID) (int, error)

	// InsertSMSCode writes a code row (created_at as given, like the
	// e-mail tokens: the expiry CHECK compares the service's times).
	InsertSMSCode(ctx context.Context, c SMSCode) error
	// SMSCode returns a code row or ErrNotFound.
	SMSCode(ctx context.Context, id uuid.UUID) (SMSCode, error)
	// OpenSMSCode returns the newest unconsumed code of (userID, purpose);
	// found=false when none.
	OpenSMSCode(ctx context.Context, userID uuid.UUID, purpose SMSPurpose) (c SMSCode, found bool, err error)
	// CloseOpenSMSCodes consumes every open code of (userID, purpose) at at:
	// a new code replaces the previous ones.
	CloseOpenSMSCodes(ctx context.Context, userID uuid.UUID, purpose SMSPurpose, at time.Time) error
	// AddSMSCodeAttempt counts one verification attempt of an open code
	// and returns the new count (at most 10); ErrNotFound when the code is
	// unknown.
	AddSMSCodeAttempt(ctx context.Context, id uuid.UUID) (attempts int, err error)
	// ConsumeSMSCode sets consumed_at of an open code; consumed=false when
	// it already was.
	ConsumeSMSCode(ctx context.Context, id uuid.UUID, at time.Time) (consumed bool, err error)
	// MarkSMSCodeSent sets sent_at of a code not sent yet; marked=false
	// when it already was.
	MarkSMSCodeSent(ctx context.Context, id uuid.UUID, at time.Time) (marked bool, err error)
	// SMSCodeUsage counts the codes of (userID, purpose) created at since or
	// later, consumed or not, and the attempts they took
	// (sms_codes_user_created_idx).
	SMSCodeUsage(ctx context.Context, userID uuid.UUID, purpose SMSPurpose, since time.Time) (SMSCodeUse, error)
	// DeleteEndedSMSCodes deletes up to limit codes that expired before
	// cutoff (retention sms_code) and returns how many.
	DeleteEndedSMSCodes(ctx context.Context, cutoff time.Time, limit int) (int, error)
	// DeleteUserSMSCodes deletes every code of userID (erasure).
	DeleteUserSMSCodes(ctx context.Context, userID uuid.UUID) (int, error)
}
