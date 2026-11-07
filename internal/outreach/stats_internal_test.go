package outreach

import (
	"slices"
	"testing"
	"uuid"
)

// TestStatsFilter_KeyGroups (BE-190): a university reaches the audiences
// that name it or one of its departments; the dimensions present all
// apply; no filter is no group.
func TestStatsFilter_KeyGroups(t *testing.T) {
	t.Parallel()
	uni, d1, d2, dept := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	year := int16(2)
	if g := (StatsFilter{}).keyGroups(); g != nil {
		t.Fatalf("no filter = %v", g)
	}
	got := StatsFilter{Uni: &uni, UniDepts: []uuid.UUID{d1, d2}, Dept: &dept, Year: &year}.keyGroups()
	want := [][]string{{keyUni + uni.String(), keyDept + d1.String(), keyDept + d2.String()}, {keyDept + dept.String()}, {"year:2"}}
	if !slices.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Fatalf("groups = %v, want %v", got, want)
	}
	keys := Definition{Dept: []uuid.UUID{d2}, Year: []int16{2, 3}}.Keys()
	if !holdsGroups(keys, [][]string{want[0], want[2]}) || holdsGroups(keys, want) || !holdsGroups(keys, nil) {
		t.Fatal("holdsGroups must need a key of every group")
	}
}
