package students

import (
	"context"
	"uuid"
)

// matchStore is the set queries over the profiles: count_segment and its
// eligible walk (Audience, segment.go) and the discovery candidates
// (discovery.go, discovery_index.go). None of them returns a profile.
type matchStore interface {
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
}
