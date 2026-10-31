package orders

import (
	"context"
	"fmt"
	"time"
	"uuid"
)

// Statistics is the sales read of a seller's panel (stats, BE-163; SR §8
// "sponsor talepleri"): the orders that companies placed on the seller's
// offerings, counts only. It is a system read without authorisation: the
// caller authorises its own read (stats.club.read).
type Statistics interface {
	// SalesStats returns the orders placed in [from, to) that hold at
	// least one line of sellerOrgID.
	SalesStats(ctx context.Context, sellerOrgID uuid.UUID, from, to time.Time) (SalesStats, error)
}

// SalesStats are a seller's orders of a range: every order counts once,
// by its status now.
type SalesStats struct {
	Orders   int
	ByStatus map[Status]int
}

// StatsStore is the persistence of Statistics, part of Store. The read is
// keyed by the seller of the caller's path (order_lines_seller_idx).
type StatsStore interface {
	// SalesStats returns the seller's orders placed in [from, to).
	SalesStats(ctx context.Context, sellerOrgID uuid.UUID, from, to time.Time) (SalesStats, error)
}

var _ Statistics = (*service)(nil)

// SalesStats implements Statistics.
func (s *service) SalesStats(ctx context.Context, sellerOrgID uuid.UUID, from, to time.Time) (SalesStats, error) {
	st, err := s.d.Store.SalesStats(ctx, sellerOrgID, from, to)
	if err != nil {
		return SalesStats{}, fmt.Errorf("orders: sales stats: %w", err)
	}
	return st, nil
}
