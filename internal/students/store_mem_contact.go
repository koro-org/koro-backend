package students

import (
	"context"
	"time"
	"uuid"
)

// ShareGrantContact implements Store.
func (s *MemStore) ShareGrantContact(_ context.Context, id uuid.UUID, at time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, g := range s.grants {
		if g.ID != id || !g.Open() || g.ContactSharedAt != nil {
			continue
		}
		g.ContactSharedAt = &at
		if err := checkGrant(g); err != nil {
			return false, err
		}
		g.UpdatedAt = s.stamp()
		s.grants[i] = g
		return true, nil
	}
	return false, nil
}

// UnshareGrantContact implements Store.
func (s *MemStore) UnshareGrantContact(_ context.Context, studentID, id uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, g := range s.grants {
		if g.ID == id && g.StudentUserID == studentID && g.ContactSharedAt != nil {
			g.ContactSharedAt, g.UpdatedAt = nil, s.stamp()
			s.grants[i] = g
			return true, nil
		}
	}
	return false, nil
}

// ClearGrantContacts implements Store.
func (s *MemStore) ClearGrantContacts(_ context.Context, studentID uuid.UUID, sharedBefore *time.Time) ([]AccessGrant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []AccessGrant
	for i, g := range s.grants {
		if g.StudentUserID != studentID || g.ContactSharedAt == nil ||
			(sharedBefore != nil && g.ContactSharedAt.After(*sharedBefore)) {
			continue
		}
		out = append(out, g)
		g.ContactSharedAt, g.UpdatedAt = nil, s.stamp()
		s.grants[i] = g
	}
	return out, nil
}
