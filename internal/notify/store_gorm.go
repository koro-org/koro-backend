package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"uuid"

	"gorm.io/gorm"

	"koro/internal/platform/db"
)

// NewGormStore returns the PostgreSQL store over the notify tables
// (02 §6): notification_deliveries, email_suppressions,
// notification_preferences, in_app_notifications and push_tokens. None of
// them has an org: every row belongs to a recipient (cross-org by design).
func NewGormStore(orm *gorm.DB) Store { return gormStore{orm: orm} }

type gormStore struct{ orm *gorm.DB }

var prefConstraints = db.Constraints{"notification_preferences_security_chk": ErrSecurityLocked}

// ---------------------------------------------------------------- deliveries

const deliveryCols = `id, user_id, recipient_bidx, channel, category, template, template_version, dedupe_key,
	provider, provider_message_id, status, attempts, last_error, sent_at, delivered_at, created_at, updated_at`

type deliveryRow struct {
	ID                uuid.UUID  `gorm:"column:id"`
	UserID            *uuid.UUID `gorm:"column:user_id"`
	RecipientBidx     []byte     `gorm:"column:recipient_bidx"`
	Channel           string     `gorm:"column:channel"`
	Category          string     `gorm:"column:category"`
	Template          string     `gorm:"column:template"`
	TemplateVersion   int        `gorm:"column:template_version"`
	DedupeKey         string     `gorm:"column:dedupe_key"`
	Provider          *string    `gorm:"column:provider"`
	ProviderMessageID *string    `gorm:"column:provider_message_id"`
	Status            string     `gorm:"column:status"`
	Attempts          int        `gorm:"column:attempts"`
	LastError         *string    `gorm:"column:last_error"`
	SentAt            *time.Time `gorm:"column:sent_at"`
	DeliveredAt       *time.Time `gorm:"column:delivered_at"`
	CreatedAt         time.Time  `gorm:"column:created_at;->"`
	UpdatedAt         time.Time  `gorm:"column:updated_at;->"`
}

// cross-org: row conversion / helper; no query of its own beyond its callers.
func (r deliveryRow) delivery() Delivery {
	return Delivery{
		ID: r.ID, UserID: r.UserID, RecipientIndex: r.RecipientBidx, Channel: Channel(r.Channel),
		Category: Category(r.Category), Template: r.Template, TemplateVersion: r.TemplateVersion,
		DedupeKey: r.DedupeKey, Provider: deref(r.Provider), ProviderMessageID: deref(r.ProviderMessageID),
		Status: Status(r.Status), Attempts: r.Attempts, LastError: deref(r.LastError),
		SentAt: db.UTCPtr(r.SentAt), DeliveredAt: db.UTCPtr(r.DeliveredAt),
		CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC(),
	}
}

// Claim implements Store: insert-or-nothing, then the conditional takeover,
// then a read. Each step is one atomic statement, so concurrent claims of a
// key never both acquire it (the takeover re-checks its WHERE clause after
// a competing update commits). The lease is measured in database time,
// and created_at and updated_at are database time too: the column defaults
// stamp a new row (one clock, 02 §0.1 [ts], BE-069).
//
// cross-org: deliveries belong to a recipient, not to an org (02 §6.1).
func (s gormStore) Claim(ctx context.Context, d Delivery, _ time.Time, lease time.Duration) (Delivery, ClaimState, error) {
	c := db.Conn(ctx, s.orm)
	var rows []deliveryRow
	err := c.Raw(`INSERT INTO notification_deliveries
		(id, user_id, recipient_bidx, channel, category, template, template_version, dedupe_key, status, attempts)
		VALUES ( ?, ?, ?, ?, ?, ?, ?, ?, 'queued', 1 )
		ON CONFLICT (dedupe_key) DO NOTHING RETURNING `+deliveryCols,
		d.ID, d.UserID, nilIfEmpty(d.RecipientIndex), string(d.Channel), string(d.Category), d.Template,
		d.TemplateVersion, d.DedupeKey).Scan(&rows).Error
	if err != nil {
		return Delivery{}, 0, fmt.Errorf("notify: claim insert: %w", db.Translate(err))
	}
	if len(rows) == 1 {
		return rows[0].delivery(), ClaimAcquired, nil
	}
	err = c.Raw(`UPDATE notification_deliveries SET status = 'queued', attempts = attempts + 1, template_version = ?
		WHERE dedupe_key = ? AND (status = 'failed'
			OR (status = 'queued' AND updated_at <= now() - ( ? * interval '1 microsecond')))
		RETURNING `+deliveryCols, d.TemplateVersion, d.DedupeKey, lease.Microseconds()).Scan(&rows).Error
	if err != nil {
		return Delivery{}, 0, fmt.Errorf("notify: claim takeover: %w", db.Translate(err))
	}
	if len(rows) == 1 {
		return rows[0].delivery(), ClaimAcquired, nil
	}
	var cur deliveryRow
	if err := c.Raw(`SELECT `+deliveryCols+` FROM notification_deliveries WHERE dedupe_key = ?`, d.DedupeKey).
		Take(&cur).Error; err != nil {
		return Delivery{}, 0, fmt.Errorf("notify: claim read: %w", err)
	}
	if Status(cur.Status) == StatusQueued {
		return cur.delivery(), ClaimInFlight, nil
	}
	return cur.delivery(), ClaimDone, nil
}

// Finish implements Store.
//
// cross-org: deliveries belong to a recipient (02 §6.1).
func (s gormStore) Finish(ctx context.Context, id uuid.UUID, r Result, _ time.Time) (Delivery, error) {
	var rows []deliveryRow
	err := db.Conn(ctx, s.orm).Raw(`UPDATE notification_deliveries
		SET status = ?, provider = ?, provider_message_id = ?, last_error = ?, sent_at = ?
		WHERE id = ? AND status = 'queued' RETURNING `+deliveryCols,
		string(r.Status), nilIfBlank(r.Provider), nilIfBlank(r.ProviderMessageID), nilIfBlank(r.LastError), r.SentAt, id).
		Scan(&rows).Error
	if err != nil {
		return Delivery{}, fmt.Errorf("notify: finish: %w", db.Translate(err))
	}
	if len(rows) == 1 {
		return rows[0].delivery(), nil
	}
	return s.Get(ctx, id)
}

// Get implements Store.
//
// cross-org: deliveries belong to a recipient (02 §6.1).
func (s gormStore) Get(ctx context.Context, id uuid.UUID) (Delivery, error) {
	var r deliveryRow
	err := db.Conn(ctx, s.orm).Raw(`SELECT `+deliveryCols+` FROM notification_deliveries WHERE id = ?`, id).Take(&r).Error
	if db.IsNotFound(err) {
		return Delivery{}, ErrNotFound
	}
	if err != nil {
		return Delivery{}, fmt.Errorf("notify: get delivery: %w", err)
	}
	return r.delivery(), nil
}

// ListOfUser implements Store.
//
// cross-org: the recipient's own deliveries (02 §6.1).
func (s gormStore) ListOfUser(ctx context.Context, userID uuid.UUID, after *Position, limit int) ([]Delivery, error) {
	q := `SELECT ` + deliveryCols + ` FROM notification_deliveries WHERE user_id = ?`
	args := []any{userID}
	if after != nil {
		q += ` AND (created_at, id) < ( ?, ? )`
		args = append(args, after.At, after.ID)
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	var rows []deliveryRow
	if err := db.Conn(ctx, s.orm).Raw(q, append(args, limit)...).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("notify: list deliveries: %w", err)
	}
	out := make([]Delivery, len(rows))
	for i, r := range rows {
		out[i] = r.delivery()
	}
	return out, nil
}

// MarkProviderOutcome implements Store.
//
// cross-org: addressed by the provider message ID (02 §6.1).
func (s gormStore) MarkProviderOutcome(ctx context.Context, providerMessageID string, status Status, at time.Time) (bool, bool, error) {
	c := db.Conn(ctx, s.orm)
	var cur []struct {
		ID     uuid.UUID `gorm:"column:id"`
		Status string    `gorm:"column:status"`
	}
	err := c.Raw(`SELECT id, status FROM notification_deliveries
		WHERE channel = 'email' AND provider_message_id = ? ORDER BY created_at LIMIT 1 FOR UPDATE`, providerMessageID).
		Scan(&cur).Error
	if err != nil {
		return false, false, fmt.Errorf("notify: provider outcome: %w", err)
	}
	if len(cur) == 0 {
		return false, false, nil
	}
	if !outcomeAllowed(Status(cur[0].Status), status) {
		return true, false, nil
	}
	upd := map[string]any{"status": string(status)}
	if status == StatusDelivered {
		upd["delivered_at"] = at
	}
	if err := c.Table("notification_deliveries").Where("id = ?", cur[0].ID).Updates(upd).Error; err != nil {
		return true, false, fmt.Errorf("notify: provider outcome: %w", db.Translate(err))
	}
	return true, true, nil
}

// DeleteDeliveriesOf implements Store.
//
// cross-org: the data subject's rows (KVKK erasure).
func (s gormStore) DeleteDeliveriesOf(ctx context.Context, userID uuid.UUID) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM notification_deliveries WHERE user_id = ?`, userID)
	if res.Error != nil {
		return 0, fmt.Errorf("notify: delete deliveries: %w", res.Error)
	}
	return int(res.RowsAffected), nil
}

// ---------------------------------------------------------------- suppressions

type suppressionRow struct {
	EmailBidx []byte    `gorm:"column:email_bidx"`
	Reason    string    `gorm:"column:reason"`
	Source    string    `gorm:"column:source"`
	CreatedAt time.Time `gorm:"column:created_at"`
}

// cross-org: row conversion / helper; no query of its own beyond its callers.
func (r suppressionRow) suppression() Suppression {
	return Suppression{EmailIndex: r.EmailBidx, Reason: SuppressionReason(r.Reason), Source: SuppressionSource(r.Source),
		CreatedAt: r.CreatedAt.UTC()}
}

// Suppress implements Store.
//
// cross-org: the suppression list is global (02 §6.2).
func (s gormStore) Suppress(ctx context.Context, sup Suppression) (bool, error) {
	res := db.Conn(ctx, s.orm).Exec(`INSERT INTO email_suppressions (email_bidx, reason, source, created_at)
		VALUES ( ?, ?, ?, ? ) ON CONFLICT (email_bidx) DO NOTHING`,
		sup.EmailIndex, string(sup.Reason), string(sup.Source), sup.CreatedAt)
	if res.Error != nil {
		return false, fmt.Errorf("notify: suppress: %w", db.Translate(res.Error))
	}
	return res.RowsAffected == 1, nil
}

// IsSuppressed implements Store.
//
// cross-org: the suppression list is global (02 §6.2).
func (s gormStore) IsSuppressed(ctx context.Context, idx []byte) (bool, error) {
	if len(idx) == 0 {
		return false, nil
	}
	var found bool
	err := db.Conn(ctx, s.orm).Raw(`SELECT EXISTS (SELECT 1 FROM email_suppressions WHERE email_bidx = ?)`, idx).
		Scan(&found).Error
	if err != nil {
		return false, fmt.Errorf("notify: is suppressed: %w", err)
	}
	return found, nil
}

// GetSuppression implements Store.
//
// cross-org: the suppression list is global (02 §6.2).
func (s gormStore) GetSuppression(ctx context.Context, idx []byte) (Suppression, error) {
	var r suppressionRow
	err := db.Conn(ctx, s.orm).Raw(`SELECT email_bidx, reason, source, created_at FROM email_suppressions WHERE email_bidx = ?`, idx).
		Take(&r).Error
	if db.IsNotFound(err) {
		return Suppression{}, ErrSuppressionNotFound
	}
	if err != nil {
		return Suppression{}, fmt.Errorf("notify: get suppression: %w", err)
	}
	return r.suppression(), nil
}

// ListSuppressions implements Store.
//
// cross-org: the suppression list is global (02 §6.2).
func (s gormStore) ListSuppressions(ctx context.Context, after *SuppressionPosition, limit int) ([]Suppression, error) {
	q := `SELECT email_bidx, reason, source, created_at FROM email_suppressions`
	var args []any
	if after != nil {
		q += ` WHERE (created_at, email_bidx) < ( ?, ? )`
		args = append(args, after.At, after.Index)
	}
	q += ` ORDER BY created_at DESC, email_bidx DESC LIMIT ?`
	var rows []suppressionRow
	if err := db.Conn(ctx, s.orm).Raw(q, append(args, limit)...).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("notify: list suppressions: %w", err)
	}
	out := make([]Suppression, len(rows))
	for i, r := range rows {
		out[i] = r.suppression()
	}
	return out, nil
}

// DeleteSuppression implements Store.
//
// cross-org: the suppression list is global (02 §6.2).
func (s gormStore) DeleteSuppression(ctx context.Context, idx []byte) error {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM email_suppressions WHERE email_bidx = ?`, idx)
	if res.Error != nil {
		return fmt.Errorf("notify: delete suppression: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrSuppressionNotFound
	}
	return nil
}

// ---------------------------------------------------------------- preferences

type prefRow struct {
	Category string `gorm:"column:category"`
	Channel  string `gorm:"column:channel"`
	Enabled  bool   `gorm:"column:enabled"`
}

// Preferences implements Store.
//
// cross-org: the user's own preferences (02 §6.5).
func (s gormStore) Preferences(ctx context.Context, userID uuid.UUID) ([]Preference, error) {
	var rows []prefRow
	err := db.Conn(ctx, s.orm).Raw(`SELECT category, channel, enabled FROM notification_preferences
		WHERE user_id = ? ORDER BY category, channel`, userID).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("notify: preferences: %w", err)
	}
	out := make([]Preference, len(rows))
	for i, r := range rows {
		out[i] = Preference{Category: PrefCategory(r.Category), Channel: Channel(r.Channel), Enabled: r.Enabled}
	}
	return out, nil
}

// Preference implements Store.
//
// cross-org: the user's own preferences (02 §6.5).
func (s gormStore) Preference(ctx context.Context, userID uuid.UUID, c PrefCategory, ch Channel) (bool, bool, error) {
	var rows []prefRow
	err := db.Conn(ctx, s.orm).Raw(`SELECT category, channel, enabled FROM notification_preferences
		WHERE user_id = ? AND category = ? AND channel = ?`, userID, string(c), string(ch)).Scan(&rows).Error
	if err != nil {
		return false, false, fmt.Errorf("notify: preference: %w", err)
	}
	if len(rows) == 0 {
		return false, false, nil
	}
	return rows[0].Enabled, true, nil
}

// UpsertPreferences implements Store. created_at and updated_at are the
// database's: the column defaults on insert, the touch trigger on update
// (one clock, 02 §0.1 [ts], BE-069).
//
// cross-org: the user's own preferences (02 §6.5).
func (s gormStore) UpsertPreferences(ctx context.Context, userID uuid.UUID, prefs []Preference, _ time.Time) error {
	c := db.Conn(ctx, s.orm)
	for _, p := range prefs {
		err := c.Exec(`INSERT INTO notification_preferences (user_id, category, channel, enabled)
			VALUES ( ?, ?, ?, ? )
			ON CONFLICT (user_id, category, channel) DO UPDATE SET enabled = EXCLUDED.enabled`,
			userID, string(p.Category), string(p.Channel), p.Enabled).Error
		if err != nil {
			return fmt.Errorf("notify: upsert preference: %w", prefConstraints.Translate(err))
		}
	}
	return nil
}

// DeletePreferencesOf implements Store.
//
// cross-org: the data subject's rows (KVKK erasure).
func (s gormStore) DeletePreferencesOf(ctx context.Context, userID uuid.UUID) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM notification_preferences WHERE user_id = ?`, userID)
	if res.Error != nil {
		return 0, fmt.Errorf("notify: delete preferences: %w", res.Error)
	}
	return int(res.RowsAffected), nil
}

// ---------------------------------------------------------------- in-app

const inAppCols = `id, user_id, kind, title, body, link_path, source_org_id, data, read_at, created_at`

type inAppRow struct {
	ID          uuid.UUID  `gorm:"column:id"`
	UserID      uuid.UUID  `gorm:"column:user_id"`
	Kind        string     `gorm:"column:kind"`
	Title       string     `gorm:"column:title"`
	Body        *string    `gorm:"column:body"`
	LinkPath    *string    `gorm:"column:link_path"`
	SourceOrgID *uuid.UUID `gorm:"column:source_org_id"`
	Data        string     `gorm:"column:data"`
	ReadAt      *time.Time `gorm:"column:read_at"`
	CreatedAt   time.Time  `gorm:"column:created_at"`
}

// cross-org: row conversion / helper; no query of its own beyond its callers.
func (r inAppRow) notification() (InAppNotification, error) {
	n := InAppNotification{ID: r.ID, UserID: r.UserID, Kind: r.Kind, Title: r.Title, Body: deref(r.Body),
		LinkPath: deref(r.LinkPath), SourceOrgID: r.SourceOrgID, ReadAt: db.UTCPtr(r.ReadAt), CreatedAt: r.CreatedAt.UTC(),
		Data: map[string]string{}}
	if err := json.Unmarshal([]byte(r.Data), &n.Data); err != nil {
		return InAppNotification{}, fmt.Errorf("notify: in-app data: %w", err)
	}
	return n, nil
}

// InsertInApp implements Store.
//
// cross-org: the recipient's inbox (02 §6.4); source_org_id is display only.
func (s gormStore) InsertInApp(ctx context.Context, n InAppNotification) (bool, error) {
	data := n.Data
	if data == nil {
		data = map[string]string{}
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return false, err
	}
	res := db.Conn(ctx, s.orm).Exec(`INSERT INTO in_app_notifications
		(id, user_id, kind, title, body, link_path, source_org_id, data, created_at)
		VALUES ( ?, ?, ?, ?, ?, ?, ?, CAST( ? AS jsonb), ? ) ON CONFLICT (id) DO NOTHING`,
		n.ID, n.UserID, n.Kind, n.Title, nilIfBlank(n.Body), nilIfBlank(n.LinkPath), n.SourceOrgID, string(raw), n.CreatedAt)
	if res.Error != nil {
		return false, fmt.Errorf("notify: insert in-app: %w", db.Translate(res.Error))
	}
	return res.RowsAffected == 1, nil
}

// ListInApp implements Store.
//
// cross-org: the recipient's inbox (02 §6.4).
func (s gormStore) ListInApp(ctx context.Context, userID uuid.UUID, unreadOnly bool, after *Position, limit int) ([]InAppNotification, error) {
	q := `SELECT ` + inAppCols + ` FROM in_app_notifications WHERE user_id = ?`
	args := []any{userID}
	if unreadOnly {
		q += ` AND read_at IS NULL`
	}
	if after != nil {
		q += ` AND (created_at, id) < ( ?, ? )`
		args = append(args, after.At, after.ID)
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	var rows []inAppRow
	if err := db.Conn(ctx, s.orm).Raw(q, append(args, limit)...).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("notify: list in-app: %w", err)
	}
	out := make([]InAppNotification, 0, len(rows))
	for _, r := range rows {
		n, err := r.notification()
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

// MarkInAppRead implements Store.
//
// cross-org: the recipient's inbox (02 §6.4).
func (s gormStore) MarkInAppRead(ctx context.Context, userID, id uuid.UUID, at time.Time) error {
	res := db.Conn(ctx, s.orm).Exec(`UPDATE in_app_notifications SET read_at = COALESCE(read_at, ?)
		WHERE id = ? AND user_id = ?`, at, id, userID)
	if res.Error != nil {
		return fmt.Errorf("notify: mark read: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrNotificationNotFound
	}
	return nil
}

// MarkAllInAppRead implements Store.
//
// cross-org: the recipient's inbox (02 §6.4).
func (s gormStore) MarkAllInAppRead(ctx context.Context, userID uuid.UUID, at time.Time) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`UPDATE in_app_notifications SET read_at = ? WHERE user_id = ? AND read_at IS NULL`, at, userID)
	if res.Error != nil {
		return 0, fmt.Errorf("notify: mark all read: %w", res.Error)
	}
	return int(res.RowsAffected), nil
}

// DeleteInAppOf implements Store.
//
// cross-org: the data subject's rows (KVKK erasure).
func (s gormStore) DeleteInAppOf(ctx context.Context, userID uuid.UUID) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM in_app_notifications WHERE user_id = ?`, userID)
	if res.Error != nil {
		return 0, fmt.Errorf("notify: delete in-app: %w", res.Error)
	}
	return int(res.RowsAffected), nil
}

// ---------------------------------------------------------------- push tokens

const pushCols = `id, user_id, session_id, platform, token, token_sha256, app_version, created_at, last_seen_at, revoked_at`

type pushRow struct {
	ID          uuid.UUID  `gorm:"column:id"`
	UserID      uuid.UUID  `gorm:"column:user_id"`
	SessionID   *uuid.UUID `gorm:"column:session_id"`
	Platform    string     `gorm:"column:platform"`
	Token       string     `gorm:"column:token"`
	TokenSHA256 []byte     `gorm:"column:token_sha256"`
	AppVersion  *string    `gorm:"column:app_version"`
	CreatedAt   time.Time  `gorm:"column:created_at"`
	LastSeenAt  time.Time  `gorm:"column:last_seen_at"`
	RevokedAt   *time.Time `gorm:"column:revoked_at"`
}

// cross-org: row conversion / helper; no query of its own beyond its callers.
func (r pushRow) token() PushToken {
	return PushToken{ID: r.ID, UserID: r.UserID, SessionID: r.SessionID, Platform: PushPlatform(r.Platform), Token: r.Token,
		TokenSHA256: r.TokenSHA256, AppVersion: deref(r.AppVersion), CreatedAt: r.CreatedAt.UTC(),
		LastSeenAt: r.LastSeenAt.UTC(), RevokedAt: db.UTCPtr(r.RevokedAt)}
}

// UpsertPushToken implements Store.
//
// cross-org: the user's own devices (02 §6.3).
func (s gormStore) UpsertPushToken(ctx context.Context, t PushToken) (PushToken, error) {
	var rows []pushRow
	err := db.Conn(ctx, s.orm).Raw(`INSERT INTO push_tokens
		(id, user_id, session_id, platform, token, token_sha256, app_version, created_at, last_seen_at)
		VALUES ( ?, ?, ?, ?, ?, ?, ?, ?, ? )
		ON CONFLICT (token_sha256) DO UPDATE SET user_id = EXCLUDED.user_id, session_id = EXCLUDED.session_id,
			platform = EXCLUDED.platform, token = EXCLUDED.token, app_version = EXCLUDED.app_version,
			last_seen_at = EXCLUDED.last_seen_at, revoked_at = NULL
		RETURNING `+pushCols,
		t.ID, t.UserID, t.SessionID, string(t.Platform), t.Token, t.TokenSHA256, nilIfBlank(t.AppVersion), t.CreatedAt, t.LastSeenAt).
		Scan(&rows).Error
	if err != nil {
		return PushToken{}, fmt.Errorf("notify: upsert push token: %w", db.Translate(err))
	}
	if len(rows) != 1 {
		return PushToken{}, errors.New("notify: upsert push token returned no row")
	}
	return rows[0].token(), nil
}

// DeletePushToken implements Store.
//
// cross-org: the user's own devices (02 §6.3).
func (s gormStore) DeletePushToken(ctx context.Context, userID, id uuid.UUID) error {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM push_tokens WHERE id = ? AND user_id = ?`, id, userID)
	if res.Error != nil {
		return fmt.Errorf("notify: delete push token: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrPushTokenNotFound
	}
	return nil
}

// ActivePushTokens implements Store.
//
// cross-org: the user's own devices (02 §6.3).
func (s gormStore) ActivePushTokens(ctx context.Context, userID uuid.UUID) ([]PushToken, error) {
	return s.tokens(ctx, `user_id = ? AND revoked_at IS NULL`, userID)
}

// RevokePushToken implements Store.
//
// cross-org: a device token reported invalid by the provider (02 §6.3).
func (s gormStore) RevokePushToken(ctx context.Context, id uuid.UUID, at time.Time) (bool, error) {
	res := db.Conn(ctx, s.orm).Exec(`UPDATE push_tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, at, id)
	if res.Error != nil {
		return false, fmt.Errorf("notify: revoke push token: %w", res.Error)
	}
	return res.RowsAffected == 1, nil
}

// PushTokensOf implements Store.
//
// cross-org: the user's own devices (02 §6.3).
func (s gormStore) PushTokensOf(ctx context.Context, userID uuid.UUID) ([]PushToken, error) {
	return s.tokens(ctx, `user_id = ?`, userID)
}

// cross-org: row conversion / helper; no query of its own beyond its callers.
func (s gormStore) tokens(ctx context.Context, where string, args ...any) ([]PushToken, error) {
	var rows []pushRow
	if err := db.Conn(ctx, s.orm).Raw(`SELECT `+pushCols+` FROM push_tokens WHERE `+where+` ORDER BY id`, args...).
		Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("notify: push tokens: %w", err)
	}
	out := make([]PushToken, len(rows))
	for i, r := range rows {
		out[i] = r.token()
	}
	return out, nil
}

// DeletePushTokensOf implements Store.
//
// cross-org: the data subject's rows (KVKK erasure).
func (s gormStore) DeletePushTokensOf(ctx context.Context, userID uuid.UUID) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM push_tokens WHERE user_id = ?`, userID)
	if res.Error != nil {
		return 0, fmt.Errorf("notify: delete push tokens: %w", res.Error)
	}
	return int(res.RowsAffected), nil
}

// ---------------------------------------------------------------- helpers

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func nilIfBlank(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nilIfEmpty(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}
