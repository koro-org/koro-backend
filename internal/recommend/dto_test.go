package recommend_test

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	"koro/internal/recommend"
)

// forbidden are member names the recommendations never carry (01 §0-7,
// BE-174): no score, rank, weight, count of people, talent value or
// sensitive flag of the student.
var forbidden = regexp.MustCompile(`(^|_)(score|rank\w*|weight|talent\w*|band|point\w*|count|sensitive|is_minor|audience_keys)$`)

// TestDTO_NoScoreOrPeopleCount (BE-174): the answer explains with reason
// codes only; the top-level and item members carry no score, rank or
// count (the event, club and post cards are listEvents', listClubs' and
// getFeed's own, pinned there).
func TestDTO_NoScoreOrPeopleCount(t *testing.T) {
	t.Parallel()
	for _, typ := range []reflect.Type{
		reflect.TypeFor[recommend.RecommendationsDTO](), reflect.TypeFor[recommend.RecommendedEventDTO](),
		reflect.TypeFor[recommend.RecommendedClubDTO](), reflect.TypeFor[recommend.RecommendedOpportunityDTO](),
	} {
		for i := range typ.NumField() {
			name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
			if forbidden.MatchString(name) {
				t.Errorf("%s.%s is forbidden", typ.Name(), name)
			}
		}
	}
}
