package messaging

import (
	"context"
	"time"
	"uuid"
)

// ContactRequestStats implements StatsStore.
func (m *MemStore) ContactRequestStats(_ context.Context, orgID uuid.UUID, from, to, now time.Time) (ContactRequestStats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := ContactRequestStats{ByStatus: map[Status]int{}}
	for _, r := range m.requests {
		if r.OrgID != orgID || r.CreatedAt.Before(from) || !r.CreatedAt.Before(to) {
			continue
		}
		st.ByStatus[r.Effective(now)]++
		st.Sent++
	}
	return st, nil
}
