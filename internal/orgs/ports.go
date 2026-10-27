package orgs

import (
	"context"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/notify"
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
	// PublicURL returns the permanent public media URL of the logo ("" while
	// there is none; BE-120). orgs shows it on public orgs only.
	PublicURL(ctx context.Context, fileID uuid.UUID) (string, error)
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

// Users is what orgs needs from identity's user directory (L4; 01 §4.6):
// member names for the member list, the account's address for an
// invitation's e-mail match, the age of a club_admin candidate and the
// locale of the invitation e-mail. It is identity.Service.
type Users interface {
	// Get returns a user or identity.ErrNotFound.
	Get(ctx context.Context, id uuid.UUID) (identity.UserSummary, error)
	// GetMany returns the users of ids that exist.
	GetMany(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]identity.UserSummary, error)
	// Contact returns the address of a live user or identity.ErrNotFound.
	Contact(ctx context.Context, id uuid.UUID) (identity.UserContact, error)
	// FindByEmail returns the account of an address.
	FindByEmail(ctx context.Context, email string) (uuid.UUID, bool, error)
}

// Students answers whether a user is a verified student (identity
// StudentVerification; BE-018 fills it in, until then nobody is). club_admin
// needs one (03 k5, 02 §7.3), and the admin sweep ends the role without it.
type Students interface {
	Active(ctx context.Context, userID uuid.UUID) (identity.EduVerification, bool, error)
	ActiveMany(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]identity.EduVerification, error)
}

// Sender delivers the notifications of the orgs workers (notify, L3): the
// invitation e-mail and the review notices (BE-144: in-app and e-mail).
type Sender interface {
	Send(ctx context.Context, m notify.Message) (uuid.UUID, error)
}

// PlatformAdmins returns the users holding platform_admin in effect (06
// H-33): the ones who may verify an org (orgs.org.verify). identity, bound
// in cmd/koro.
type PlatformAdmins func(ctx context.Context) ([]uuid.UUID, error)

// ClubInfo returns the join-relevant facts of a club org (T-77): its
// university, join policy and sensitivity live in directory (L6), above
// orgs, so directory binds it late (hooks.clubInfo, BE-020). found=false
// when directory has no club for the org. With no provider joinClub is 404
// and every club counts as sensitive (fail closed: its member list is the
// club_admin's only, and an ended membership is deleted at once).
type ClubInfo func(ctx context.Context, clubOrgID uuid.UUID) (club ClubFacts, found bool, err error)

// GraduatedUsers reports which of userIDs have a graduated student profile
// (students, L7, binds it late; BE-023). The admin sweep uses it as the
// safety net of student.graduated (01 §7; T-210). nil → the sweep checks
// the student verification only.
type GraduatedUsers func(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]bool, error)

// AvatarURLs returns short-lived avatar URLs of the users among userIDs
// that have one (students, L7, binds it late; BE-023). The member list
// shows them (04 §2.2 MemberDTO.user.avatar_url). nil, or an error (logged:
// an avatar never fails a read) → no avatar.
type AvatarURLs func(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]string, error)
