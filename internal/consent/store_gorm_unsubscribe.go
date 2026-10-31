package consent

import (
	"context"
	"errors"
	"fmt"
	"time"
	"uuid"

	"gorm.io/gorm"

	"koro/internal/platform/db"
)

// linkRow scans one unsubscribe_links row (000162).
type linkRow struct {
	ID          uuid.UUID `gorm:"column:id"`
	TokenHash   []byte    `gorm:"column:token_hash"`
	UserID      uuid.UUID `gorm:"column:user_id"`
	PurposeCode string    `gorm:"column:purpose_code"`
	CreatedAt   time.Time `gorm:"column:created_at"`
	ExpiresAt   time.Time `gorm:"column:expires_at"`
}

// InsertUnsubscribeLink implements UnsubscribeLinkStore.
//
// cross-org: a link belongs to a user, not an org.
func (s gormStore) InsertUnsubscribeLink(ctx context.Context, l UnsubscribeToken) error {
	err := db.Conn(ctx, s.orm).Exec(`INSERT INTO unsubscribe_links (id, token_hash, user_id, purpose_code, created_at, expires_at)
		VALUES ( ?, ?, ?, ?, ?, ?)`, l.ID, l.TokenHash, l.UserID, string(l.Purpose), l.CreatedAt, l.ExpiresAt).Error
	if err != nil {
		return fmt.Errorf("consent: insert unsubscribe link: %w", db.Translate(err))
	}
	return nil
}

// UnsubscribeLinkByHash implements UnsubscribeLinkStore
// (unsubscribe_links_token_hash_uq).
//
// cross-org: a link belongs to a user, not an org.
func (s gormStore) UnsubscribeLinkByHash(ctx context.Context, hash []byte) (UnsubscribeToken, error) {
	var r linkRow
	err := db.Conn(ctx, s.orm).Table("unsubscribe_links").
		Select("id, token_hash, user_id, purpose_code, created_at, expires_at").
		Where("token_hash = ?", hash).Take(&r).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return UnsubscribeToken{}, ErrNotFound
	case err != nil:
		return UnsubscribeToken{}, fmt.Errorf("consent: unsubscribe link: %w", err)
	}
	return UnsubscribeToken{
		ID: r.ID, TokenHash: r.TokenHash, UserID: r.UserID, Purpose: Purpose(r.PurposeCode),
		CreatedAt: r.CreatedAt.UTC(), ExpiresAt: r.ExpiresAt.UTC(),
	}, nil
}

// DeleteUnsubscribeLinksOf implements UnsubscribeLinkStore
// (unsubscribe_links_user_idx).
//
// cross-org: a link belongs to a user, not an org.
func (s gormStore) DeleteUnsubscribeLinksOf(ctx context.Context, userID uuid.UUID) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM unsubscribe_links WHERE user_id = ?`, userID)
	if res.Error != nil {
		return 0, fmt.Errorf("consent: delete unsubscribe links of user: %w", res.Error)
	}
	return int(res.RowsAffected), nil
}

// DeleteExpiredUnsubscribeLinks implements UnsubscribeLinkStore
// (unsubscribe_links_expires_at_idx).
//
// cross-org: retention spans every user.
func (s gormStore) DeleteExpiredUnsubscribeLinks(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM unsubscribe_links WHERE id IN (
		SELECT id FROM unsubscribe_links WHERE expires_at < ? ORDER BY expires_at, id LIMIT ?)`, cutoff, max(limit, 1))
	if res.Error != nil {
		return 0, fmt.Errorf("consent: delete expired unsubscribe links: %w", res.Error)
	}
	return int(res.RowsAffected), nil
}
