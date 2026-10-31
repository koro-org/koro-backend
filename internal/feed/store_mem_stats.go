package feed

import (
	"context"
	"time"
	"uuid"
)

// ContentStats implements statsStore.
func (s *MemStore) ContentStats(_ context.Context, from, to time.Time) (ContentStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := newContentStats()
	for _, p := range s.posts {
		st.PostsByStatus[p.Status]++
		if p.PublishedAt != nil && statsInRange(*p.PublishedAt, from, to) {
			st.PublishedInRange++
		}
	}
	for _, r := range s.reports {
		st.ReportsByStatus[r.Status]++
		if statsInRange(r.CreatedAt, from, to) {
			st.ReportsInRange++
		}
	}
	return st, nil
}

// OrgPostStats implements statsStore.
func (s *MemStore) OrgPostStats(_ context.Context, orgID uuid.UUID, from, to time.Time) (OrgPostStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var st OrgPostStats
	for _, p := range s.posts {
		if p.OrgID != orgID || p.PublishedAt == nil || !statsInRange(*p.PublishedAt, from, to) {
			continue
		}
		st.Published++
		if p.AudienceKind == AudienceSegment {
			st.Targeted++
		}
		st.Likes += p.LikeCount
		st.Saves += p.SaveCount
	}
	return st, nil
}

// statsInRange reports whether t is in [from, to).
func statsInRange(t, from, to time.Time) bool { return !t.Before(from) && t.Before(to) }
