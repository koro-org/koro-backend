package outreach

import (
	"context"
	"time"
	"uuid"
)

// Store is the persistence boundary of outreach (05 §2.2) over segments
// (02 §13.1) and outreach_dispatches (02 §13.2; BE-035). The GORM store
// (store_gorm.go) and the in-memory store (store_mem.go) pass the same
// contract test (store_contract_test.go).
//
// Every method works in the transaction carried by ctx, if any; the service
// draws the boundaries.
//
// Org scope: a segment is addressed under its org (/orgs/{org_id}/
// segments/{segment_id}); every org-facing method takes the org and finds
// nothing outside it (IDOR → ErrNotFound). Only the nightly recount walks
// every org (ActiveAfter, SetCount: cross-org, system job).
type Store interface {
	// Insert writes a new segment and returns it as stored (created_at and
	// updated_at from the database).
	Insert(ctx context.Context, s Segment) (Segment, error)
	// Get returns segment id of orgID or ErrNotFound.
	Get(ctx context.Context, orgID, id uuid.UUID) (Segment, error)
	// Lock is Get with the row locked for the transaction.
	Lock(ctx context.Context, orgID, id uuid.UUID) (Segment, error)
	// Update writes the mutable columns of s (name, definition,
	// audience_keys, audience_dim_count, last_count, last_counted_at,
	// archived_at) scoped to s.OrgID and returns the stored row: updated_at
	// changes on every call, so the ETag does too (04 §1.13). ErrNotFound
	// when the segment is not s.OrgID's.
	Update(ctx context.Context, s Segment) (Segment, error)
	// List returns up to limit segments of orgID, newest first (id DESC),
	// after the position after (nil: the first page): the archived ones
	// when archived is true, the active ones otherwise.
	List(ctx context.Context, orgID uuid.UUID, archived bool, after *uuid.UUID, limit int) ([]Segment, error)
	// ActiveAfter returns up to limit active segments of every org with id
	// > after, by id (the nightly recount).
	ActiveAfter(ctx context.Context, after uuid.UUID, limit int) ([]Segment, error)
	// SetCount writes last_count and last_counted_at of an active segment
	// of orgID (the nightly recount); an archived or missing one is left
	// alone and reported false.
	SetCount(ctx context.Context, orgID, id uuid.UUID, count *int, at time.Time) (bool, error)

	DispatchStore
}

// DispatchStore is the outreach_dispatches part of Store (02 §13.2;
// BE-035). The org-facing methods take the org and find nothing outside it
// (IDOR → ErrNotFound); the worker and the retention sweeper address a
// dispatch by its ID across orgs (system paths: the event and the job carry
// the ID, each row carries its org).
type DispatchStore interface {
	// InsertDispatch writes a new dispatch and returns it as stored. A
	// second dispatch of the same post is ErrDispatchExists
	// (outreach_dispatches_post_id_uq); a post or segment that is not the
	// org's is ErrNotFound (the composite FKs of 06 H-38).
	InsertDispatch(ctx context.Context, d Dispatch) (Dispatch, error)
	// GetDispatch returns dispatch id of orgID or ErrNotFound.
	GetDispatch(ctx context.Context, orgID, id uuid.UUID) (Dispatch, error)
	// LockDispatch is GetDispatch with the row locked for the transaction.
	LockDispatch(ctx context.Context, orgID, id uuid.UUID) (Dispatch, error)
	// DispatchOfPost returns the dispatch of postID in orgID or ErrNotFound;
	// lock locks the row for the transaction.
	DispatchOfPost(ctx context.Context, orgID, postID uuid.UUID, lock bool) (Dispatch, error)
	// ListDispatches returns up to limit dispatches of orgID, newest first
	// (id DESC), after the position after (nil: the first page).
	ListDispatches(ctx context.Context, orgID uuid.UUID, after *uuid.UUID, limit int) ([]Dispatch, error)
	// DeleteDispatch deletes dispatch id of orgID (a post withdrawn before
	// it was ever published); a missing one is ErrNotFound.
	DeleteDispatch(ctx context.Context, orgID, id uuid.UUID) error
	// TransitionDispatch moves dispatch id of orgID from status from to
	// status to, with lastError (nil: NULL); a terminal status stamps
	// completed_at with at. ok=false (and the row unchanged) when the row is
	// not in from: a concurrent change won.
	TransitionDispatch(ctx context.Context, orgID, id uuid.UUID, from, to DispatchStatus, lastError *string, at time.Time) (Dispatch, bool, error)
	// CompleteDispatch writes the published counts of a running dispatch
	// of orgID and completes it at at; ok=false when it is not running.
	CompleteDispatch(ctx context.Context, orgID, id uuid.UUID, c DispatchCounts, at time.Time) (bool, error)

	// DispatchByID returns dispatch id of any org (worker) or ErrNotFound.
	DispatchByID(ctx context.Context, id uuid.UUID) (Dispatch, error)
	// ClaimDispatch moves dispatch id to running (started_at set on the
	// first claim) when it is scheduled, or running with a claim older than
	// lease (updated_at; an abandoned run). ok=false otherwise: a terminal
	// dispatch, or a run in progress elsewhere.
	ClaimDispatch(ctx context.Context, id uuid.UUID, now time.Time, lease time.Duration) (Dispatch, bool, error)
	// TouchDispatch renews the claim of a running dispatch (updated_at).
	TouchDispatch(ctx context.Context, id uuid.UUID) error
	// PendingDispatches returns up to limit dispatches with id > after, by
	// id: the scheduled ones created before scheduledBefore and the running
	// ones whose claim is older than lease (outreach.resume_dispatches).
	PendingDispatches(ctx context.Context, scheduledBefore time.Time, lease time.Duration, after uuid.UUID, limit int) ([]Dispatch, error)
	// PurgeDispatches deletes up to limit finished dispatches (completed,
	// cancelled, failed) created before cutoff, oldest first, and returns
	// how many it deleted; with dryRun it counts them all and deletes
	// nothing (retention outreach_dispatch, 2 years).
	PurgeDispatches(ctx context.Context, cutoff time.Time, limit int, dryRun bool) (int, error)
}
