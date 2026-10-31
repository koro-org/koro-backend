package orders

import (
	"context"
	"fmt"
	"time"
	"uuid"

	"koro/internal/platform/db"
)

// SalesStats implements StatsStore.
//
// cross-org: keyed by the seller (seller_org_id), like ListSales; the
// buyers' orders are counted, never read out.
func (s gormStore) SalesStats(ctx context.Context, sellerOrgID uuid.UUID, from, to time.Time) (SalesStats, error) {
	var rows []struct {
		Status string
		N      int
	}
	err := db.Conn(ctx, s.orm).Raw(`SELECT o.status, count(*) AS n FROM orders o
	  WHERE o.placed_at >= ? AND o.placed_at < ?
	    AND o.id IN (SELECT l.order_id FROM order_lines l WHERE l.seller_org_id = ?)
	  GROUP BY o.status`, from, to, sellerOrgID).Scan(&rows).Error
	if err != nil {
		return SalesStats{}, fmt.Errorf("orders: sales stats: %w", err)
	}
	st := SalesStats{ByStatus: map[Status]int{}}
	for _, r := range rows {
		st.ByStatus[Status(r.Status)] = r.N
		st.Orders += r.N
	}
	return st, nil
}
