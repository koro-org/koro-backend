package events

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
	"koro/internal/notify"
	"koro/internal/orgs"
	"koro/internal/platform/privacy"
	"koro/internal/students"
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
	// ActiveMemberIDs returns the users with an active membership of orgID
	// (the managers notified of a new application, 01 §6.2
	// registration.submitted).
	ActiveMemberIDs(ctx context.Context, orgID uuid.UUID) ([]uuid.UUID, error)
	// HasRole reports whether userID holds one of roles in orgID.
	HasRole(ctx context.Context, orgID, userID uuid.UUID, roles ...authz.MemberRole) (bool, error)
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
	// GetDepartment returns a department or directory.ErrNotFound (the
	// registrant's department on RegistrationOrgDTO, 04 §V.6).
	GetDepartment(ctx context.Context, id uuid.UUID) (directory.Department, error)
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
// cover_file_id, purpose event_cover) and the registration exports (02
// §11.9 file_id, purpose event_export; BE-050).
type Files interface {
	// Get returns the file's metadata or files.ErrNotFound.
	Get(ctx context.Context, fileID uuid.UUID) (files.FileMeta, error)
	// Attach binds the cover (the export file) to the event (the export) in
	// the caller's transaction.
	//
	// sync-write: files.Files.Attach
	Attach(ctx context.Context, a authz.Actor, fileID uuid.UUID, owner files.Owner) error
	// PresignGet returns a short-lived download URL of a clean file.
	PresignGet(ctx context.Context, fileID uuid.UUID, ttl time.Duration) (files.Download, error)
	// PutGenerated stores a server-generated file (the export; outside a
	// transaction: object storage I/O).
	PutGenerated(ctx context.Context, in files.GeneratedFile) (uuid.UUID, error)
	// Delete removes an export file in the transaction that clears its
	// reference (events.purge_exports after 7 days; a build that lost its
	// row), so the file never outlives its reference nor the reference its
	// file (01 §6.3, T-193, 06 H-49; the objects go after the commit).
	//
	// sync-write: files.Files.Delete
	Delete(ctx context.Context, a authz.Actor, fileID uuid.UUID) error
}

// Reauthenticator verifies the re-authentication proof of the R operation
// createEventExport (03 §6.2; identity.Service.Verify). A refused proof is
// a 4xx problem (identity.reauth_required, identity.reauth_failed, 429);
// an overloaded server answers the bare request deadline (BE-070).
type Reauthenticator interface {
	Verify(ctx context.Context, userID uuid.UUID, proof identity.ReauthProof) (kind string, err error)
}

// Notices is what events needs from consent (L2; 01 §4.3 Notices): the
// current consent text of each consent field of a form, shown with the
// form (getEventForm consent_notices) and required to publish it. It is
// consent.Service.
type Notices interface {
	// Current returns the published version for key (fallback: audience
	// "all", locale tr-TR) or consent.ErrNoNotice.
	Current(ctx context.Context, key consent.NoticeKey) (consent.Notice, error)
}

// Recorder is what events needs from consent (L2; 01 §6.3 "events form
// rıza alanı → consent.Recorder.Record"): a consent field of the form
// answered yes is recorded in the registration's transaction, scoped to
// the registration (event_sponsor_share is a per-registration purpose).
// It is consent.Service.
type Recorder interface {
	// Record appends one consent row in the transaction carried by ctx.
	//
	// sync-write: consent.Recorder.Record
	Record(ctx context.Context, in consent.RecordInput) (uuid.UUID, error)
}

// Sealer seals and opens the special-category answers of a registration
// (02 §11.6 answers_sensitive_enc; 02 §0.4 envelope encryption). It is the
// shared *crypto.Envelope (OpenBao Transit or the local KEK, T-82).
type Sealer interface {
	Seal(ctx context.Context, purpose string, plaintext []byte) ([]byte, error)
	Open(ctx context.Context, purpose string, sealed []byte) ([]byte, error)
}

// TicketKeys derives ticket tokens (02 §11.7, R-003 §2.5.4) with rotating
// keys (CRYPTO_TOKEN_KEYS). It is *crypto.Deriver.
type TicketKeys interface {
	// ActiveKeyID is the key of new tickets.
	ActiveKeyID() string
	// DerivedToken is HMAC-SHA256 of the parts under (keyID, purpose).
	DerivedToken(purpose, keyID string, parts ...[]byte) ([]byte, error)
}

// Sender is what events needs from notify (L3): the in-app notices of the
// registration events and the ticket e-mails (BE-031).
type Sender interface {
	Send(ctx context.Context, m notify.Message) (uuid.UUID, error)
}

// Students is what events needs from students (L7; 01 §4.9) for the
// registrant card of the organiser's list (04 §V.6): the university,
// department and class year of a student and the avatar.
type Students interface {
	// AudienceProfile returns the student view of userID or ErrNotFound.
	AudienceProfile(ctx context.Context, userID uuid.UUID) (students.AudienceProfile, error)
	// AvatarURLs returns short-lived avatar URLs of the users that have one.
	AvatarURLs(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]string, error)
}

// Contacts is what events needs from identity (L4; 01 §4.6 Users.Contact)
// for the ticket e-mails: the registrant's address, first name and locale,
// read in the worker only (01 §5.3: notify never resolves a recipient). It
// is identity.Service.
type Contacts interface {
	// Contact returns the address of a live user or identity.ErrNotFound
	// (unknown or erased: nothing is sent).
	Contact(ctx context.Context, id uuid.UUID) (identity.UserContact, error)
}

// Destructions is what events needs from kvkk (L13, late bound through
// hooks.destruction; privacy.DestructionRecorder): the destruction record
// of an export file events.purge_exports deletes at its expiry (T-318,
// BE-083; 02 §16.3), so the file's deletion is recorded where every other
// destruction is.
type Destructions interface {
	// Record appends one destruction_logs row in the transaction that
	// deletes the file and marks the export purged (01 §6.3), so the
	// destruction commits with its record or not at all.
	//
	// sync-write: kvkk.DestructionRecorder.Record
	Record(ctx context.Context, d privacy.Destruction) error
}
