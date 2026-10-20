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
	"koro/internal/orgs"
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
	// GetClub returns the catalog data of a club org or
	// directory.ErrNotFound (an unknown club counts as sensitive where
	// students must decide: the member card, the admin view; T-240).
	GetClub(ctx context.Context, orgID uuid.UUID) (directory.Club, error)
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

// Members is the single membership read of orgs (L5) behind the member
// card (04 §V.4): orgs answers as the caller sees it (orgs.member.read:
// the club's club_admin; 03 k4: a sensitive club's read is audited by
// orgs too). students checks its own action first, so only a club_admin
// of the org ever reaches it.
type Members interface {
	GetMember(ctx context.Context, a authz.Actor, orgID, id uuid.UUID, access orgs.Access) (orgs.MemberView, error)
}

// AdminUsers is identity's account view of the platform staff (L4;
// identity.user.read: support, dpo, platform_admin): the account part of
// the admin student view (04 §V.5). It hides every club membership.
type AdminUsers interface {
	AdminUser(ctx context.Context, a authz.Actor, id uuid.UUID) (identity.AdminUser, error)
}

// DSRs answers whether a data subject request is open and whose it is
// (kvkk, L13: bound at call time by cmd/koro). The dpo reads a student's
// profile only for the subject of an open request (03 k11).
type DSRs interface {
	// OpenSubject returns the data subject of request dsrID as a sees it;
	// ok=false when the request is unknown, closed, of an applicant
	// without an account, or not visible to a.
	OpenSubject(ctx context.Context, a authz.Actor, dsrID uuid.UUID) (subject uuid.UUID, ok bool, err error)
}

// RegistrationSummary is one event registration of a student without its
// answers (04 §V.5 registrations[]).
type RegistrationSummary struct {
	EventID      uuid.UUID
	EventTitle   string
	Status       string
	RegisteredAt time.Time
}

// Registrations is what students needs from events (L8, built after
// students: bound at call time; nil until events provides it).
type Registrations interface {
	// CountInOrg returns how many registrations userID has for the events
	// of orgID (the member card's club_event_registrations_count).
	CountInOrg(ctx context.Context, orgID, userID uuid.UUID) (int, error)
	// SummaryOf returns userID's registrations, newest first, without
	// form answers (the admin view).
	SummaryOf(ctx context.Context, userID uuid.UUID) ([]RegistrationSummary, error)
}

// Files is what students needs from files (L2) for the avatar and the CVs
// (purposes avatar and cv; 04 §1.14).
type Files interface {
	// Get returns the file's metadata or files.ErrNotFound.
	Get(ctx context.Context, fileID uuid.UUID) (files.FileMeta, error)
	// Attach binds an avatar or a CV to its record in the caller's
	// transaction.
	//
	// sync-write: files.Files.Attach
	Attach(ctx context.Context, a authz.Actor, fileID uuid.UUID, owner files.Owner) error
	// PresignGet returns a short-lived download URL of a clean file.
	PresignGet(ctx context.Context, fileID uuid.UUID, ttl time.Duration) (files.Download, error)
	// Delete removes a replaced or erased avatar, a deleted or erased CV
	// and a file the scan rejected in the caller's transaction, after
	// students has released the reference: the files row goes with the
	// transaction, the stored objects after the commit (files retries
	// them). The pair is on the 01 §6.3 allow-list (T-239, BE-057).
	// sync-write: files.Files.Delete
	Delete(ctx context.Context, a authz.Actor, fileID uuid.UUID) error
}

// Contacts is identity's address lookup (L4; identity.Users.Contact): the
// e-mail of level 3 contact sharing (04 §V.3), read only when the student
// turned show_contact_on_grant on and the contact_info_share consent is in
// force.
type Contacts interface {
	// Contact returns the address of a live user; identity.ErrNotFound for
	// an unknown or erased one.
	Contact(ctx context.Context, id uuid.UUID) (identity.UserContact, error)
}

// OrgNames is orgs' org directory (L5; orgs.OrgDirectory.GetMany): the
// names of the companies in a student's grant list and of the clubs of a
// level 3 view.
type OrgNames interface {
	// GetMany returns the summaries of the ids that exist.
	GetMany(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]orgs.OrgSummary, error)
}

// SkillNamer names catalog skills for the company views (level 2 and 3:
// skills[].name). directory does not offer it yet; cmd/koro binds it when
// the directory service implements the method (nil → empty names; the
// public listSkills names every skill ID).
type SkillNamer interface {
	SkillNames(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error)
}
