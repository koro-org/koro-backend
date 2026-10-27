package students

import (
	"context"
	"time"
	"uuid"

	"koro/internal/platform/civil"
)

// profileStore is the student_profiles rows (02 §9.1) with their derived
// audience columns: the profile, the nightly walks over it and the reads
// other modules' facts need (graduation, avatars).
type profileStore interface {
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
}
