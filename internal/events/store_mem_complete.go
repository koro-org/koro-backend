package events

import (
	"cmp"
	"context"
	"slices"
	"time"
)

// CompleteEnded implements Store.
func (s *MemStore) CompleteEnded(_ context.Context, endedBy time.Time, limit int) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var due []Event
	for _, e := range s.events {
		if e.Status == StatusPublished && !e.EndsAt.After(endedBy) {
			due = append(due, e)
		}
	}
	slices.SortFunc(due, func(a, b Event) int { return cmp.Or(a.EndsAt.Compare(b.EndsAt), compareUUID(a.ID, b.ID)) })
	out := []Event{}
	for _, e := range due {
		if len(out) == limit {
			break
		}
		e.Status, e.UpdatedAt = StatusCompleted, s.stamp(e.UpdatedAt)
		s.events[e.ID] = e
		e.DescriptionJSON = cloneJSON(e.DescriptionJSON)
		out = append(out, e)
	}
	return out, nil
}
