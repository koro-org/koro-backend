package catalog

import (
	"context"
	"fmt"
	"uuid"
)

// Statistics is the approval queue read of a university's panel (stats,
// BE-163; SR §8 "bekleyen talepler"): a count only. It is a system read
// without authorisation: the caller authorises its own read
// (stats.university.read) and resolves the university's clubs itself.
type Statistics interface {
	// PendingUniversityDecisions counts the offerings of sellerOrgIDs that
	// wait for a university decision (T-21: pending_approval,
	// requires_university_approval, university_approval_status pending).
	PendingUniversityDecisions(ctx context.Context, sellerOrgIDs []uuid.UUID) (int, error)
}

// StatsStore is the persistence of Statistics, part of Store.
type StatsStore interface {
	// CountUniversityQueue counts UniversityQueue's offerings of sellerOrgIDs.
	CountUniversityQueue(ctx context.Context, sellerOrgIDs []uuid.UUID) (int, error)
}

var _ Statistics = (*service)(nil)

// PendingUniversityDecisions implements Statistics.
func (s *service) PendingUniversityDecisions(ctx context.Context, sellerOrgIDs []uuid.UUID) (int, error) {
	if len(sellerOrgIDs) == 0 {
		return 0, nil
	}
	n, err := s.d.Store.CountUniversityQueue(ctx, sellerOrgIDs)
	if err != nil {
		return 0, fmt.Errorf("catalog: pending university decisions: %w", err)
	}
	return n, nil
}
