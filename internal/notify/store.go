package notify

import (
	"context"
	"time"
	"uuid"
)

// Store is the persistence boundary of notify (05 §2.2) over the five notify
// tables of 02 §6. Org scoping does not apply: every row belongs to a
// recipient, not to an organisation. The GORM store (store_gorm.go) works in
// the transaction carried by ctx (db.Conn); the in-memory store
// (store_mem.go) backs service and wiring tests. Both pass the same contract
// (store_contract_test.go, runStoreContract; 05 §13.3).
type Store interface {
	DeliveryStore
	SuppressionStore
	PreferenceStore
	InAppStore
	PushTokenStore
}

// DeliveryStore persists notification_deliveries (02 §6.1).
type DeliveryStore interface {
	// Claim registers d as the delivery of d.DedupeKey, or finds the existing
	// one, and tells the caller what to do. It is atomic: of any number of
	// concurrent Claims of one key, at most one gets ClaimAcquired.
	//
	//   - no delivery for the key: d is stored as queued with attempts = 1
	//     (CreatedAt = now) → ClaimAcquired;
	//   - the delivery is failed, or queued and not touched for at least
	//     lease (a crashed attempt): it is re-opened as queued, attempts + 1,
	//     TemplateVersion = d.TemplateVersion → ClaimAcquired;
	//   - the delivery is queued and younger than lease: another attempt is
	//     running → ClaimInFlight;
	//   - any other status is final → ClaimDone.
	//
	// UpdatedAt is the store's own clock: the database's now() in the GORM
	// store (the touch trigger of 02 [ts] sets it on every update), now in
	// the in-memory store; the lease is measured against it. The returned
	// Delivery is the stored one (d.ID is used only for a new row).
	Claim(ctx context.Context, d Delivery, now time.Time, lease time.Duration) (Delivery, ClaimState, error)

	// Finish records the outcome of the attempt that holds the delivery. It
	// overwrites status, provider, provider message ID, last error and sent_at
	// with r (zero values clear them) and touches UpdatedAt. It is a no-op
	// unless the delivery is queued: a final delivery is never overwritten by a
	// late attempt. ErrNotFound when id does not exist.
	Finish(ctx context.Context, id uuid.UUID, r Result, now time.Time) (Delivery, error)

	// Get returns the delivery or ErrNotFound.
	Get(ctx context.Context, id uuid.UUID) (Delivery, error)

	// ListOfUser returns up to limit deliveries of userID in (created_at
	// DESC, id DESC) order, strictly after the position after (nil: first
	// page).
	ListOfUser(ctx context.Context, userID uuid.UUID, after *Position, limit int) ([]Delivery, error)

	// MarkProviderOutcome applies a provider webhook outcome to the e-mail
	// delivery whose provider message ID is providerMessageID: delivered
	// (sets DeliveredAt) only from sent; bounced and complained from sent or
	// delivered. found=false when no e-mail delivery has the ID; a delivery
	// in another status is left unchanged (found=true, changed=false).
	MarkProviderOutcome(ctx context.Context, providerMessageID string, status Status, at time.Time) (found, changed bool, err error)

	// DeleteDeliveriesOf deletes every delivery of userID (KVKK erasure).
	DeleteDeliveriesOf(ctx context.Context, userID uuid.UUID) (int, error)
}

// Position is a keyset position in (created_at DESC, id DESC) order: the
// next page holds rows strictly older than (At, ID).
type Position struct {
	At time.Time
	ID uuid.UUID
}

// SuppressionStore persists email_suppressions (02 §6.2).
type SuppressionStore interface {
	// Suppress stores s unless the index is already suppressed (the first
	// reason wins). added reports a new row.
	Suppress(ctx context.Context, s Suppression) (added bool, err error)
	// IsSuppressed reports whether the index is suppressed.
	IsSuppressed(ctx context.Context, emailIndex []byte) (bool, error)
	// GetSuppression returns the suppression or ErrSuppressionNotFound.
	GetSuppression(ctx context.Context, emailIndex []byte) (Suppression, error)
	// ListSuppressions returns up to limit suppressions in (created_at DESC,
	// email_bidx DESC) order after the position (nil: first page).
	ListSuppressions(ctx context.Context, after *SuppressionPosition, limit int) ([]Suppression, error)
	// DeleteSuppression removes the suppression; ErrSuppressionNotFound when
	// there is none.
	DeleteSuppression(ctx context.Context, emailIndex []byte) error
}

// SuppressionPosition is a keyset position of ListSuppressions.
type SuppressionPosition struct {
	At    time.Time
	Index []byte
}

// PreferenceStore persists notification_preferences (02 §6.5).
type PreferenceStore interface {
	// Preferences returns the stored rows of userID (missing pairs are at
	// their default).
	Preferences(ctx context.Context, userID uuid.UUID) ([]Preference, error)
	// Preference returns one stored row; ok=false when the pair has none.
	Preference(ctx context.Context, userID uuid.UUID, c PrefCategory, ch Channel) (enabled, ok bool, err error)
	// UpsertPreferences writes the given rows of userID (insert or update).
	// A disabled security row violates the table CHECK: ErrSecurityLocked.
	UpsertPreferences(ctx context.Context, userID uuid.UUID, prefs []Preference, now time.Time) error
	// DeletePreferencesOf deletes every row of userID (KVKK erasure).
	DeletePreferencesOf(ctx context.Context, userID uuid.UUID) (int, error)
}

// InAppStore persists in_app_notifications (02 §6.4).
type InAppStore interface {
	// InsertInApp stores n unless a row with n.ID exists (a retried send of
	// the same delivery); inserted reports a new row.
	InsertInApp(ctx context.Context, n InAppNotification) (inserted bool, err error)
	// ListInApp returns up to limit notifications of userID in (created_at
	// DESC, id DESC) order after the position, only unread ones when
	// unreadOnly.
	ListInApp(ctx context.Context, userID uuid.UUID, unreadOnly bool, after *Position, limit int) ([]InAppNotification, error)
	// MarkInAppRead sets read_at = at on the notification id of userID
	// unless it is read already. ErrNotificationNotFound when userID has no
	// such notification (another user's is not found either).
	MarkInAppRead(ctx context.Context, userID, id uuid.UUID, at time.Time) error
	// MarkAllInAppRead marks every unread notification of userID read and
	// returns how many changed.
	MarkAllInAppRead(ctx context.Context, userID uuid.UUID, at time.Time) (int, error)
	// DeleteInAppOf deletes every notification of userID (KVKK erasure).
	DeleteInAppOf(ctx context.Context, userID uuid.UUID) (int, error)
}

// PushTokenStore persists push_tokens (02 §6.3).
type PushTokenStore interface {
	// UpsertPushToken stores t, or, when a row with t.TokenSHA256 exists
	// (the device registers again, possibly for another account), moves it
	// to t's user and session, refreshes platform, app version and
	// last_seen_at and clears revoked_at. It returns the stored row.
	UpsertPushToken(ctx context.Context, t PushToken) (PushToken, error)
	// DeletePushToken deletes the token id of userID; ErrPushTokenNotFound
	// when userID has no such token.
	DeletePushToken(ctx context.Context, userID, id uuid.UUID) error
	// ActivePushTokens returns the unrevoked tokens of userID (the push
	// adapter of BE-055).
	ActivePushTokens(ctx context.Context, userID uuid.UUID) ([]PushToken, error)
	// PushTokensOf returns every token of userID (KVKK export).
	PushTokensOf(ctx context.Context, userID uuid.UUID) ([]PushToken, error)
	// DeletePushTokensOf deletes every token of userID (KVKK erasure).
	DeletePushTokensOf(ctx context.Context, userID uuid.UUID) (int, error)
}

// ClaimState is the outcome of Store.Claim.
type ClaimState int

// Claim outcomes.
const (
	// ClaimAcquired: the caller owns the delivery and must attempt the send,
	// then call Finish.
	ClaimAcquired ClaimState = iota + 1
	// ClaimDone: the delivery is final; the caller returns its ID.
	ClaimDone
	// ClaimInFlight: another attempt is running; the caller must not send.
	ClaimInFlight
)

// Result is the outcome of one send attempt (Store.Finish).
type Result struct {
	Status            Status
	Provider          string
	ProviderMessageID string
	LastError         string // already scrubbed of personal data
	SentAt            *time.Time
}
