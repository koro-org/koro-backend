package directory

import (
	"cmp"
	"context"
	"slices"
	"uuid"
)

// UniversityByOrg implements StatsStore.
func (s *MemStore) UniversityByOrg(_ context.Context, orgID uuid.UUID) (University, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range s.unis {
		if u.OrgID == orgID {
			return u, nil
		}
	}
	return University{}, ErrNotFound
}

// ClubsOf implements StatsStore.
func (s *MemStore) ClubsOf(_ context.Context, universityID uuid.UUID) ([]Club, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Club{}
	for _, c := range s.clubs {
		if c.UniversityID == universityID {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b Club) int { return cmp.Compare(a.OrgID.String(), b.OrgID.String()) })
	return out, nil
}

// DepartmentsOf implements StatsStore.
func (s *MemStore) DepartmentsOf(_ context.Context, universityID uuid.UUID) ([]Department, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Department{}
	for _, d := range s.depts {
		if d.UniversityID == universityID {
			out = append(out, d)
		}
	}
	slices.SortFunc(out, func(a, b Department) int { return a.ID.Compare(b.ID) })
	return out, nil
}
