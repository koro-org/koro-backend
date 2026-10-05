package directory

import (
	"context"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/files"
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
// an approved club claim grants club_admin in directory's transaction
// (01 §6.3, 02 §8.6), and the catalog reads the org's public profile.
type Orgs interface {
	// Create writes a new org in the transaction carried by ctx (it joins
	// the caller's: directory's rollback removes the org as well).
	//
	// sync-write: orgs.OrgAdmin.Create
	Create(ctx context.Context, in orgs.CreateOrg) (uuid.UUID, error)
	// GrantRole gives userID role in orgID in the transaction carried by
	// ctx (club claim approval): club_admin needs a verified adult student
	// (422 orgs.club_admin_requires_adult_student); the caller audits.
	//
	// sync-write: orgs.OrgAdmin.GrantRole
	GrantRole(ctx context.Context, orgID, userID uuid.UUID, role authz.MemberRole, reason string) error
	// Get returns the summary of an org (any status) or orgs.ErrNotFound.
	Get(ctx context.Context, orgID uuid.UUID) (orgs.OrgSummary, error)
	// GetMany returns the summaries of the ids that exist (club names of
	// the claim lists).
	GetMany(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]orgs.OrgSummary, error)
	// ActiveMemberIDs and HasRole find the university_rep of a club's
	// university: the approver of its claims (E-21).
	ActiveMemberIDs(ctx context.Context, orgID uuid.UUID) ([]uuid.UUID, error)
	HasRole(ctx context.Context, orgID, userID uuid.UUID, roles ...authz.MemberRole) (bool, error)
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

// Files is what directory needs from files (L2) for the evidence of a club
// claim (purpose club_claim_evidence, 02 §8.6): the upload's metadata,
// attaching it to its claim and the short download of the admin review.
type Files interface {
	// Get returns the file's metadata or files.ErrNotFound.
	Get(ctx context.Context, fileID uuid.UUID) (files.FileMeta, error)
	// Attach binds the evidence to its claim in the caller's transaction
	// (orphan cleanup leaves it alone).
	//
	// sync-write: files.Files.Attach
	Attach(ctx context.Context, a authz.Actor, fileID uuid.UUID, owner files.Owner) error
	// PresignGet returns a download URL valid for ttl (≤ 5 minutes) of a
	// clean file; files.ErrNotClean otherwise.
	PresignGet(ctx context.Context, fileID uuid.UUID, ttl time.Duration) (files.Download, error)
	// Delete removes a claim's evidence in the caller's transaction once
	// directory has cleared its reference: KVKK erasure, a file the virus
	// scan rejected and the end of the claim's retention (club_claim,
	// BE-057). The objects go after the commit. The pair is on the
	// 01 §6.3 allow-list (BE-057).
	// sync-write: files.Files.Delete
	Delete(ctx context.Context, a authz.Actor, fileID uuid.UUID) error
}

// Students answers whether a user holds an active student verification and
// of which university (identity, L4). A claim approval re-checks the
// claimant's university at decision time (03 k8).
type Students interface {
	Active(ctx context.Context, userID uuid.UUID) (identity.EduVerification, bool, error)
}

// Users returns the display names of claimants for the admin review
// (identity, L4). Optional: nil leaves the names out.
type Users interface {
	GetMany(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]identity.UserSummary, error)
}
