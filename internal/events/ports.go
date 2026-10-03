package events

import (
	"context"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/directory"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/orgs"
)

// AuditLogger is what events needs from audit (L1): every write is logged
// in the business transaction.
type AuditLogger interface {
	// Log appends an audit event in the transaction carried by ctx.
	//
	// sync-write: audit.Logger.Log
	Log(ctx context.Context, e audit.Event) error
}

// Orgs is what events needs from orgs (L5; 01 §4.7 OrgDirectory): the
// organiser's kind and status (03 §2.3) and its public name for the DTOs.
type Orgs interface {
	// Get returns the summary of an org (any status) or orgs.ErrNotFound.
	Get(ctx context.Context, orgID uuid.UUID) (orgs.OrgSummary, error)
	// GetMany returns the summaries of the ids that exist.
	GetMany(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]orgs.OrgSummary, error)
}

// Catalog is what events needs from directory (L6; 01 §4.8 Catalog and
// directory.Service.UniversityOrgs): the sensitive club flag (an event of a
// sensitive club is Sensitive, 02 §11.1), a club's university, the
// universities of university-only events and the orgs of a university.
type Catalog interface {
	// SensitiveOrgIDs reports which of orgIDs are sensitive clubs.
	SensitiveOrgIDs(ctx context.Context, orgIDs []uuid.UUID) (map[uuid.UUID]bool, error)
	// GetClub returns the catalog data of a club org or directory.ErrNotFound.
	GetClub(ctx context.Context, orgID uuid.UUID) (directory.Club, error)
	// GetUniversity returns a university or directory.ErrNotFound.
	GetUniversity(ctx context.Context, id uuid.UUID) (directory.University, error)
	// UniversityOrgs returns the orgs of a university (its own org and its
	// clubs) for listEvents?university_id; an unknown university has none.
	UniversityOrgs(ctx context.Context, universityID uuid.UUID) ([]uuid.UUID, error)
}

// Users is what events needs from identity's user directory (L4; 01
// §4.6): the account of a staff member's e-mail (addEventStaff) and the
// names on the staff list. It is identity.Service.
type Users interface {
	// Get returns a user or identity.ErrNotFound.
	Get(ctx context.Context, id uuid.UUID) (identity.UserSummary, error)
	// GetMany returns the users of ids that exist.
	GetMany(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]identity.UserSummary, error)
	// FindByEmail returns the account of an address.
	FindByEmail(ctx context.Context, email string) (uuid.UUID, bool, error)
}

// Files is what events needs from files (L2) for the event cover (02 §11.1
// cover_file_id, purpose event_cover).
type Files interface {
	// Get returns the file's metadata or files.ErrNotFound.
	Get(ctx context.Context, fileID uuid.UUID) (files.FileMeta, error)
	// Attach binds the cover to the event in the caller's transaction.
	//
	// sync-write: files.Files.Attach
	Attach(ctx context.Context, a authz.Actor, fileID uuid.UUID, owner files.Owner) error
	// PresignGet returns a short-lived download URL of a clean file.
	PresignGet(ctx context.Context, fileID uuid.UUID, ttl time.Duration) (files.Download, error)
}
