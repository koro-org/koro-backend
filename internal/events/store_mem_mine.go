package events

import (
	"context"
	"slices"
	"uuid"
)

// ListStaffEvents implements Store.
func (s *MemStore) ListStaffEvents(_ context.Context, f StaffEventFilter, after *After, limit int) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Event
	for _, e := range s.events {
		if e.Status != StatusPublished || !e.EndsAt.After(f.EndsAfter) {
			continue
		}
		staff := slices.ContainsFunc(s.staff, func(x Staff) bool { return x.EventID == e.ID && x.UserID == f.UserID })
		if staff || slices.Contains(f.OrgIDs, e.OrgID) {
			out = append(out, e)
		}
	}
	sortByStart(out)
	return limitOf(afterStart(out, after), limit), nil
}

// StaffRolesOf implements Store.
func (s *MemStore) StaffRolesOf(_ context.Context, userID uuid.UUID, eventIDs []uuid.UUID) (map[uuid.UUID]StaffRole, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[uuid.UUID]StaffRole{}
	for _, x := range s.staff {
		if x.UserID == userID && slices.Contains(eventIDs, x.EventID) {
			out[x.EventID] = x.Role
		}
	}
	return out, nil
}
