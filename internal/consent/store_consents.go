package consent

import (
	"context"
	"uuid"
)

// consentStore is consents, the append-only ledger of grants and
// withdrawals, and its consent_current view (02 §4).
type consentStore interface {
	// InsertConsent appends a consent row.
	InsertConsent(ctx context.Context, c Consent) error
	// Latest returns the newest row of (user, purpose, scope), ordered by
	// occurred_at then id; found=false when there is none. scopeID nil
	// means the unscoped row.
	Latest(ctx context.Context, userID uuid.UUID, purpose Purpose, scopeID *uuid.UUID) (c Consent, found bool, err error)
	// LockSubject serialises the unscoped chain of (user, purpose) for the
	// rest of the transaction carried by ctx (a transaction-scoped advisory
	// lock): concurrent one-click unsubscribes read the latest row one
	// after the other and withdraw once (BE-062). Outside a transaction it
	// guards nothing.
	LockSubject(ctx context.Context, userID uuid.UUID, purpose Purpose) error
	// CurrentOf returns the newest row of every (purpose, scope) of a user
	// (the consent_current view), ordered by purpose then scope.
	CurrentOf(ctx context.Context, userID uuid.UUID) ([]Consent, error)
	// GrantedAmong returns those of userIDs whose newest unscoped row of
	// purpose is a grant, in ascending ID order.
	GrantedAmong(ctx context.Context, purpose Purpose, userIDs []uuid.UUID) ([]uuid.UUID, error)
	// GrantedChains returns up to limit newest unscoped rows of purpose that
	// are grants (one per user: the consent in force), with a user ID
	// greater than after, in ascending user ID order
	// (consent.iys_status_sync, BE-103).
	GrantedChains(ctx context.Context, purpose Purpose, after uuid.UUID, limit int) ([]Consent, error)
	// ListConsents returns up to q.Limit rows of a user, newest first
	// (occurred_at DESC, id DESC), after q.After.
	ListConsents(ctx context.Context, q ListQuery) ([]Consent, error)
	// ConsentByID returns one consent row or ErrNotFound.
	ConsentByID(ctx context.Context, id uuid.UUID) (Consent, error)
	// AllOf returns every row of a user, oldest first (KVKK export).
	AllOf(ctx context.Context, userID uuid.UUID) ([]Consent, error)
}
