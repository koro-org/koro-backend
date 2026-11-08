package stats

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"koro/internal/directory"
)

// TestCompanyInput_ClubCategoryEnumIsDirectorys (BE-207): the query's list
// of club categories is directory's targetable list (one vocabulary with
// the segment definition).
func TestCompanyInput_ClubCategoryEnumIsDirectorys(t *testing.T) {
	t.Parallel()
	f, ok := reflect.TypeFor[companyInput]().FieldByName("ClubCategory")
	if !ok {
		t.Fatal("no ClubCategory field")
	}
	var want []string
	for _, c := range directory.TargetableCategories() {
		want = append(want, string(c))
	}
	if got := strings.Split(f.Tag.Get("enum"), ","); !slices.Equal(got, want) {
		t.Fatalf("enum = %v, want %v", got, want)
	}
}
