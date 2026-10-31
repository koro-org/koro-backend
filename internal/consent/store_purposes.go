package consent

import (
	"context"
)

// purposeStore is consent_purposes, the purpose catalogue the notices and
// consents refer to (000010).
type purposeStore interface {
	// Purposes returns every purpose ordered by code.
	Purposes(ctx context.Context) ([]PurposeDef, error)
	// Purpose returns one purpose or ErrUnknownPurpose.
	Purpose(ctx context.Context, code Purpose) (PurposeDef, error)
	// LockPurpose returns the purpose locked for the rest of the transaction
	// (SELECT … FOR UPDATE), or ErrUnknownPurpose.
	LockPurpose(ctx context.Context, code Purpose) (PurposeDef, error)
	// UpdatePurpose writes description_tr and active of p.
	UpdatePurpose(ctx context.Context, p PurposeDef) error
}
