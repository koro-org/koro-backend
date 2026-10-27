package students

import (
	"context"
	"uuid"
)

// sectionStore is the profile sections: student_skills, student_languages
// and student_experiences. Every row is addressed together with its
// owner's user_id (IDOR: another user's ID is not found).
type sectionStore interface {
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
}
