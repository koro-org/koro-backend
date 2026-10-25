package identity

import (
	"context"
	"fmt"
	"time"
	"uuid"

	"gorm.io/gorm/clause"

	"koro/internal/platform/db"
)

// eduRow is one edu_verifications row (02 §5.4); created_at and updated_at
// are read only (see userRow).
type eduRow struct {
	ID                 uuid.UUID  `gorm:"column:id;primaryKey"`
	UserID             uuid.UUID  `gorm:"column:user_id"`
	Method             string     `gorm:"column:method"`
	EduEmail           *string    `gorm:"column:edu_email"`
	EduEmailNormalized *string    `gorm:"column:edu_email_normalized"`
	EmailDomain        *string    `gorm:"column:email_domain"`
	UniversityID       *uuid.UUID `gorm:"column:university_id"`
	DocumentFileID     *uuid.UUID `gorm:"column:document_file_id"`
	Status             string     `gorm:"column:status"`
	VerifiedAt         *time.Time `gorm:"column:verified_at"`
	ExpiresAt          *time.Time `gorm:"column:expires_at"`
	ReviewedBy         *uuid.UUID `gorm:"column:reviewed_by"`
	ReviewNote         *string    `gorm:"column:review_note"`
	CreatedAt          time.Time  `gorm:"column:created_at;->"`
	UpdatedAt          time.Time  `gorm:"column:updated_at;->"`
}

// TableName implements gorm's Tabler.
func (eduRow) TableName() string { return "edu_verifications" }

func eduRowOf(v EduRecord) eduRow {
	return eduRow{
		ID: v.ID, UserID: v.UserID, Method: string(v.Method), EduEmail: db.Nullable(v.EduEmail),
		EduEmailNormalized: db.Nullable(v.EduEmailNormalized), EmailDomain: db.Nullable(v.EmailDomain),
		UniversityID: v.UniversityID, DocumentFileID: v.DocumentFileID, Status: string(v.Status),
		VerifiedAt: v.VerifiedAt, ExpiresAt: v.ExpiresAt, ReviewedBy: v.ReviewedBy, ReviewNote: db.Nullable(v.ReviewNote),
		CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt,
	}
}

func (r eduRow) record() EduRecord {
	return EduRecord{
		ID: r.ID, UserID: r.UserID, Method: EduMethod(r.Method), EduEmail: str(r.EduEmail),
		EduEmailNormalized: str(r.EduEmailNormalized), EmailDomain: str(r.EmailDomain), UniversityID: r.UniversityID,
		DocumentFileID: r.DocumentFileID, Status: EduStatus(r.Status), VerifiedAt: db.UTCPtr(r.VerifiedAt),
		ExpiresAt: db.UTCPtr(r.ExpiresAt), ReviewedBy: r.ReviewedBy, ReviewNote: str(r.ReviewNote),
		CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC(),
	}
}

func eduRecords(rows []eduRow) []EduRecord {
	out := make([]EduRecord, len(rows))
	for i, r := range rows {
		out[i] = r.record()
	}
	return out
}

// eduConstraints maps the edu_verifications constraints to module errors.
var eduConstraints = db.Constraints{
	"edu_verifications_one_pending_uq":        ErrEduPending,
	"edu_verifications_email_uq":              ErrEduEmailTaken,
	"edu_verifications_university_id_fkey":    ErrUniversityUnknown,
	"edu_verifications_document_file_id_fkey": ErrInvalidEduDocument,
	"edu_verifications_reviewer_chk":          ErrForbidden, // 03 Y6: nobody reviews their own document
}

// InsertEduVerification implements Store.
//
// cross-org: student verifications belong to no org.
func (s gormStore) InsertEduVerification(ctx context.Context, v EduRecord) error {
	r := eduRowOf(v)
	if err := db.Conn(ctx, s.orm).Create(&r).Error; err != nil {
		return fmt.Errorf("identity: insert edu verification: %w", eduConstraints.Translate(err))
	}
	return nil
}

// EduVerification implements Store.
//
// cross-org: student verifications belong to no org.
func (s gormStore) EduVerification(ctx context.Context, id uuid.UUID) (EduRecord, error) {
	return s.eduVerification(ctx, id, false)
}

// LockEduVerification implements Store.
//
// cross-org: student verifications belong to no org.
func (s gormStore) LockEduVerification(ctx context.Context, id uuid.UUID) (EduRecord, error) {
	return s.eduVerification(ctx, id, true)
}

// cross-org: student verifications belong to no org.
func (s gormStore) eduVerification(ctx context.Context, id uuid.UUID, lock bool) (EduRecord, error) {
	q := db.Conn(ctx, s.orm)
	if lock {
		q = q.Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate})
	}
	var r eduRow
	if err := q.Where("id = ?", id).Take(&r).Error; err != nil {
		if db.IsNotFound(err) {
			return EduRecord{}, ErrNotFound
		}
		return EduRecord{}, fmt.Errorf("identity: edu verification: %w", err)
	}
	return r.record(), nil
}

// LockOpenEduVerifications implements Store.
//
// cross-org: student verifications belong to no org.
func (s gormStore) LockOpenEduVerifications(ctx context.Context, userID uuid.UUID) ([]EduRecord, error) {
	var rows []eduRow
	err := db.Conn(ctx, s.orm).Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate}).
		Where("user_id = ? AND status IN ?", userID, []string{string(EduPending), string(EduVerified)}).
		Order("id").Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("identity: open edu verifications: %w", err)
	}
	return eduRecords(rows), nil
}

// UpdateEduVerification implements Store (the touch trigger stamps
// updated_at).
//
// cross-org: student verifications belong to no org.
func (s gormStore) UpdateEduVerification(ctx context.Context, v EduRecord) error {
	r := eduRowOf(v)
	res := db.Conn(ctx, s.orm).Model(&eduRow{}).Where("id = ?", v.ID).Updates(map[string]any{
		"status": r.Status, "university_id": r.UniversityID, "document_file_id": r.DocumentFileID,
		"verified_at": r.VerifiedAt, "expires_at": r.ExpiresAt, "reviewed_by": r.ReviewedBy, "review_note": r.ReviewNote,
	})
	if res.Error != nil {
		return fmt.Errorf("identity: update edu verification: %w", eduConstraints.Translate(res.Error))
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// EduVerificationsOf implements Store.
//
// cross-org: student verifications belong to no org.
func (s gormStore) EduVerificationsOf(ctx context.Context, userID uuid.UUID) ([]EduRecord, error) {
	var rows []eduRow
	if err := db.Conn(ctx, s.orm).Where("user_id = ?", userID).Order("id DESC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("identity: edu verifications of user: %w", err)
	}
	return eduRecords(rows), nil
}

// LatestEduOutcome implements Store.
//
// cross-org: student verifications belong to no org.
func (s gormStore) LatestEduOutcome(ctx context.Context, userID uuid.UUID) (EduRecord, bool, error) {
	var rows []eduRow
	err := db.Conn(ctx, s.orm).
		Where("user_id = ? AND status IN ?", userID, []string{string(EduVerified), string(EduExpired), string(EduRevoked)}).
		Order("id DESC").Limit(1).Find(&rows).Error
	if err != nil {
		return EduRecord{}, false, fmt.Errorf("identity: latest edu outcome: %w", err)
	}
	if len(rows) == 0 {
		return EduRecord{}, false, nil
	}
	return rows[0].record(), true, nil
}

// ActiveEduVerifications implements Store (index
// edu_verifications_one_verified_uq).
//
// cross-org: student verifications belong to no org.
func (s gormStore) ActiveEduVerifications(ctx context.Context, userIDs []uuid.UUID, now time.Time) ([]EduRecord, error) {
	if len(userIDs) == 0 {
		return nil, nil
	}
	var rows []eduRow
	err := db.Conn(ctx, s.orm).Where("user_id IN ? AND status = ? AND expires_at > ?", userIDs, string(EduVerified), now).
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("identity: active edu verifications: %w", err)
	}
	return eduRecords(rows), nil
}

// OpenEduVerificationOfEmail implements Store (index edu_verifications_email_uq).
//
// cross-org: student verifications belong to no org.
func (s gormStore) OpenEduVerificationOfEmail(ctx context.Context, normalized string) (EduRecord, bool, error) {
	var rows []eduRow
	err := db.Conn(ctx, s.orm).Where("edu_email_normalized = ? AND status IN ?", normalized,
		[]string{string(EduPending), string(EduVerified)}).Limit(1).Find(&rows).Error
	if err != nil {
		return EduRecord{}, false, fmt.Errorf("identity: edu verification of address: %w", err)
	}
	if len(rows) == 0 {
		return EduRecord{}, false, nil
	}
	return rows[0].record(), true, nil
}

// EduVerificationOfFile implements Store.
//
// cross-org: student verifications belong to no org.
func (s gormStore) EduVerificationOfFile(ctx context.Context, fileID uuid.UUID) (EduRecord, bool, error) {
	var rows []eduRow
	if err := db.Conn(ctx, s.orm).Where("document_file_id = ?", fileID).Limit(1).Find(&rows).Error; err != nil {
		return EduRecord{}, false, fmt.Errorf("identity: edu verification of file: %w", err)
	}
	if len(rows) == 0 {
		return EduRecord{}, false, nil
	}
	return rows[0].record(), true, nil
}

// EduQueue implements Store.
//
// cross-org: the platform staff queue spans every user.
func (s gormStore) EduQueue(ctx context.Context, q EduQuery) ([]EduRecord, error) {
	tx := db.Conn(ctx, s.orm).Where("method = ? AND status = ?", string(EduMethodDocument), string(q.Status))
	switch {
	case q.After != nil && q.newestFirst():
		tx = tx.Where("id < ?", *q.After)
	case q.After != nil:
		tx = tx.Where("id > ?", *q.After)
	}
	order := "id"
	if q.newestFirst() {
		order = "id DESC"
	}
	var rows []eduRow
	if err := tx.Order(order).Limit(q.Limit).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("identity: edu queue: %w", err)
	}
	return eduRecords(rows), nil
}

// ExpireEduVerifications implements Store (index edu_verifications_expiry_idx).
//
// cross-org: the nightly expiry spans every user.
func (s gormStore) ExpireEduVerifications(ctx context.Context, now time.Time, limit int) ([]EduRecord, error) {
	var rows []eduRow
	err := db.Conn(ctx, s.orm).Raw(`UPDATE edu_verifications SET status = 'expired'
		WHERE id IN (SELECT id FROM edu_verifications WHERE status = 'verified' AND expires_at <= ?
		             ORDER BY expires_at, id LIMIT ? FOR UPDATE SKIP LOCKED)
		RETURNING *`, now, limit).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("identity: expire edu verifications: %w", err)
	}
	return eduRecords(rows), nil
}

// EduExpiringBetween implements Store (index edu_verifications_expiry_idx).
//
// cross-org: the reminder job spans every user.
func (s gormStore) EduExpiringBetween(ctx context.Context, from, to time.Time, after *uuid.UUID, limit int) ([]EduRecord, error) {
	tx := db.Conn(ctx, s.orm).Where("status = ? AND expires_at >= ? AND expires_at < ?", string(EduVerified), from, to)
	if after != nil {
		tx = tx.Where("id > ?", *after)
	}
	var rows []eduRow
	if err := tx.Order("id").Limit(limit).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("identity: expiring edu verifications: %w", err)
	}
	return eduRecords(rows), nil
}

// DeleteEduVerification implements Store.
//
// cross-org: student verifications belong to no org.
func (s gormStore) DeleteEduVerification(ctx context.Context, id uuid.UUID) (bool, error) {
	res := db.Conn(ctx, s.orm).Where("id = ?", id).Delete(&eduRow{})
	if res.Error != nil {
		return false, fmt.Errorf("identity: delete edu verification: %w", res.Error)
	}
	return res.RowsAffected > 0, nil
}

// DeleteUserEduVerifications implements Store.
//
// cross-org: student verifications belong to no org.
func (s gormStore) DeleteUserEduVerifications(ctx context.Context, userID uuid.UUID) ([]EduRecord, error) {
	var rows []eduRow
	if err := db.Conn(ctx, s.orm).Raw(`DELETE FROM edu_verifications WHERE user_id = ? RETURNING *`, userID).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("identity: delete edu verifications of user: %w", err)
	}
	return eduRecords(rows), nil
}
