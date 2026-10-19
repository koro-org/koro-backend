package entitlements

import (
	"context"
	"time"
	"uuid"
)

// Store is the persistence boundary of entitlements (05 §2.2). The GORM
// store (store_gorm.go) and the in-memory store (store_mem.go) pass the same
// contract test (store_contract_test.go).
//
// Every method works in the transaction carried by ctx, if any; the
// service draws the boundaries (a consumption joins the caller's
// transaction, 01 §6.3).
//
// Org scope (06 H-38): usage rows take org_id and entitlement_id from the
// entitlement row they consume or reverse, never from a request.
type Store interface {
	// Insert writes a new entitlement. The store stamps created_at and
	// updated_at (one clock, BE-069): e's are ignored.
	Insert(ctx context.Context, e Entitlement) error
	// Get returns the entitlement of orgID, or ErrNotFound (another org's
	// entitlement is not found either).
	Get(ctx context.Context, orgID, id uuid.UUID) (Entitlement, error)
	// Lock returns the entitlement locked for the rest of the transaction
	// (SELECT … FOR UPDATE), or ErrNotFound.
	Lock(ctx context.Context, id uuid.UUID) (Entitlement, error)
	// List returns up to limit entitlements of orgID matching f, newest
	// first, with an ID below after (nil: from the newest).
	List(ctx context.Context, orgID uuid.UUID, f ListFilter, after *uuid.UUID, limit int) ([]Entitlement, error)
	// Usable returns the active entitlements of orgID and feature whose
	// validity window contains now, earliest valid_to first (then ID): the
	// order in which a consumption tries them (02 §10.1).
	Usable(ctx context.Context, orgID uuid.UUID, f Feature, now time.Time) ([]Entitlement, error)
	// BySource returns the entitlements granted by a source (any org), in ID order.
	BySource(ctx context.Context, t SourceType, sourceID uuid.UUID) ([]Entitlement, error)

	// TryConsume is the race-free consumption (02 §10.1): one conditional
	// UPDATE adds qty to quantity_used only while the row belongs to orgID,
	// is active, inside its validity window at now and has qty units left;
	// the last unit flips the status to exhausted. It reports whether the
	// row was updated (false: try the next entitlement). Concurrent calls
	// serialise on the row, so they never exceed quantity_total.
	TryConsume(ctx context.Context, orgID, id uuid.UUID, qty int, now time.Time) (Entitlement, bool, error)
	// Unconsume gives qty units back (a reversal, or the undo of a
	// consumption that lost the Ref race): an exhausted row becomes active
	// again while valid at now, else expired. ErrNotFound when the row is
	// gone or has fewer than qty units used.
	Unconsume(ctx context.Context, id uuid.UUID, qty int, now time.Time) error
	// Activate sets the pending entitlements of a source active
	// (activated_at = now) and returns how many changed.
	Activate(ctx context.Context, t SourceType, sourceID uuid.UUID, now time.Time) (int, error)
	// Revoke sets the pending, active or exhausted entitlements of a source
	// revoked and returns how many changed.
	Revoke(ctx context.Context, t SourceType, sourceID uuid.UUID, now time.Time, reason string) (int, error)
	// RevokeOne sets one entitlement revoked; ErrNotFound when it is gone
	// or not pending, active or exhausted.
	RevokeOne(ctx context.Context, id uuid.UUID, now time.Time, reason string) (Entitlement, error)
	// Expire sets up to limit active entitlements whose valid_to is not
	// after now expired and returns them (entitlements.expire). Rows locked
	// by a running transaction are skipped until the next batch.
	Expire(ctx context.Context, now time.Time, limit int) ([]Entitlement, error)

	// InsertUsage appends a usage row and returns it as stored. A
	// consumption (ReversalOf nil) takes org_id from the entitlement row
	// u.EntitlementID; a reversal takes entitlement, org, ref and the
	// negated quantity from the usage *u.ReversalOf (u's own values of those
	// fields are ignored; 06 H-38, T-134). It reports false and writes
	// nothing when the Ref already has a consumption, the reversed usage
	// already has a reversal (the unique indexes of 02 §10.2) or the parent
	// row does not exist; a concurrent insert of the same Ref waits for the
	// other transaction to end.
	InsertUsage(ctx context.Context, u Usage) (Usage, bool, error)
	// UsageByRef returns the consumption (the positive row) of a Ref and
	// whether it has been reversed, or ErrNotFound.
	UsageByRef(ctx context.Context, ref Ref) (u Usage, reversed bool, err error)
	// ListUsages returns up to limit usage rows of orgID (consumptions and
	// reversals) matching f, newest first, with an ID below after.
	ListUsages(ctx context.Context, orgID uuid.UUID, f UsageFilter, after *uuid.UUID, limit int) ([]UsageView, error)

	// UsagesOfActor returns every usage row a user made, oldest first
	// (KVKK export; BE-057).
	UsagesOfActor(ctx context.Context, userID uuid.UUID) ([]Usage, error)
	// PurgeEndedGrants deletes, with their usages, admin and promo grants
	// whose validity ended (revoked_at, else valid_to) before cutoff and no
	// usage of which still proves a post, a dispatch or a contact request,
	// oldest first, at most limit rows (a grant and its usages) per call
	// but always at least one grant; it returns the rows deleted. dryRun
	// counts the grants and deletes nothing (entitlement_other; BE-057).
	// The GORM store goes through 000121's entitlements_purge (the usages
	// are append-only): it never passes now − the policy's period and
	// refuses a real purge of an unapproved policy (55000).
	PurgeEndedGrants(ctx context.Context, cutoff time.Time, limit int, dryRun bool) (int, error)
}

// ListFilter narrows listOrgEntitlements.
type ListFilter struct {
	Feature Feature // "" = every feature
	Status  Status  // "" = every status
}

// UsageFilter narrows listOrgEntitlementUsages.
type UsageFilter struct {
	Feature Feature // "" = every feature
}
