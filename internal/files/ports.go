package files

import (
	"context"

	"koro/internal/audit"
)

// AuditLogger is what files needs from audit (01 §4.1): every write of a
// file row (upload start and completion, attach, delete, orphan purge) is
// logged in the transaction of the write.
type AuditLogger interface {
	// Log appends an audit event in the transaction carried by ctx.
	//
	// sync-write: audit.Logger.Log
	Log(ctx context.Context, e audit.Event) error
}
