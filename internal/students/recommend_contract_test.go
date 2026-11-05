package students_test

import (
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/students"
)

func TestRecommendStore_Contract_Mem(t *testing.T) {
	t.Parallel()
	recommendContract(t, func(*testing.T) (students.Store, world) {
		return students.NewMemStore(func() time.Time { return t0 }), memWorld()
	})
}

// recommendContract is the popular clubs count behind the recommendations
// (BE-174): only the clubs at least min eligible profiles of the dimension
// hold, most members first, then by club key; ineligible profiles, other
// dimensions and the keys that are not clubs do not count.
func recommendContract(t *testing.T, newStore func(t *testing.T) (students.Store, world)) {
	t.Helper()
	t.Run("popular clubs", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		uni := w.university(t)
		dept, otherDept := w.department(t, uni), w.department(t, uni)
		uniKey, deptKey, otherKey := "uni:"+uni.String(), "dept:"+dept.String(), "dept:"+otherDept.String()
		clubA, clubB, clubC := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
		profile := func(eligible bool, keys ...string) {
			t.Helper()
			p := students.Profile{UserID: w.user(t), UniversityID: uni, DegreeLevel: students.DegreeBachelor, Status: students.ProfileActive}
			if _, ok, err := s.InsertProfile(ctx, p); err != nil || !ok {
				t.Fatalf("InsertProfile: %v %v", ok, err)
			}
			keys = append([]string{students.KeyAll}, keys...)
			slices.Sort(keys)
			if err := s.SetAudience(ctx, p.UserID, keys, eligible, t0); err != nil {
				t.Fatal(err)
			}
		}
		// club A: 11 of the department; club B: 10 of the department (one
		// more is not eligible); club C: 10 of another department.
		for i := range 11 {
			if i < 10 {
				profile(true, uniKey, deptKey, "year:2", students.ClubKey(clubA), students.ClubKey(clubB))
			} else {
				profile(true, uniKey, deptKey, students.ClubKey(clubA))
			}
		}
		profile(false, uniKey, deptKey, students.ClubKey(clubB))
		for range 10 {
			profile(true, uniKey, otherKey, students.ClubKey(clubC))
		}

		got, err := s.PopularClubs(ctx, deptKey, 10, 10)
		if err != nil || !slices.Equal(got, []uuid.UUID{clubA, clubB}) {
			t.Fatalf("department = %v, %v; want A then B", got, err)
		}
		if got, err := s.PopularClubs(ctx, deptKey, 11, 10); err != nil || !slices.Equal(got, []uuid.UUID{clubA}) {
			t.Fatalf("min 11 = %v, %v; want A (B has 10 eligible members)", got, err)
		}
		got, err = s.PopularClubs(ctx, uniKey, 10, 10)
		if err != nil || len(got) != 3 || got[0] != clubA {
			t.Fatalf("university = %v, %v; want A first, then B and C", got, err)
		}
		if got, err := s.PopularClubs(ctx, uniKey, 10, 1); err != nil || !slices.Equal(got, []uuid.UUID{clubA}) {
			t.Fatalf("limit 1 = %v, %v", got, err)
		}
		if got, err := s.PopularClubs(ctx, "dept:"+uuid.NewV7().String(), 10, 10); err != nil || len(got) != 0 {
			t.Fatalf("unknown department = %v, %v", got, err)
		}
		// A tie (10 and 10) is ordered by club key.
		got, err = s.PopularClubs(ctx, "year:2", 10, 10)
		want := []uuid.UUID{clubA, clubB}
		if students.ClubKey(clubB) < students.ClubKey(clubA) {
			want = []uuid.UUID{clubB, clubA}
		}
		if err != nil || !slices.Equal(got, want) {
			t.Fatalf("any dimension key = %v, %v; want %v", got, err, want)
		}
	})
}
