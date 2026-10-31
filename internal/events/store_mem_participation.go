package events

import (
	"context"
	"slices"
	"time"
	"uuid"
)

// EventsOf implements Store.
func (s *MemStore) EventsOf(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := make(map[uuid.UUID]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	out := make(map[uuid.UUID]uuid.UUID, len(ids))
	for _, r := range s.regs {
		if want[r.ID] {
			out[r.ID] = r.EventID
		}
	}
	for _, c := range s.checkins {
		if want[c.ID] {
			out[c.ID] = c.EventID
		}
	}
	return out, nil
}

// ParticipationFacts implements Store.
func (s *MemStore) ParticipationFacts(_ context.Context, eventIDs []uuid.UUID) (map[uuid.UUID]ParticipationFacts, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[uuid.UUID]ParticipationFacts, len(eventIDs))
	for _, id := range eventIDs {
		e, ok := s.events[id]
		if !ok {
			continue
		}
		f, in := ParticipationFacts{OrgID: e.OrgID}, map[uuid.UUID]bool{}
		for _, r := range s.regs {
			if r.EventID != id || r.OrgID != e.OrgID || !heldPlace(r.Status) {
				continue
			}
			f.Confirmed++
			if r.Status == RegCheckedIn {
				in[r.UserID] = true
			}
		}
		f.DistinctCheckins = len(in)
		out[id] = f
	}
	return out, nil
}

// Undone implements Store.
func (s *MemStore) Undone(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := make(map[uuid.UUID]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	out := make(map[uuid.UUID]bool, len(ids))
	for _, r := range s.regs {
		if want[r.ID] && r.Status == RegCancelled {
			out[r.ID] = true
		}
	}
	for _, c := range s.checkins {
		if want[c.ID] && c.VoidedAt != nil {
			out[c.ID] = true
		}
	}
	return out, nil
}

// TargetableParticipations implements Store.
func (s *MemStore) TargetableParticipations(_ context.Context, userIDs []uuid.UUID, since time.Time) ([]TargetableRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []TargetableRow{}
	for _, r := range s.regs {
		e, ok := s.events[r.EventID]
		if !ok || e.OrgID != r.OrgID || !slices.Contains(userIDs, r.UserID) || !heldPlace(r.Status) ||
			(e.Status != StatusPublished && e.Status != StatusCompleted) || e.Visibility != VisibilityPublic ||
			e.IsSensitiveCategory || e.StartsAt.Before(since) {
			continue
		}
		out = append(out, TargetableRow{UserID: r.UserID, EventID: e.ID, OrgID: e.OrgID, Attended: r.Status == RegCheckedIn})
	}
	slices.SortFunc(out, func(a, b TargetableRow) int {
		if c := a.UserID.Compare(b.UserID); c != 0 {
			return c
		}
		if c := s.events[b.EventID].StartsAt.Compare(s.events[a.EventID].StartsAt); c != 0 {
			return c
		}
		return a.EventID.Compare(b.EventID)
	})
	return out, nil
}
