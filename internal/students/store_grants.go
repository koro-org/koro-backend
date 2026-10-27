package students

import (
	"context"
	"time"
	"uuid"
)

// grantStore is the level 3 access grants, profile_access_grants (02 §9.7).
// Org scope: company_org_id; a grant is addressed by its student too
// (IDOR). A grant is open until it is
// revoked; an open grant is active unless it is suspended (BE-077).
type grantStore interface {
	// grantContacts is the per-company contact sharing of the grants
	// (store_contact.go; BE-089).
	grantContacts

	// LockGrants serialises the grant writes of studentID for the rest of
	// the transaction (an advisory lock: no row is created).
	LockGrants(ctx context.Context, studentID uuid.UUID) error
	// InsertGrant writes a new active grant and returns it as stored; false
	// and nothing written when its source already has a grant (ON CONFLICT
	// (source, source_id) DO NOTHING keeps the transaction usable).
	InsertGrant(ctx context.Context, g AccessGrant) (AccessGrant, bool, error)
	// GrantBySource returns the grant of (source, sourceID) or ErrNotFound.
	GrantBySource(ctx context.Context, source GrantSource, sourceID uuid.UUID) (AccessGrant, error)
	// ActiveGrant returns the oldest active (open, not suspended) grant of
	// companyOrgID on studentID or ErrNotFound.
	ActiveGrant(ctx context.Context, studentID, companyOrgID uuid.UUID) (AccessGrant, error)
	// GetGrant returns studentID's grant id (whatever its state) or
	// ErrNotFound (also when id belongs to another student).
	GetGrant(ctx context.Context, studentID, id uuid.UUID) (AccessGrant, error)
	// RevokeGrants revokes every open grant (active or suspended) of
	// companyOrgID on studentID and returns the revoked rows (none: an
	// empty slice).
	RevokeGrants(ctx context.Context, studentID, companyOrgID uuid.UUID, by RevokedBy, reason *string, at time.Time) ([]AccessGrant, error)
	// SuspendGrants suspends, at at, every active grant of studentID
	// granted at or before at (and not resumed after at: a late task never
	// undoes a resume, BE-088) and returns the suspended rows (none: an
	// empty slice). A suspended grant is never made active again but by
	// ResumeGrant.
	SuspendGrants(ctx context.Context, studentID uuid.UUID, at time.Time) ([]AccessGrant, error)
	// ResumeGrant makes studentID's suspended open grant id active again at
	// at (suspended_at cleared, resumed_at = at) and returns it, or
	// ErrNotFound when id is not studentID's or not suspended and open
	// (BE-088).
	ResumeGrant(ctx context.Context, studentID, id uuid.UUID, at time.Time) (AccessGrant, error)
	// SuspendedStudentsBefore returns up to limit students with user_id >
	// after, in user_id order, holding an open grant suspended before
	// cutoff (the expiry walk, BE-088).
	SuspendedStudentsBefore(ctx context.Context, cutoff time.Time, after uuid.UUID, limit int) ([]uuid.UUID, error)
	// ExpireSuspendedGrants revokes at at, by the system with
	// ReasonSuspensionExpired, every open grant of studentID suspended
	// before cutoff and returns the revoked rows (none: an empty slice).
	ExpireSuspendedGrants(ctx context.Context, studentID uuid.UUID, cutoff, at time.Time) ([]AccessGrant, error)
	// ExpiredRevocations returns up to limit distinct (student, revoked_at)
	// pairs of the grants the system revoked with ReasonSuspensionExpired
	// at or after since, after the position (nil: from the start), in
	// (student_user_id, revoked_at) order (the expiry notices, BE-088).
	ExpiredRevocations(ctx context.Context, since time.Time, after *ExpiryKey, limit int) ([]ExpiryKey, error)
	// CountSuspendedAt counts the open grants of studentID suspended at
	// exactly at (one lapse's suspension, also after a retry).
	CountSuspendedAt(ctx context.Context, studentID uuid.UUID, at time.Time) (int, error)
	// StudentGrants returns up to limit open grants (active and suspended)
	// of studentID after the position (nil: from the start), newest first.
	StudentGrants(ctx context.Context, studentID uuid.UUID, after *GrantKey, limit int) ([]AccessGrant, error)
	// CompanyGrants returns up to limit active grants of companyOrgID
	// after the position (nil: from the start), newest first.
	CompanyGrants(ctx context.Context, companyOrgID uuid.UUID, after *GrantKey, limit int) ([]AccessGrant, error)
	// AllGrants returns every grant of studentID, revoked ones included,
	// oldest first (the KVKK export).
	AllGrants(ctx context.Context, studentID uuid.UUID) ([]AccessGrant, error)
	// DeleteGrants deletes every grant of studentID and returns how many
	// went (account erasure).
	DeleteGrants(ctx context.Context, studentID uuid.UUID) (int, error)
	// CountRevokedGrantsBefore counts the grants revoked before cutoff
	// (the retention dry run).
	CountRevokedGrantsBefore(ctx context.Context, cutoff time.Time) (int, error)
	// PurgeRevokedGrants deletes up to limit grants revoked before cutoff,
	// oldest revocation first, and returns how many went.
	PurgeRevokedGrants(ctx context.Context, cutoff time.Time, limit int) (int, error)
}
