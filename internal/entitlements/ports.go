package entitlements

import (
	"context"
	"uuid"

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

// OrgKindFunc returns the kind of an org ("company", "club", ...) and
// whether it exists (06 H-40, T-148: an admin grant goes to companies
// only). organizations belongs to orgs (L5, above entitlements), so the
// composition root binds it late (cmd/koro: orgs fills it in its build).
type OrgKindFunc func(ctx context.Context, orgID uuid.UUID) (kind string, found bool, err error)
