package notify

import (
	"context"
	"errors"
	"fmt"
	"time"
	"uuid"

	"koro/internal/platform/db"
)

// Web push subscriptions of the GORM store (web_push_subscriptions,
// migration 000175; BE-173).

const webSubCols = `id, user_id, session_id, endpoint, endpoint_sha256, p256dh, auth, created_at, last_seen_at`

type webSubRow struct {
	ID             uuid.UUID  `gorm:"column:id"`
	UserID         uuid.UUID  `gorm:"column:user_id"`
	SessionID      *uuid.UUID `gorm:"column:session_id"`
	Endpoint       string     `gorm:"column:endpoint"`
	EndpointSHA256 []byte     `gorm:"column:endpoint_sha256"`
	P256DH         []byte     `gorm:"column:p256dh"`
	Auth           []byte     `gorm:"column:auth"`
	CreatedAt      time.Time  `gorm:"column:created_at"`
	LastSeenAt     time.Time  `gorm:"column:last_seen_at"`
}

// cross-org: row conversion / helper; no query of its own beyond its callers.
func (r webSubRow) subscription() WebPushSubscription {
	return WebPushSubscription{ID: r.ID, UserID: r.UserID, SessionID: r.SessionID, Endpoint: r.Endpoint,
		EndpointSHA256: r.EndpointSHA256, P256DH: r.P256DH, Auth: r.Auth, CreatedAt: r.CreatedAt.UTC(),
		LastSeenAt: r.LastSeenAt.UTC()}
}

// UpsertWebPushSubscription implements Store.
//
// cross-org: the user's own browsers (BE-173).
func (s gormStore) UpsertWebPushSubscription(ctx context.Context, w WebPushSubscription, keep int) (WebPushSubscription, error) {
	conn := db.Conn(ctx, s.orm)
	var rows []webSubRow
	err := conn.Raw(`INSERT INTO web_push_subscriptions
		(id, user_id, session_id, endpoint, endpoint_sha256, p256dh, auth, created_at, last_seen_at)
		VALUES ( ?, ?, ?, ?, ?, ?, ?, ?, ? )
		ON CONFLICT (endpoint_sha256) DO UPDATE SET user_id = EXCLUDED.user_id, session_id = EXCLUDED.session_id,
			endpoint = EXCLUDED.endpoint, p256dh = EXCLUDED.p256dh, auth = EXCLUDED.auth, last_seen_at = EXCLUDED.last_seen_at
		RETURNING `+webSubCols,
		w.ID, w.UserID, w.SessionID, w.Endpoint, w.EndpointSHA256, w.P256DH, w.Auth, w.CreatedAt, w.LastSeenAt).
		Scan(&rows).Error
	if err != nil {
		return WebPushSubscription{}, fmt.Errorf("notify: upsert web push subscription: %w", db.Translate(err))
	}
	if len(rows) != 1 {
		return WebPushSubscription{}, errors.New("notify: upsert web push subscription returned no row")
	}
	stored := rows[0].subscription()
	// The per-user cap: the stored row and the keep-1 most recently seen
	// others stay.
	if err := conn.Exec(`DELETE FROM web_push_subscriptions WHERE user_id = ? AND id <> ? AND id NOT IN (
			SELECT id FROM web_push_subscriptions WHERE user_id = ? AND id <> ?
			ORDER BY last_seen_at DESC, id DESC LIMIT ? )`,
		stored.UserID, stored.ID, stored.UserID, stored.ID, max(keep-1, 0)).Error; err != nil {
		return WebPushSubscription{}, fmt.Errorf("notify: cap web push subscriptions: %w", db.Translate(err))
	}
	return stored, nil
}

// DeleteWebPushSubscription implements Store.
//
// cross-org: the user's own browsers (BE-173).
func (s gormStore) DeleteWebPushSubscription(ctx context.Context, userID, id uuid.UUID) error {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM web_push_subscriptions WHERE id = ? AND user_id = ?`, id, userID)
	if res.Error != nil {
		return fmt.Errorf("notify: delete web push subscription: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrWebPushSubscriptionNotFound
	}
	return nil
}

// WebPushSubscriptionsOf implements Store.
//
// cross-org: the user's own browsers (BE-173).
func (s gormStore) WebPushSubscriptionsOf(ctx context.Context, userID uuid.UUID) ([]WebPushSubscription, error) {
	var rows []webSubRow
	if err := db.Conn(ctx, s.orm).Raw(`SELECT `+webSubCols+` FROM web_push_subscriptions WHERE user_id = ? ORDER BY id`, userID).
		Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("notify: web push subscriptions: %w", err)
	}
	out := make([]WebPushSubscription, len(rows))
	for i, r := range rows {
		out[i] = r.subscription()
	}
	return out, nil
}

// DropWebPushSubscription implements Store.
//
// cross-org: a subscription the push service reported gone (BE-173).
func (s gormStore) DropWebPushSubscription(ctx context.Context, id uuid.UUID) (bool, error) {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM web_push_subscriptions WHERE id = ?`, id)
	if res.Error != nil {
		return false, fmt.Errorf("notify: drop web push subscription: %w", res.Error)
	}
	return res.RowsAffected == 1, nil
}

// DeleteWebPushSubscriptionsOf implements Store.
//
// cross-org: the data subject's rows (KVKK erasure).
func (s gormStore) DeleteWebPushSubscriptionsOf(ctx context.Context, userID uuid.UUID) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM web_push_subscriptions WHERE user_id = ?`, userID)
	if res.Error != nil {
		return 0, fmt.Errorf("notify: delete web push subscriptions: %w", res.Error)
	}
	return int(res.RowsAffected), nil
}
