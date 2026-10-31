package outreach

import (
	"cmp"
	"context"
	"slices"
	"time"
	"uuid"
)

// OutreachStats implements statsStore.
func (m *MemStore) OutreachStats(_ context.Context, orgID uuid.UUID, from, to time.Time, limit int) (StatsCounts, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := StatsCounts{ByStatus: map[DispatchStatus]int{}}
	for _, s := range m.rows {
		if s.OrgID == orgID && !s.Archived() {
			c.Segments = append(c.Segments, s)
		}
	}
	c.Active = len(c.Segments)
	slices.SortFunc(c.Segments, func(a, b Segment) int {
		switch {
		case a.LastCount == nil && b.LastCount != nil:
			return 1
		case a.LastCount != nil && b.LastCount == nil:
			return -1
		case a.LastCount != nil && *a.LastCount != *b.LastCount:
			return cmp.Compare(*b.LastCount, *a.LastCount)
		}
		return cmp.Compare(a.ID.String(), b.ID.String())
	})
	if len(c.Segments) > limit {
		c.Segments = c.Segments[:limit]
	}
	for _, d := range m.dispatches {
		if d.OrgID != orgID || d.CreatedAt.Before(from) || !d.CreatedAt.Before(to) {
			continue
		}
		c.ByStatus[d.Status]++
		if d.EligibleCount != nil {
			c.Eligible += *d.EligibleCount
		}
		if d.SentCount != nil {
			c.Sent += *d.SentCount
		}
	}
	return c, nil
}
