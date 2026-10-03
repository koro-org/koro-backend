package students

import (
	"context"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/directory"
	"koro/internal/files"
	"koro/internal/identity"
)

// AuditLogger is what students needs from audit (L1): every write, every
// consent and visibility change, in the business transaction.
type AuditLogger interface {
	// Log appends an audit event in the transaction carried by ctx.
	//
	// sync-write: audit.Logger.Log
	Log(ctx context.Context, e audit.Event) error
}

// ConsentRecorder records the consents that discoverable and
// show_contact_on_grant mirror, in the privacy update's transaction
// (02 §9.2, 03 k9; 01 §6.3 "students gizlilik ayarı → consent.Recorder").
type ConsentRecorder interface {
	// Record appends one consent row in the transaction carried by ctx.
	//
	// sync-write: consent.Recorder.Record
	Record(ctx context.Context, in consent.RecordInput) (uuid.UUID, error)
}

// ConsentChecker answers which users' consent is in force (consent,
// Checker). The nightly refresh turns off a flag whose consent was
// withdrawn through the consent module.
type ConsentChecker interface {
	FilterActive(ctx context.Context, userIDs []uuid.UUID, purpose consent.Purpose) ([]uuid.UUID, error)
}

// Catalog is what students needs from directory (L6; 01 §4.8): names and
// validation of the reference data and the sensitive flag of clubs.
type Catalog interface {
	// GetUniversity returns a university or directory.ErrNotFound.
	GetUniversity(ctx context.Context, id uuid.UUID) (directory.University, error)
	// GetDepartment returns a department or directory.ErrNotFound.
	GetDepartment(ctx context.Context, id uuid.UUID) (directory.Department, error)
	// SkillsExist returns directory.ErrUnknownSkill unless every id is an
	// active skill.
	SkillsExist(ctx context.Context, ids []uuid.UUID) error
	// SensitiveOrgIDs reports which of orgIDs are sensitive clubs.
	SensitiveOrgIDs(ctx context.Context, orgIDs []uuid.UUID) (map[uuid.UUID]bool, error)
}

// Memberships is what students needs from orgs (L5; 01 §4.7): the active
// memberships of a user (sensitive clubs included) and the active members
// of a club.
type Memberships interface {
	MembershipsOf(ctx context.Context, userID uuid.UUID) ([]authz.Membership, error)
	ActiveMemberIDs(ctx context.Context, orgID uuid.UUID) ([]uuid.UUID, error)
}

// Users is what students needs from identity's user directory (L4): the
// account status and the age of the students (audience eligibility,
// cohorts).
type Users interface {
	// GetMany returns the users of ids that exist.
	GetMany(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]identity.UserSummary, error)
}

// Verifications is identity's student verification (01 §4.6): an active
// verification is a condition of audience eligibility and the source of
// the profile's university.
type Verifications interface {
	ActiveMany(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]identity.EduVerification, error)
}

// Files is what students needs from files (L2) for the avatar
// (purpose avatar; 04 §1.14).
type Files interface {
	// Get returns the file's metadata or files.ErrNotFound.
	Get(ctx context.Context, fileID uuid.UUID) (files.FileMeta, error)
	// Attach binds the avatar to the profile in the caller's transaction.
	//
	// sync-write: files.Files.Attach
	Attach(ctx context.Context, a authz.Actor, fileID uuid.UUID, owner files.Owner) error
	// PresignGet returns a short-lived download URL of a clean file.
	PresignGet(ctx context.Context, fileID uuid.UUID, ttl time.Duration) (files.Download, error)
}

// FileRemover deletes a replaced or erased avatar (files.Files.Delete) in
// the caller's transaction, after students has released the reference:
// the files row goes with the transaction, the stored objects after the
// commit (files retries them). It is the pattern of identity's RemoveFile
// (student documents); the pair is not on the 01 §6.3 list yet (H-49 adds
// kvkk → files.Files.Delete; see the ticket notes).
type FileRemover func(ctx context.Context, a authz.Actor, fileID uuid.UUID) error
