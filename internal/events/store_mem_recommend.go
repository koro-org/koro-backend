package events

import (
	"cmp"
	"context"
	"slices"
	"time"
	"uuid"
)

// RecommendSignals implements RecommendStore.
func (s *MemStore) RecommendSignals(_ context.Context, userID uuid.UUID, since time.Time) ([]SignalRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	type key struct {
		cat Category
		org uuid.UUID
	}
	counts := map[key]int{}
	for _, r := range s.regs {
		e, ok := s.events[r.EventID]
		if !ok || e.OrgID != r.OrgID || r.UserID != userID || (r.Status != RegConfirmed && r.Status != RegCheckedIn) ||
			(e.Status != StatusPublished && e.Status != StatusCompleted) || e.IsSensitiveCategory || e.StartsAt.Before(since) {
			continue
		}
		counts[key{e.Category, e.OrgID}]++
	}
	out := make([]SignalRow, 0, len(counts))
	for k, n := range counts {
		out = append(out, SignalRow{Category: k.cat, OrgID: k.org, Events: n})
	}
	slices.SortFunc(out, func(a, b SignalRow) int {
		return cmp.Or(cmp.Compare(b.Events, a.Events), cmp.Compare(a.Category, b.Category), compareUUID(a.OrgID, b.OrgID))
	})
	return out[:min(len(out), maxSignalRows)], nil
}

// RecommendableEvents implements RecommendStore.
func (s *MemStore) RecommendableEvents(_ context.Context, f RecommendFilter, limit int) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Event{}
	if (len(f.OrgIDs) == 0 && len(f.Categories) == 0 && f.MinConfirmed <= 0) || limit <= 0 {
		return out, nil
	}
	for _, e := range s.events {
		match := slices.Contains(f.OrgIDs, e.OrgID) || slices.Contains(f.Categories, e.Category) ||
			(f.MinConfirmed > 0 && e.ConfirmedCount >= f.MinConfirmed)
		if e.Status != StatusPublished || e.IsSensitiveCategory || e.StartsAt.Before(f.From) || !e.StartsAt.Before(f.To) ||
			!match || !s.visibleTo(f.Viewer, e) || s.registered(e.ID, f.UserID) {
			continue
		}
		out = append(out, e)
	}
	sortByStart(out)
	return limitOf(out, limit), nil
}

// registered reports whether userID has a registration (any status) of
// eventID. The caller holds s.mu.
func (s *MemStore) registered(eventID, userID uuid.UUID) bool {
	for _, r := range s.regs {
		if r.EventID == eventID && r.UserID == userID {
			return true
		}
	}
	return false
}
