package students

import (
	"context"
	"time"
	"uuid"

	"koro/internal/platform/civil"
)

// Store is the persistence boundary of students (05 §2.2) over
// student_profiles, student_privacy_settings, student_skills,
// student_experiences, student_languages and student_cvs (02 §9.1–9.6)
// and the count_segment function (02 §9.1, 01 §1.4). The GORM store
// (store_gorm.go) and the in-memory store (store_mem.go) pass the same
// contract test (store_contract_test.go).
//
// Every method works in the transaction carried by ctx, if any. Org scope:
// none (02 §9: a student belongs to no org); every per-user table is
// addressed by user_id, and an experience is read and written only
// together with its owner's user_id (IDOR: another user's ID is not found).
type Store interface {
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
}
