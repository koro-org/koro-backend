package identity

import (
	"context"
	"fmt"
	"time"
	"uuid"

	"gorm.io/gorm/clause"

	"koro/internal/platform/db"
)

// The user_phones and sms_codes rows of the PostgreSQL store (000177;
// BE-172). Both belong to a user, never to an org.

type phoneRow struct {
	UserID          uuid.UUID  `gorm:"column:user_id;primaryKey"`
	Number          string     `gorm:"column:phone_e164"`
	VerifiedAt      time.Time  `gorm:"column:verified_at"`
	SMSMFAEnabledAt *time.Time `gorm:"column:sms_mfa_enabled_at"`
	CreatedAt       time.Time  `gorm:"column:created_at;default:now()"`
}

// TableName implements gorm's Tabler.
func (phoneRow) TableName() string { return "user_phones" }

func (r phoneRow) phone() Phone {
	return Phone{UserID: r.UserID, Number: r.Number, VerifiedAt: r.VerifiedAt.UTC(), SMSMFAEnabledAt: db.UTCPtr(r.SMSMFAEnabledAt),
		CreatedAt: r.CreatedAt.UTC()}
}

type smsCodeRow struct {
	ID         uuid.UUID  `gorm:"column:id;primaryKey"`
	UserID     uuid.UUID  `gorm:"column:user_id"`
	Purpose    string     `gorm:"column:purpose"`
	Number     string     `gorm:"column:phone_e164"`
	KeyID      string     `gorm:"column:key_id"`
	CodeHash   []byte     `gorm:"column:code_hash"`
	Attempts   int16      `gorm:"column:attempts"`
	ExpiresAt  time.Time  `gorm:"column:expires_at"`
	ConsumedAt *time.Time `gorm:"column:consumed_at"`
	SentAt     *time.Time `gorm:"column:sent_at"`
	CreatedAt  time.Time  `gorm:"column:created_at"`
}

// TableName implements gorm's Tabler.
func (smsCodeRow) TableName() string { return "sms_codes" }

func (r smsCodeRow) code() SMSCode {
	return SMSCode{
		ID: r.ID, UserID: r.UserID, Purpose: SMSPurpose(r.Purpose), Number: r.Number, KeyID: r.KeyID, CodeHash: r.CodeHash,
		Attempts: int(r.Attempts), ExpiresAt: r.ExpiresAt.UTC(), ConsumedAt: db.UTCPtr(r.ConsumedAt), SentAt: db.UTCPtr(r.SentAt),
		CreatedAt: r.CreatedAt.UTC(),
	}
}

// Phone implements Store.
//
// cross-org: a phone number belongs to a user, not an org.
func (s gormStore) Phone(ctx context.Context, userID uuid.UUID) (Phone, bool, error) {
	var r phoneRow
	if err := db.Conn(ctx, s.orm).Where("user_id = ?", userID).Take(&r).Error; err != nil {
		if db.IsNotFound(err) {
			return Phone{}, false, nil
		}
		return Phone{}, false, fmt.Errorf("identity: phone: %w", err)
	}
	return r.phone(), true, nil
}

// PhoneOwner implements Store (user_phones_phone_uq).
//
// cross-org: a phone number belongs to a user, not an org.
func (s gormStore) PhoneOwner(ctx context.Context, number string) (uuid.UUID, bool, error) {
	var r phoneRow
	if err := db.Conn(ctx, s.orm).Select("user_id").Where("phone_e164 = ?", number).Take(&r).Error; err != nil {
		if db.IsNotFound(err) {
			return uuid.Nil(), false, nil
		}
		return uuid.Nil(), false, fmt.Errorf("identity: phone owner: %w", err)
	}
	return r.UserID, true, nil
}

// PutPhone implements Store: an upsert on user_id (created_at stays).
//
// cross-org: a phone number belongs to a user, not an org.
func (s gormStore) PutPhone(ctx context.Context, p Phone) error {
	r := phoneRow{UserID: p.UserID, Number: p.Number, VerifiedAt: p.VerifiedAt, SMSMFAEnabledAt: p.SMSMFAEnabledAt}
	err := db.Conn(ctx, s.orm).Omit("created_at").Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "user_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"phone_e164", "verified_at", "sms_mfa_enabled_at"}),
	}).Create(&r).Error
	if err != nil {
		return fmt.Errorf("identity: put phone: %w", constraints.Translate(err))
	}
	return nil
}

// SetSMSMFA implements Store.
//
// cross-org: a phone number belongs to a user, not an org.
func (s gormStore) SetSMSMFA(ctx context.Context, userID uuid.UUID, at *time.Time) (bool, error) {
	res := db.Conn(ctx, s.orm).Model(&phoneRow{}).Where("user_id = ?", userID).Update("sms_mfa_enabled_at", at)
	if res.Error != nil {
		return false, fmt.Errorf("identity: set SMS second step: %w", db.Translate(res.Error))
	}
	return res.RowsAffected == 1, nil
}

// DeletePhone implements Store.
//
// cross-org: a phone number belongs to a user, not an org.
func (s gormStore) DeletePhone(ctx context.Context, userID uuid.UUID) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM user_phones WHERE user_id = ?`, userID)
	if res.Error != nil {
		return 0, fmt.Errorf("identity: delete phone: %w", res.Error)
	}
	return int(res.RowsAffected), nil
}

// InsertSMSCode implements Store.
//
// cross-org: SMS codes belong to a user, not an org.
func (s gormStore) InsertSMSCode(ctx context.Context, c SMSCode) error {
	r := smsCodeRow{
		ID: c.ID, UserID: c.UserID, Purpose: string(c.Purpose), Number: c.Number, KeyID: c.KeyID, CodeHash: c.CodeHash,
		Attempts: int16(c.Attempts), ExpiresAt: c.ExpiresAt, ConsumedAt: c.ConsumedAt, SentAt: c.SentAt, CreatedAt: c.CreatedAt, //nolint:gosec // G115: attempts is 0..10 (sms_codes_attempts_chk)
	}
	if err := db.Conn(ctx, s.orm).Create(&r).Error; err != nil {
		return fmt.Errorf("identity: insert SMS code: %w", db.Translate(err))
	}
	return nil
}

// SMSCode implements Store.
//
// cross-org: SMS codes belong to a user, not an org.
func (s gormStore) SMSCode(ctx context.Context, id uuid.UUID) (SMSCode, error) {
	var r smsCodeRow
	if err := db.Conn(ctx, s.orm).Where("id = ?", id).Take(&r).Error; err != nil {
		if db.IsNotFound(err) {
			return SMSCode{}, ErrNotFound
		}
		return SMSCode{}, fmt.Errorf("identity: SMS code: %w", err)
	}
	return r.code(), nil
}

// OpenSMSCode implements Store (sms_codes_open_idx).
//
// cross-org: SMS codes belong to a user, not an org.
func (s gormStore) OpenSMSCode(ctx context.Context, userID uuid.UUID, purpose SMSPurpose) (SMSCode, bool, error) {
	var r smsCodeRow
	err := db.Conn(ctx, s.orm).Where("user_id = ? AND purpose = ? AND consumed_at IS NULL", userID, string(purpose)).
		Order("id DESC").Take(&r).Error
	if err != nil {
		if db.IsNotFound(err) {
			return SMSCode{}, false, nil
		}
		return SMSCode{}, false, fmt.Errorf("identity: open SMS code: %w", err)
	}
	return r.code(), true, nil
}

// CloseOpenSMSCodes implements Store (sms_codes_open_idx).
//
// cross-org: SMS codes belong to a user, not an org.
func (s gormStore) CloseOpenSMSCodes(ctx context.Context, userID uuid.UUID, purpose SMSPurpose, at time.Time) error {
	err := db.Conn(ctx, s.orm).Model(&smsCodeRow{}).Where("user_id = ? AND purpose = ? AND consumed_at IS NULL", userID, string(purpose)).
		Update("consumed_at", at).Error
	if err != nil {
		return fmt.Errorf("identity: close SMS codes: %w", db.Translate(err))
	}
	return nil
}

// AddSMSCodeAttempt implements Store: one conditional UPDATE (concurrent
// guesses each count).
//
// cross-org: SMS codes belong to a user, not an org.
func (s gormStore) AddSMSCodeAttempt(ctx context.Context, id uuid.UUID) (int, error) {
	var attempts []int
	err := db.Conn(ctx, s.orm).Raw(`UPDATE sms_codes SET attempts = LEAST(attempts + 1, ?) WHERE id = ? RETURNING attempts`,
		maxSMSAttemptsStored, id).Scan(&attempts).Error
	if err != nil {
		return 0, fmt.Errorf("identity: count SMS code attempt: %w", db.Translate(err))
	}
	if len(attempts) == 0 {
		return 0, ErrNotFound
	}
	return attempts[0], nil
}

// ConsumeSMSCode implements Store.
//
// cross-org: SMS codes belong to a user, not an org.
func (s gormStore) ConsumeSMSCode(ctx context.Context, id uuid.UUID, at time.Time) (bool, error) {
	res := db.Conn(ctx, s.orm).Model(&smsCodeRow{}).Where("id = ? AND consumed_at IS NULL", id).Update("consumed_at", at)
	if res.Error != nil {
		return false, fmt.Errorf("identity: consume SMS code: %w", db.Translate(res.Error))
	}
	return res.RowsAffected == 1, nil
}

// MarkSMSCodeSent implements Store.
//
// cross-org: SMS codes belong to a user, not an org.
func (s gormStore) MarkSMSCodeSent(ctx context.Context, id uuid.UUID, at time.Time) (bool, error) {
	res := db.Conn(ctx, s.orm).Model(&smsCodeRow{}).Where("id = ? AND sent_at IS NULL", id).Update("sent_at", at)
	if res.Error != nil {
		return false, fmt.Errorf("identity: mark SMS code sent: %w", db.Translate(res.Error))
	}
	return res.RowsAffected == 1, nil
}

// SMSCodeUsage implements Store (sms_codes_user_created_idx).
//
// cross-org: SMS codes belong to a user, not an org.
func (s gormStore) SMSCodeUsage(ctx context.Context, userID uuid.UUID, purpose SMSPurpose, since time.Time) (SMSCodeUse, error) {
	var r struct {
		Codes    int `gorm:"column:codes"`
		Attempts int `gorm:"column:attempts"`
	}
	err := db.Conn(ctx, s.orm).Raw(`SELECT count(*) AS codes, coalesce(sum(attempts), 0) AS attempts FROM sms_codes
		WHERE user_id = ? AND purpose = ? AND created_at >= ?`, userID, string(purpose), since).Scan(&r).Error
	if err != nil {
		return SMSCodeUse{}, fmt.Errorf("identity: SMS code usage: %w", db.Translate(err))
	}
	return SMSCodeUse(r), nil
}

// DeleteEndedSMSCodes implements Store (retention sms_code; sms_codes_expires_at_idx).
//
// cross-org: retention sweep over every user's codes.
func (s gormStore) DeleteEndedSMSCodes(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM sms_codes WHERE id IN (
		SELECT id FROM sms_codes WHERE expires_at < ? LIMIT ?)`, cutoff, limit)
	if res.Error != nil {
		return 0, fmt.Errorf("identity: delete ended SMS codes: %w", res.Error)
	}
	return int(res.RowsAffected), nil
}

// DeleteUserSMSCodes implements Store.
//
// cross-org: SMS codes belong to a user, not an org.
func (s gormStore) DeleteUserSMSCodes(ctx context.Context, userID uuid.UUID) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM sms_codes WHERE user_id = ?`, userID)
	if res.Error != nil {
		return 0, fmt.Errorf("identity: delete user SMS codes: %w", res.Error)
	}
	return int(res.RowsAffected), nil
}
