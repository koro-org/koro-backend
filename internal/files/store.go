package files

import (
	"context"
	"time"
	"uuid"
)

// Store is the persistence boundary of files (05 §2.2). Org scoping does
// not apply: org_id is optional metadata of a file, rows are addressed by
// their ID and the owning module decides access (01 §4.2). The GORM store
// (store_gorm.go) and the in-memory store (store_mem.go) pass the same
// contract test (store_contract_test.go).
//
// Every method works in the transaction carried by ctx, if any.
type Store interface {
	// Insert writes a new row.
	Insert(ctx context.Context, f File) error
	// Get returns the row or ErrNotFound.
	Get(ctx context.Context, id uuid.UUID) (File, error)
	// Lock returns the row locked for the rest of the transaction
	// (SELECT … FOR UPDATE), or ErrNotFound.
	Lock(ctx context.Context, id uuid.UUID) (File, error)
	// Update writes every mutable column of f (all but id and created_at;
	// updated_at is set by the trigger). ErrNotFound when the row is gone.
	Update(ctx context.Context, f File) error
	// Delete removes the row; a missing row is not an error. A row still
	// referenced by another table fails with ErrReferenced.
	Delete(ctx context.Context, id uuid.UUID) error
	// Orphans returns up to limit unattached rows created before the cutoff
	// with an ID greater than after, in ID order (files.orphan_cleanup).
	Orphans(ctx context.Context, before time.Time, after uuid.UUID, limit int) ([]File, error)
	// PendingScans returns up to limit active rows whose scan is pending
	// and that were last updated before the cutoff, with an ID greater than
	// after, in ID order (files.rescan_pending).
	PendingScans(ctx context.Context, before time.Time, after uuid.UUID, limit int) ([]File, error)
	// UnattachedOf returns the unattached rows whose data subject is userID
	// (KVKK export and erasure), in ID order.
	UnattachedOf(ctx context.Context, userID uuid.UUID) ([]File, error)
}
