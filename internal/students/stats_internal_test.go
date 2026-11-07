package students

import (
	"slices"
	"testing"
	"uuid"
)

// TestStats_GroupsBelowKSuppressed: a count below KThreshold never leaves
// the module (SR §8: 10 kişinin altındaki gruplar gösterilmez).
func TestStats_GroupsBelowKSuppressed(t *testing.T) {
	t.Parallel()
	if atLeastK(9) != nil || *atLeastK(10) != 10 {
		t.Fatal("atLeastK must drop 9 and keep 10")
	}
	got := groups(map[string]int{"a": 9, "b": 10, "c": 40, "d": 10, "e": 1})
	want := []StudentGroup{{Key: "c", Count: 40}, {Key: "b", Count: 10}, {Key: "d", Count: 10}}
	if !slices.Equal(got, want) {
		t.Fatalf("groups = %+v, want %+v", got, want)
	}
	if g := groups(map[string]int{}); g == nil || len(g) != 0 {
		t.Fatalf("no group = %#v, want an empty list", g)
	}
}

// TestStats_FacultySumsDepartmentsBeforeSuppression (BE-190): a faculty
// sums the exact counts of its departments, so two departments of 6 make
// a published faculty of 12 while neither department is published; a
// department outside the map is in no faculty.
func TestStats_FacultySumsDepartmentsBeforeSuppression(t *testing.T) {
	t.Parallel()
	d1, d2, d3, loose := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	f1, f2 := uuid.NewV7(), uuid.NewV7()
	counts := map[string]int{d1.String(): 6, d2.String(): 6, d3.String(): 4, loose.String(): 30, "not-an-id": 50}
	got := groups(byFaculty(counts, map[uuid.UUID]uuid.UUID{d1: f1, d2: f1, d3: f2}))
	if want := []StudentGroup{{Key: f1.String(), Count: 12}}; !slices.Equal(got, want) {
		t.Fatalf("faculties = %+v, want %+v", got, want)
	}
	if g := groups(counts); len(g) != 2 {
		t.Fatalf("departments = %+v (only the two of at least 10)", g)
	}
}
