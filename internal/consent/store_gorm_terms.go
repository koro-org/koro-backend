package consent

import (
	"context"
	"fmt"
	"time"
	"uuid"

	"koro/internal/platform/db"
)

// termsRow scans one terms_acceptances row (000148).
type termsRow struct {
	ID              uuid.UUID `gorm:"column:id"`
	UserID          uuid.UUID `gorm:"column:user_id"`
	NoticeVersionID uuid.UUID `gorm:"column:notice_version_id"`
	TextSHA256      []byte    `gorm:"column:text_sha256"`
	Channel         string    `gorm:"column:channel"`
	RequestID       *string   `gorm:"column:request_id"`
	AcceptedAt      time.Time `gorm:"column:accepted_at"`
	CreatedAt       time.Time `gorm:"column:created_at"`
}

// InsertTermsAcceptance implements TermsStore (ON CONFLICT on
// terms_acceptances_user_notice_uq: a second acceptance of a version is a
// no-op).
//
// cross-org: personal ledger addressed by user.
func (s gormStore) InsertTermsAcceptance(ctx context.Context, t TermsAcceptance) (bool, error) {
	res := db.Conn(ctx, s.orm).Exec(`INSERT INTO terms_acceptances (id, user_id, notice_version_id, text_sha256, channel,
		request_id, accepted_at) VALUES ( ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (user_id, notice_version_id) DO NOTHING`,
		t.ID, t.UserID, t.NoticeVersionID, t.TextSHA256, string(t.Channel), db.Nullable(t.RequestID), t.AcceptedAt)
	if res.Error != nil {
		return false, fmt.Errorf("consent: insert terms acceptance: %w", db.Translate(res.Error))
	}
	return res.RowsAffected == 1, nil
}

// TermsAcceptancesOf implements TermsStore (terms_acceptances_user_notice_uq
// leads with user_id).
//
// cross-org: personal ledger addressed by user.
func (s gormStore) TermsAcceptancesOf(ctx context.Context, userID uuid.UUID) ([]TermsAcceptance, error) {
	var rows []termsRow
	err := db.Conn(ctx, s.orm).Table("terms_acceptances").
		Select("id, user_id, notice_version_id, text_sha256, channel, request_id, accepted_at, created_at").
		Where("user_id = ?", userID).Order("accepted_at, id").Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("consent: terms acceptances of user: %w", err)
	}
	out := make([]TermsAcceptance, len(rows))
	for i, r := range rows {
		out[i] = TermsAcceptance{
			ID: r.ID, UserID: r.UserID, NoticeVersionID: r.NoticeVersionID, TextSHA256: r.TextSHA256,
			Channel: Channel(r.Channel), AcceptedAt: r.AcceptedAt.UTC(), CreatedAt: r.CreatedAt.UTC(),
		}
		if r.RequestID != nil {
			out[i].RequestID = *r.RequestID
		}
	}
	return out, nil
}

// PurgeTerms implements TermsStore.
//
// cross-org: retention spans every user.
func (s gormStore) PurgeTerms(ctx context.Context, cutoff time.Time, limit int, dryRun bool) (int, error) {
	var n int64
	if err := db.Conn(ctx, s.orm).Raw(`SELECT terms_acceptances_purge( ?, ?, ?)`, cutoff, max(limit, 1), dryRun).Scan(&n).Error; err != nil {
		return 0, fmt.Errorf("consent: purge terms acceptances: %w", db.Translate(err))
	}
	return int(n), nil
}
