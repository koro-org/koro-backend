package students

import (
	"context"
	"time"
	"uuid"

	"koro/internal/platform/civil"
)

// Store is the persistence boundary of students (05 §2.2) over
// student_profiles, student_privacy_settings, student_skills,
// student_experiences, student_languages, student_cvs and
// profile_access_grants (02 §9.1–9.7) and the count_segment function
// (02 §9.1, 01 §1.4). The GORM store
// (store_gorm.go) and the in-memory store (store_mem.go) pass the same
// contract test (store_contract_test.go).
//
// Every method works in the transaction carried by ctx, if any. Org scope:
// none (02 §9: a student belongs to no org) but for the access grants
// (company_org_id); every per-user table is
// addressed by user_id, and an experience is read and written only
// together with its owner's user_id (IDOR: another user's ID is not found).
type Store interface {
	// grantContacts is the per-company contact sharing of the level 3
	// grants (store_contact.go; BE-089).
	grantContacts

	// GetProfile returns the profile of userID or ErrNotFound.
	GetProfile(ctx context.Context, userID uuid.UUID) (Profile, error)
	// LockProfile returns the profile locked for the rest of the
	// transaction (SELECT … FOR UPDATE) or ErrNotFound.
	LockProfile(ctx context.Context, userID uuid.UUID) (Profile, error)
	// InsertProfile writes a new profile and returns it as stored; false
	// and nothing written when the user already has one (ON CONFLICT DO
	// NOTHING keeps the transaction usable).
	InsertProfile(ctx context.Context, p Profile) (Profile, bool, error)
	// UpdateProfile writes every mutable column of p (all but user_id and
	// created_at; the audience columns too) and returns the stored row, or
	// ErrNotFound.
	UpdateProfile(ctx context.Context, p Profile) (Profile, error)
	// SetAudience writes the audience columns of userID.
	SetAudience(ctx context.Context, userID uuid.UUID, keys []string, eligible bool, at time.Time) error
	// RemoveAudienceKey removes key from up to limit profiles holding it
	// and returns how many changed.
	RemoveAudienceKey(ctx context.Context, key string, at time.Time, limit int) (int, error)
	// Profiles returns the profiles of the users among ids (any order).
	Profiles(ctx context.Context, ids []uuid.UUID) ([]Profile, error)
	// ProfilesAfter returns up to limit profiles with user_id > after, in
	// user_id order (keyset walk of the nightly jobs).
	ProfilesAfter(ctx context.Context, after uuid.UUID, limit int) ([]Profile, error)
	// GraduationCandidates returns up to limit active profiles whose
	// expected graduation is before today, with user_id > after, in
	// user_id order.
	GraduationCandidates(ctx context.Context, today civil.Date, after uuid.UUID, limit int) ([]Profile, error)
	// GraduatedAmong returns the users among ids with a graduated profile.
	GraduatedAmong(ctx context.Context, ids []uuid.UUID) ([]uuid.UUID, error)
	// AvatarIDs returns the avatar file of the users among ids that have one.
	AvatarIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]uuid.UUID, error)
	// DeleteProfile deletes the profile of userID; false when there was none.
	DeleteProfile(ctx context.Context, userID uuid.UUID) (bool, error)

	// GetPrivacy returns the settings row of userID or ErrNotFound.
	GetPrivacy(ctx context.Context, userID uuid.UUID) (Privacy, error)
	// LockPrivacy returns the settings row of userID locked for the rest of
	// the transaction, inserting the default row first when there is none.
	LockPrivacy(ctx context.Context, userID uuid.UUID) (Privacy, error)
	// UpdatePrivacy writes the four settings of p and returns the stored
	// row, or ErrNotFound.
	UpdatePrivacy(ctx context.Context, p Privacy) (Privacy, error)
	// PrivacyFlagged returns up to limit settings rows with discoverable or
	// show_contact_on_grant on, with user_id > after, in user_id order.
	PrivacyFlagged(ctx context.Context, after uuid.UUID, limit int) ([]Privacy, error)
	// DeletePrivacy deletes the settings row of userID; false when none.
	DeletePrivacy(ctx context.Context, userID uuid.UUID) (bool, error)

	// Skills returns the skills of userID in created_at, skill_id order.
	Skills(ctx context.Context, userID uuid.UUID) ([]Skill, error)
	// ReplaceSkills makes items the skills of userID (created_at of a kept
	// skill does not change).
	ReplaceSkills(ctx context.Context, userID uuid.UUID, items []Skill) ([]Skill, error)
	// Languages returns the languages of userID in created_at, code order.
	Languages(ctx context.Context, userID uuid.UUID) ([]Language, error)
	// ReplaceLanguages makes items the languages of userID.
	ReplaceLanguages(ctx context.Context, userID uuid.UUID, items []Language) ([]Language, error)

	// Experiences returns the experiences of userID, newest start first.
	Experiences(ctx context.Context, userID uuid.UUID) ([]Experience, error)
	// GetExperience returns userID's experience id, or ErrNotFound (also
	// when id belongs to another user).
	GetExperience(ctx context.Context, userID, id uuid.UUID) (Experience, error)
	// InsertExperience writes a new experience and returns it as stored.
	InsertExperience(ctx context.Context, e Experience) (Experience, error)
	// UpdateExperience writes the mutable columns of userID's experience
	// e.ID and returns the stored row, or ErrNotFound.
	UpdateExperience(ctx context.Context, e Experience) (Experience, error)
	// DeleteExperience deletes userID's experience id; false when it is
	// not there (or not userID's).
	DeleteExperience(ctx context.Context, userID, id uuid.UUID) (bool, error)
	// DeleteSections deletes every skill, experience and language of
	// userID and returns how many rows went.
	DeleteSections(ctx context.Context, userID uuid.UUID) (int, error)

	// CVs returns the CVs of userID, oldest first (created_at, id).
	CVs(ctx context.Context, userID uuid.UUID) ([]CV, error)
	// GetCV returns userID's CV id, or ErrNotFound (also when id belongs
	// to another user: IDOR).
	GetCV(ctx context.Context, userID, id uuid.UUID) (CV, error)
	// CVByFile returns the CV holding fileID or ErrNotFound (file.rejected).
	CVByFile(ctx context.Context, fileID uuid.UUID) (CV, error)
	// InsertCV writes a new CV (not current) and returns it as stored. A
	// file already used by a CV is a conflict (student_cvs_file_id_uq).
	InsertCV(ctx context.Context, cv CV) (CV, error)
	// SetCurrentCV makes userID's CV id the only current one and returns
	// it, or ErrNotFound when id is not userID's.
	SetCurrentCV(ctx context.Context, userID, id uuid.UUID) (CV, error)
	// DeleteCV deletes userID's CV id; false when it is not there (or not
	// userID's).
	DeleteCV(ctx context.Context, userID, id uuid.UUID) (bool, error)

	// CountSegment is count_segment(keys, dims) (02 §9.1): the number of
	// audience eligible profiles matching keys (OR inside a dimension,
	// AND across dims dimensions); ok=false when the number is below 10
	// (the function's NULL: the real number never leaves the database).
	CountSegment(ctx context.Context, keys []string, dims int16) (n int, ok bool, err error)
	// AudienceAfter returns up to limit user IDs > after, in user_id
	// order, of the audience eligible profiles count_segment would count
	// (keyset walk of Audience.Stream).
	AudienceAfter(ctx context.Context, keys []string, dims int16, after uuid.UUID, limit int) ([]uuid.UUID, error)

	// DiscoveryCandidates returns up to limit user IDs > after, in user_id
	// order, of the active profiles whose settings have discoverable on
	// and that match q's filters (q.After and q.Limit are ignored). The
	// caller still checks the facts other modules hold (verification, age,
	// account, the consent in force) before anyone sees a candidate.
	DiscoveryCandidates(ctx context.Context, q DiscoveryQuery, after uuid.UUID, limit int) ([]uuid.UUID, error)

	// Level 3 access grants (02 §9.7). Org scope: company_org_id; a grant
	// is addressed by its student too (IDOR). A grant is open until it is
	// revoked; an open grant is active unless it is suspended (BE-077).

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
