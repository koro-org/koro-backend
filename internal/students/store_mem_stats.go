package students

import (
	"context"
	"slices"
	"strconv"
	"time"
	"uuid"
)

// ProfileCounts implements statsStore.
func (m *MemStore) ProfileCounts(_ context.Context, universityID uuid.UUID, departments []uuid.UUID, from, to time.Time) (ProfileCounts, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := newProfileCounts()
	for _, p := range m.profiles {
		if p.UniversityID != universityID || p.Status != ProfileActive ||
			len(departments) > 0 && (p.DepartmentID == nil || !slices.Contains(departments, *p.DepartmentID)) {
			continue
		}
		c.Total++
		if !p.CreatedAt.Before(from) && p.CreatedAt.Before(to) {
			c.NewInRange++
		}
		c.ByDegreeLevel[string(p.DegreeLevel)]++
		if p.ClassYear != nil {
			c.ByClassYear[strconv.Itoa(int(*p.ClassYear))]++
		}
		if p.DepartmentID != nil {
			c.ByDepartment[p.DepartmentID.String()]++
		}
	}
	return c, nil
}
