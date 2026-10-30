package identity

import (
	"context"
	"uuid"
)

// accountActionStore is the privileged_account_actions rows (06 H-47): the
// four-eyes requests of an admin action against a privileged account
// (admin.go).
type accountActionStore interface {
	// InsertAccountAction writes a four-eyes account action request (06
	// H-47). A second pending request of (target, action) is
	// ErrAccountActionOpen; a self request ErrForbidden.
	InsertAccountAction(ctx context.Context, r AccountActionRequest) error
	// LockOpenAccountAction returns the pending request of (target,
	// action) locked for the transaction; found=false when none.
	LockOpenAccountAction(ctx context.Context, target uuid.UUID, action AccountActionKind) (r AccountActionRequest, found bool, err error)
	// UpdateAccountAction writes status, approved_by and approved_at of a
	// request; approved_by = requested_by is ErrFourEyes (DB CHECK).
	UpdateAccountAction(ctx context.Context, r AccountActionRequest) error
	// AccountActionsOf returns every request against userID, oldest first
	// (KVKK export).
	AccountActionsOf(ctx context.Context, userID uuid.UUID) ([]AccountActionRequest, error)
}
