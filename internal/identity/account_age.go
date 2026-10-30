package identity

import (
	"context"
	"slices"
	"time"
	"uuid"
)

// AccountAges is the account age read of other modules (BE-085): talent's
// like threshold min_account_age_days_for_likes (02 §15.1, R-003 §3.4:
// only an account at least that old earns points with a like). It returns
// the creation instant of every account among userIDs that exists
// (tombstones included); nothing else of the account leaves.
type AccountAges interface {
	AccountAge(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]time.Time, error)
}

// accountAgeBatch bounds the IDs of one store read.
const accountAgeBatch = 1000

var _ AccountAges = (*service)(nil)

// AccountAge implements AccountAges.
func (s *service) AccountAge(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]time.Time, error) {
	out := make(map[uuid.UUID]time.Time, len(userIDs))
	for batch := range slices.Chunk(userIDs, accountAgeBatch) {
		us, err := s.users().Users(ctx, batch)
		if err != nil {
			return nil, err
		}
		for _, u := range us {
			out[u.ID] = u.CreatedAt.UTC()
		}
	}
	return out, nil
}
