package orders

import (
	"context"
	"time"
	"uuid"
)

// SalesStats implements StatsStore.
func (m *MemStore) SalesStats(_ context.Context, sellerOrgID uuid.UUID, from, to time.Time) (SalesStats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sold := map[uuid.UUID]bool{}
	for _, l := range m.lines {
		if l.SellerOrgID == sellerOrgID {
			sold[l.OrderID] = true
		}
	}
	st := SalesStats{ByStatus: map[Status]int{}}
	for id := range sold {
		o, ok := m.orders[id]
		if !ok || o.PlacedAt.Before(from) || !o.PlacedAt.Before(to) {
			continue
		}
		st.ByStatus[o.Status]++
		st.Orders++
	}
	return st, nil
}
