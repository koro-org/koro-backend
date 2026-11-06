package feed

import (
	"cmp"
	"context"
	"slices"
	"time"
	"uuid"

	"koro/internal/platform/problem"
)

// DropLogo implements Store (the file_id key of org_dropped_logos). The
// in-memory store knows no files: the ON DELETE CASCADE of a deleted file is
// the GORM store's.
func (s *MemStore) DropLogo(_ context.Context, l DroppedLogo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, taken := s.logos[l.FileID]; taken {
		return problem.ErrConflict
	}
	l.DroppedAt = l.DroppedAt.UTC().Truncate(time.Microsecond)
	s.logos[l.FileID] = l
	return nil
}

// DroppedLogosOf implements Store.
func (s *MemStore) DroppedLogosOf(_ context.Context, orgID uuid.UUID) ([]DroppedLogo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []DroppedLogo
	for _, l := range s.logos {
		if l.OrgID == orgID {
			out = append(out, l)
		}
	}
	slices.SortFunc(out, compareLogos)
	return out, nil
}

// ForgetDroppedLogos implements Store.
func (s *MemStore) ForgetDroppedLogos(_ context.Context, fileIDs []uuid.UUID) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, id := range fileIDs {
		if _, ok := s.logos[id]; ok {
			delete(s.logos, id)
			n++
		}
	}
	return n, nil
}

// compareLogos orders kept logos oldest first.
func compareLogos(a, b DroppedLogo) int {
	return cmp.Or(a.DroppedAt.Compare(b.DroppedAt), a.FileID.Compare(b.FileID))
}

// logoHeld is the NOT EXISTS of droppedLogoDueSQL.
func (s *MemStore) logoHeld(l DroppedLogo, cutoff time.Time) bool {
	for _, r := range s.reports {
		if r.TargetType == TargetOrg && r.TargetID == l.OrgID && !r.CreatedAt.After(l.DroppedAt) &&
			(r.Status.Unresolved() || r.ResolvedAt != nil && !r.ResolvedAt.Before(cutoff)) {
			return true
		}
	}
	return false
}

// logosDue is droppedLogoDueSQL of the GORM store.
func (s *MemStore) logosDue(cutoff time.Time) []DroppedLogo {
	var out []DroppedLogo
	for _, l := range s.logos {
		if l.DroppedAt.Before(cutoff) && !s.logoHeld(l, cutoff) {
			out = append(out, l)
		}
	}
	slices.SortFunc(out, compareLogos)
	return out
}

// DroppedLogosDue implements Store.
func (s *MemStore) DroppedLogosDue(_ context.Context, cutoff time.Time, limit int) ([]DroppedLogo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.logosDue(cutoff)
	return out[:min(len(out), max(limit, 0))], nil
}

// CountDroppedLogosDue implements Store.
func (s *MemStore) CountDroppedLogosDue(_ context.Context, cutoff time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.logosDue(cutoff)), nil
}
