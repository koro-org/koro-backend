package students

import (
	"context"
	"uuid"
)

// recommendStore is the aggregate behind the recommendations' popular
// clubs (BE-174): a count over the audience keys of student_profiles that
// returns club IDs only, never a profile or a count.
type recommendStore interface {
	// PopularClubs returns up to limit club org IDs that at least min
	// audience eligible profiles holding the audience key dim (uni:<id> or
	// dept:<id>) hold the club key of, by that number descending, then by
	// club key. The stored club keys never name a sensitive club.
	PopularClubs(ctx context.Context, dim string, min, limit int) ([]uuid.UUID, error)
}
