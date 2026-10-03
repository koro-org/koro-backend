package notify

import (
	"context"
	"time"
	"uuid"
)

// Store is the persistence boundary of notify (05 §2.2). Org scoping does not
// apply: deliveries belong to a recipient, not to an organisation. The
// in-memory store (store_mem.go) serves BE-005; BE-015 adds the GORM store
// over notification_deliveries and both must pass the same contract test
// (store_contract_test.go, 05 §13.3).
type Store interface {
	// Claim registers d as the delivery of d.DedupeKey, or finds the existing
	// one, and tells the caller what to do. It is atomic: of any number of
	// concurrent Claims of one key, at most one gets ClaimAcquired.
	//
	//   - no delivery for the key: d is stored as queued with attempts = 1
	//     (CreatedAt = UpdatedAt = now) → ClaimAcquired;
	//   - the delivery is failed, or queued with UpdatedAt older than lease
	//     (a crashed attempt): it is re-opened as queued, attempts + 1,
	//     UpdatedAt = now, TemplateVersion = d.TemplateVersion → ClaimAcquired;
	//   - the delivery is queued and younger than lease: another attempt is
	//     running → ClaimInFlight;
	//   - any other status is final → ClaimDone.
	//
	// The returned Delivery is the stored one (d.ID is used only for a new row).
	Claim(ctx context.Context, d Delivery, now time.Time, lease time.Duration) (Delivery, ClaimState, error)

	// Finish records the outcome of the attempt that holds the delivery. It
	// overwrites status, provider, provider message ID, last error and sent_at
	// with r (zero values clear them) and sets UpdatedAt = now. It is a no-op
	// unless the delivery is queued: a final delivery is never overwritten by a
	// late attempt. ErrNotFound when id does not exist.
	Finish(ctx context.Context, id uuid.UUID, r Result, now time.Time) (Delivery, error)

	// Get returns the delivery or ErrNotFound.
	Get(ctx context.Context, id uuid.UUID) (Delivery, error)
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

// MissingStore fails every call with ErrStoreNotWired. The composition root
// installs it while no real store exists, so a missing store surfaces as a
// loud error on the first send instead of an in-memory fallback (05 §12.2).
type MissingStore struct{}

// Claim implements Store.
func (MissingStore) Claim(context.Context, Delivery, time.Time, time.Duration) (Delivery, ClaimState, error) {
	return Delivery{}, 0, ErrStoreNotWired
}

// Finish implements Store.
func (MissingStore) Finish(context.Context, uuid.UUID, Result, time.Time) (Delivery, error) {
	return Delivery{}, ErrStoreNotWired
}

// Get implements Store.
func (MissingStore) Get(context.Context, uuid.UUID) (Delivery, error) {
	return Delivery{}, ErrStoreNotWired
}
