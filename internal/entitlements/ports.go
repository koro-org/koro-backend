package entitlements

import (
	"context"

	"koro/internal/audit"
)

// AuditLogger is what entitlements needs from audit (01 §4.1): admin grants
// and revocations and the expiry job are written in their own transaction.
type AuditLogger interface {
	// Log appends an audit event in the transaction carried by ctx.
	//
	// sync-write: audit.Logger.Log
	Log(ctx context.Context, e audit.Event) error
}
