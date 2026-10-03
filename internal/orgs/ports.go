package orgs

import (
	"context"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/identity"
)

// AuditLogger is what orgs needs from audit (L1): every write is logged in
// the business transaction.
type AuditLogger interface {
	// Log appends an audit event in the transaction carried by ctx.
	//
	// sync-write: audit.Logger.Log
	Log(ctx context.Context, e audit.Event) error
}

// Reauthenticator verifies the re-authentication proof of the R operations
// (03 §6.2: closeOrg, transferOrgOwnership, adminSuspendOrg,
// adminReactivateOrg): after authz.Can, before any business data. It is
// identity.Service (L4).
type Reauthenticator interface {
	Verify(ctx context.Context, userID uuid.UUID, proof identity.ReauthProof) (kind string, err error)
}

// Files is what orgs needs from files (L2) for the org logo (02 §7.1
// logo_file_id, purpose org_logo).
type Files interface {
	// Get returns the file's metadata or files.ErrNotFound.
	Get(ctx context.Context, fileID uuid.UUID) (files.FileMeta, error)
	// Attach binds the logo to the org in the caller's transaction.
	//
	// sync-write: files.Files.Attach
	Attach(ctx context.Context, a authz.Actor, fileID uuid.UUID, owner files.Owner) error
	// PresignGet returns a short-lived download URL of a clean file.
	PresignGet(ctx context.Context, fileID uuid.UUID, ttl time.Duration) (files.Download, error)
}

// SessionRevoker closes every open session of a user with reason
// role_change (03 §2.2-6, 04 §2.2 createCompany: the founder must sign in
// again so that the MFA policy of company_owner applies at once). It is
// identity.Sessions.RevokeAll, bound in cmd/koro.
//
// orgs calls it after its own transaction has committed (db.AfterCommit),
// never inside it: identity.Sessions.RevokeAll is not on the 01 §6.3 list
// of same-transaction cross-module writes (internal/arch sync_writes.txt).
// A failed revocation is logged; the role path still demands MFA (k-mfa)
// and identity applies the MFA role's 30 min / 12 h windows at the next
// request, so an old session gains nothing.
type SessionRevoker func(ctx context.Context, userID uuid.UUID) (int, error)

// OpenCommerceCheck reports whether an org still has open orders or
// payouts (hooks.openCommerce: orders BE-040, ledger BE-044). It runs in
// closeOrg's transaction.
type OpenCommerceCheck func(ctx context.Context, orgID uuid.UUID) (open bool, err error)

// UniversityOrgs returns the orgs that belong to a university (its own
// university org and its clubs) for searchOrgs?university_id (04 §2.2).
// The university ↔ club relation lives in directory (L6), above orgs, so
// directory binds it late (BE-020). nil → the filter matches nothing.
type UniversityOrgs func(ctx context.Context, universityID uuid.UUID) ([]uuid.UUID, error)
