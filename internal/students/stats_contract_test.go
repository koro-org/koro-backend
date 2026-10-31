package students_test

import (
	"testing"
	"time"
	"uuid"

	"koro/internal/students"
)

func TestStatsStore_Contract_Mem(t *testing.T) {
	t.Parallel()
	statsContract(t, func(*testing.T) (students.Store, world) {
		return students.NewMemStore(func() time.Time { return t0 }), memWorld()
	})
}

// statsContract is the exact counts behind a university's panel (BE-163):
// its active profiles in total, created in a range and by degree level,
// class year and department; inactive profiles and other universities'
// are left out. (The service suppresses the groups below 10.)
func statsContract(t *testing.T, newStore func(t *testing.T) (students.Store, world)) {
	t.Helper()
	t.Run("profile counts", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		uni, other := w.university(t), w.university(t)
		dept := w.department(t, uni)
		year3 := int16(3)
		insert := func(p students.Profile) {
			t.Helper()
			if _, ok, err := s.InsertProfile(ctx, p); err != nil || !ok {
				t.Fatalf("InsertProfile: %v %v", ok, err)
			}
		}
		for i := range 12 {
			p := students.Profile{UserID: w.user(t), UniversityID: uni, DegreeLevel: students.DegreeBachelor, Status: students.ProfileActive}
			if i < 11 {
				p.ClassYear = &year3
			}
			if i < 10 {
				p.DepartmentID = &dept
			}
			insert(p)
		}
		for range 3 {
			insert(students.Profile{UserID: w.user(t), UniversityID: uni, DegreeLevel: students.DegreeMaster, Status: students.ProfileActive})
		}
		insert(students.Profile{UserID: w.user(t), UniversityID: uni, DegreeLevel: students.DegreeMaster, Status: students.ProfileInactive})
		insert(students.Profile{UserID: w.user(t), UniversityID: other, DegreeLevel: students.DegreeBachelor, Status: students.ProfileActive})

		wideFrom, wideTo := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
		c, err := s.ProfileCounts(ctx, uni, wideFrom, wideTo)
		if err != nil {
			t.Fatal(err)
		}
		if c.Total != 15 || c.NewInRange != 15 || c.ByDegreeLevel["bachelor"] != 12 || c.ByDegreeLevel["master"] != 3 ||
			len(c.ByDegreeLevel) != 2 || c.ByClassYear["3"] != 11 || len(c.ByClassYear) != 1 ||
			c.ByDepartment[dept.String()] != 10 || len(c.ByDepartment) != 1 {
			t.Fatalf("counts = %+v", c)
		}
		if c, err := s.ProfileCounts(ctx, uni, wideFrom, wideFrom.Add(time.Hour)); err != nil || c.NewInRange != 0 || c.Total != 15 {
			t.Fatalf("an empty range = %+v, %v", c, err)
		}
		if c, err := s.ProfileCounts(ctx, uuid.NewV7(), wideFrom, wideTo); err != nil || c.Total != 0 || len(c.ByDegreeLevel) != 0 {
			t.Fatalf("unknown university = %+v, %v", c, err)
		}
	})
}
