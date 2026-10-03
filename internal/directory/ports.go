package directory

import (
	"context"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/orgs"
)

// AuditLogger is what directory needs from audit (L1): every write is
// logged in the business transaction.
type AuditLogger interface {
	// Log appends an audit event in the transaction carried by ctx.
	//
	// sync-write: audit.Logger.Log
	Log(ctx context.Context, e audit.Event) error
}

// Orgs is what directory needs from orgs (L5, 01 §4.7): the org of a new
// club or university is opened in directory's transaction (01 §6.3, T-78),
// and the catalog reads the org's public profile.
type Orgs interface {
	// Create writes a new org in the transaction carried by ctx (it joins
	// the caller's: directory's rollback removes the org as well).
	//
	// sync-write: orgs.OrgAdmin.Create
	Create(ctx context.Context, in orgs.CreateOrg) (uuid.UUID, error)
	// Get returns the summary of an org (any status) or orgs.ErrNotFound.
	Get(ctx context.Context, orgID uuid.UUID) (orgs.OrgSummary, error)
	// GetOrg returns the public profile of an org as a sees it (404 for a
	// pending_review or closed org a may not see).
	GetOrg(ctx context.Context, a authz.Actor, orgID uuid.UUID) (orgs.OrgView, error)
}

// Reauthenticator verifies the re-authentication proof of the R operation
// adminSetClubSensitivity (03 §6.2): after authz.Can, before any business
// data. It is identity.Service (L4).
type Reauthenticator interface {
	Verify(ctx context.Context, userID uuid.UUID, proof identity.ReauthProof) (kind string, err error)
}

// Sender is notify's Sender (L3): the worker task of requestClubSensitivity
// sends in-app notices to the platform admins.
type Sender interface {
	Send(ctx context.Context, m notify.Message) (uuid.UUID, error)
}

// PlatformAdmins returns the users with an effective platform_admin role
// (06 H-33), read in the worker. identity provides it (cmd/koro).
type PlatformAdmins func(ctx context.Context) ([]uuid.UUID, error)

// MemberCounter returns the number of active members of a club org
// (orgs.OrgDirectory.ActiveMemberIDs, BE-019). found=false: not known
// (shown as null).
type MemberCounter func(ctx context.Context, orgID uuid.UUID) (count int, found bool, err error)
