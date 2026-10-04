package outreach

import (
	"context"
	"time"
	"uuid"
)

// Store is the persistence boundary of outreach (05 §2.2) over segments
// (02 §13.1; outreach_dispatches comes with BE-035). The GORM store
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
}
