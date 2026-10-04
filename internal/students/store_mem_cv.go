package students

import (
	"context"
	"slices"
	"strings"
	"uuid"

	"koro/internal/platform/problem"
)

// CVs implements Store.
func (s *MemStore) CVs(_ context.Context, userID uuid.UUID) ([]CV, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []CV
	for _, c := range s.cvs {
		if c.UserID == userID {
			out = append(out, c)
		}
	}
	slices.SortStableFunc(out, func(a, b CV) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return compareUUID(a.ID, b.ID)
	})
	return out, nil
}

// GetCV implements Store.
func (s *MemStore) GetCV(_ context.Context, userID, id uuid.UUID) (CV, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.cvs {
		if c.ID == id && c.UserID == userID {
			return c, nil
		}
	}
	return CV{}, ErrNotFound
}

// CVByFile implements Store.
func (s *MemStore) CVByFile(_ context.Context, fileID uuid.UUID) (CV, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.cvs {
		if c.FileID == fileID {
			return c, nil
		}
	}
	return CV{}, ErrNotFound
}

// InsertCV implements Store (student_cvs_file_id_uq, the primary key).
func (s *MemStore) InsertCV(_ context.Context, c CV) (CV, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if slices.ContainsFunc(s.cvs, func(x CV) bool { return x.ID == c.ID || x.FileID == c.FileID }) {
		return CV{}, problem.ErrConflict
	}
	c.IsCurrent, c.CreatedAt = false, s.stamp()
	s.cvs = append(s.cvs, c)
	return c, nil
}

// SetCurrentCV implements Store.
func (s *MemStore) SetCurrentCV(_ context.Context, userID, id uuid.UUID) (CV, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.cvs, func(x CV) bool { return x.ID == id && x.UserID == userID })
	if i < 0 {
		return CV{}, ErrNotFound
	}
	for j := range s.cvs {
		if s.cvs[j].UserID == userID {
			s.cvs[j].IsCurrent = j == i
		}
	}
	return s.cvs[i], nil
}

// DeleteCV implements Store.
func (s *MemStore) DeleteCV(_ context.Context, userID, id uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.cvs)
	s.cvs = slices.DeleteFunc(s.cvs, func(x CV) bool { return x.ID == id && x.UserID == userID })
	return len(s.cvs) < n, nil
}

// segmentMatch is count_segment's predicate on one profile (02 §9.1): the
// profile is eligible, holds at least one of keys, and its matching keys
// span exactly dims dimensions.
func segmentMatch(p Profile, keys []string, dims int16) bool {
	if !p.AudienceEligible {
		return false
	}
	seen := map[string]bool{}
	for _, k := range p.AudienceKeys {
		if slices.Contains(keys, k) {
			dim, _, _ := strings.Cut(k, ":")
			seen[dim] = true
		}
	}
	return len(seen) > 0 && len(seen) == int(dims)
}

// CountSegment implements Store with count_segment's semantics (below 10:
// no number).
func (s *MemStore) CountSegment(_ context.Context, keys []string, dims int16) (int, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, p := range s.profiles {
		if segmentMatch(p, keys, dims) {
			n++
		}
	}
	if n < KThreshold {
		return 0, false, nil
	}
	return n, true, nil
}

// AudienceAfter implements Store.
func (s *MemStore) AudienceAfter(_ context.Context, keys []string, dims int16, after uuid.UUID, limit int) ([]uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []uuid.UUID
	for _, id := range s.sortedIDs() {
		if len(out) == limit {
			break
		}
		if compareUUID(id, after) > 0 && segmentMatch(s.profiles[id], keys, dims) {
			out = append(out, id)
		}
	}
	return out, nil
}
