package students

import (
	"slices"
	"testing"
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
