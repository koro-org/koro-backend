package events

import (
	"cmp"
	"context"
	"slices"
	"time"
	"uuid"
)

// EventStats implements StatsStore.
func (s *MemStore) EventStats(_ context.Context, from, to, now time.Time) (EventStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var st EventStats
	held := map[uuid.UUID]bool{}
	for _, e := range s.events {
		inRange := !e.StartsAt.Before(from) && e.StartsAt.Before(to)
		switch {
		case e.Status == StatusPublished && e.StartsAt.After(now):
			st.Upcoming++
		}
		switch {
		case (e.Status == StatusPublished || e.Status == StatusCompleted) && inRange && !e.StartsAt.After(now):
			st.Held++
			held[e.ID] = true
		case e.Status == StatusCancelled && inRange:
			st.Cancelled++
		}
	}
	for _, r := range s.regs {
		if !held[r.EventID] {
			continue
		}
		if heldPlace(r.Status) {
			st.Registered++
		}
		if r.Status == RegCheckedIn {
			st.CheckedIn++
		}
	}
	return st, nil
}

// OrgEventFigures implements StatsStore.
func (s *MemStore) OrgEventFigures(_ context.Context, orgIDs []uuid.UUID, from, to time.Time, limit int) ([]EventFigures, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []EventFigures{}
	byID := map[uuid.UUID]int{}
	for _, e := range s.events {
		if !slices.Contains(orgIDs, e.OrgID) || e.Status == StatusDraft || e.StartsAt.Before(from) || !e.StartsAt.Before(to) {
			continue
		}
		byID[e.ID] = len(out)
		out = append(out, EventFigures{EventID: e.ID, OrgID: e.OrgID, Title: e.Title, Status: e.Status, StartsAt: e.StartsAt,
			EndsAt: e.EndsAt, Sensitive: e.IsSensitiveCategory})
	}
	for _, r := range s.regs {
		i, ok := byID[r.EventID]
		if !ok || r.OrgID != out[i].OrgID {
			continue
		}
		if heldPlace(r.Status) {
			out[i].Registered++
		}
		switch r.Status {
		case RegCheckedIn:
			out[i].CheckedIn++
		case RegNoShow:
			out[i].NoShow++
		}
	}
	slices.SortFunc(out, func(a, b EventFigures) int {
		return cmp.Or(b.StartsAt.Compare(a.StartsAt), cmp.Compare(b.EventID.String(), a.EventID.String()))
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// OrgEventTotals implements StatsStore.
func (s *MemStore) OrgEventTotals(_ context.Context, orgIDs []uuid.UUID, from, to, now time.Time) (map[uuid.UUID]EventTotals, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[uuid.UUID]EventTotals{}
	counted := map[uuid.UUID]bool{} // events that started: their places count
	for _, e := range s.events {
		if !slices.Contains(orgIDs, e.OrgID) || (e.Status != StatusPublished && e.Status != StatusCompleted) ||
			e.IsSensitiveCategory || e.StartsAt.Before(from) || !e.StartsAt.Before(to) {
			continue
		}
		t := out[e.OrgID]
		t.Events++
		out[e.OrgID] = t
		if !e.StartsAt.After(now) {
			counted[e.ID] = true
		}
	}
	for _, r := range s.regs {
		if counted[r.EventID] && heldPlace(r.Status) && s.events[r.EventID].OrgID == r.OrgID {
			t := out[r.OrgID]
			t.Registered++
			out[r.OrgID] = t
		}
	}
	return out, nil
}
