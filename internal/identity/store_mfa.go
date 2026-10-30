package identity

import (
	"context"
	"time"
	"uuid"
)

// mfaStore is the second factor of an account (02 §5.5–5.6): the mfa_totp
// row with its replay guard and the mfa_recovery_codes rows (enrollment,
// MFA login, step-up, disable and the admin reset; 05 §14.1).
type mfaStore interface {
	// TOTP returns the TOTP row of userID; found=false when none.
	TOTP(ctx context.Context, userID uuid.UUID) (t TOTP, found bool, err error)
	// PutTOTP writes a new, unconfirmed enrollment, replacing an earlier
	// unconfirmed one. A confirmed row stays: ErrMFAAlreadyEnabled.
	PutTOTP(ctx context.Context, t TOTP) error
	// ConfirmTOTP confirms the open enrollment of userID at at and records
	// step as used; confirmed=false when there is no open enrollment.
	ConfirmTOTP(ctx context.Context, userID uuid.UUID, step int64, at time.Time) (confirmed bool, err error)
	// UseTOTPStep is the replay guard (05 §14.1): it records step as the
	// last used step of a confirmed TOTP only if no step ≥ step was used
	// before, atomically. used=false: replay (or no confirmed TOTP).
	UseTOTPStep(ctx context.Context, userID uuid.UUID, step int64) (used bool, err error)
	// DeleteTOTP deletes the TOTP row of userID (disable, admin reset,
	// erasure) and returns how many rows it deleted.
	DeleteTOTP(ctx context.Context, userID uuid.UUID) (int, error)

	// ReplaceRecoveryCodes deletes every recovery code of userID and
	// stores hashes (SHA-256, 32 bytes each) as the new set.
	ReplaceRecoveryCodes(ctx context.Context, userID uuid.UUID, hashes [][]byte, at time.Time) error
	// UseRecoveryCode marks the unused code of userID with hash as used;
	// used=false when there is none (unknown or already used: single use).
	UseRecoveryCode(ctx context.Context, userID uuid.UUID, hash []byte, at time.Time) (used bool, err error)
	// RecoveryCodesLeft returns how many unused recovery codes userID has.
	RecoveryCodesLeft(ctx context.Context, userID uuid.UUID) (int, error)
	// DeleteRecoveryCodes deletes every recovery code of userID.
	DeleteRecoveryCodes(ctx context.Context, userID uuid.UUID) (int, error)
}
