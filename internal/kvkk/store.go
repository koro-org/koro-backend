package kvkk

import (
	"context"
	"time"
	"uuid"
)

// Store is the persistence boundary of kvkk's request workflow (05 §2.2)
// over data_subject_requests (02 §16.1). No kvkk table is org-owned
// ("Org kapsamı: yok"): every method is cross-org and addresses rows by ID
// or by data subject. The GORM store (store_gorm.go) and the in-memory store
// (store_mem.go) pass the same contract test (store_contract_test.go).
//
// Every method works in the transaction carried by ctx, if any. Rows are
// never deleted here: closed requests are proof for 3 years (dsr_record,
// swept by BE-049/BE-057).
type Store interface {
	// Insert writes a new request (created_at/updated_at = ReceivedAt).
	Insert(ctx context.Context, d DSR) error
	// Get returns a request or ErrNotFound.
	Get(ctx context.Context, id uuid.UUID) (DSR, error)
	// GetOwn returns the request id of userID, or ErrNotFound when it does
	// not exist or belongs to someone else (IDOR rule: 404).
	GetOwn(ctx context.Context, id, userID uuid.UUID) (DSR, error)
	// Lock returns a request locked for the rest of the transaction.
	Lock(ctx context.Context, id uuid.UUID) (DSR, error)
	// Update writes the mutable columns of d (status, identity_verified_at,
	// assigned_to, first/second_approver, response_summary,
	// export_file_id, rejection_reason, completed_at); updated_at moves.
	Update(ctx context.Context, d DSR) error

	// ListOwn returns up to limit requests of userID, newest first
	// (received_at DESC, id DESC), after the cursor.
	ListOwn(ctx context.Context, userID uuid.UUID, after *After, limit int) ([]DSR, error)
	// List returns up to limit requests matching f, soonest deadline first
	// (due_at ASC, id ASC), after the cursor.
	List(ctx context.Context, f AdminFilter, after *After, limit int) ([]DSR, error)
	// HasOpen reports whether userID has an open request of type t.
	HasOpen(ctx context.Context, userID uuid.UUID, t Type) (bool, error)
	// AllOf returns every request of userID, oldest first (KVKK export and
	// erasure of kvkk's own data).
	AllOf(ctx context.Context, userID uuid.UUID) ([]DSR, error)

	// DueBy returns up to limit open requests whose due_at is at or before
	// cutoff, soonest first, after the cursor (kvkk.dsr_deadlines).
	DueBy(ctx context.Context, cutoff time.Time, after *After, limit int) ([]DSR, error)
	// ExportsCompletedBefore returns up to limit requests with an export
	// file that were completed at or before cutoff, in ID order after afterID
	// (kvkk.purge_exports).
	ExportsCompletedBefore(ctx context.Context, cutoff time.Time, afterID uuid.UUID, limit int) ([]DSR, error)
}
