package recommend

import (
	"slices"

	"koro/internal/directory"
	"koro/internal/events"
	"koro/internal/students"
)

// interestMatch is what one interest of the student matches (BE-204): the
// club category of the same name, event categories and opportunity types.
// Every value is one of the owning module's enums (events.Categories,
// directory.Categories, feed.OpportunityTypes; pinned by a test); no
// sensitive club category is ever one.
type interestMatch struct {
	events        []events.Category
	opportunities []string
}

// interestMatches maps every students.Interests value (a test pins that
// none is missing). The club category is the interest's own name.
var interestMatches = map[students.Interest]interestMatch{
	students.InterestAcademic:             {[]events.Category{events.CategoryTalk}, []string{"project"}},
	students.InterestTechnology:           {[]events.Category{events.CategoryWorkshop, events.CategoryCompetition}, []string{"project"}},
	students.InterestCareer:               {[]events.Category{events.CategoryCareer}, []string{"internship", "job", "part_time"}},
	students.InterestEntrepreneurship:     {[]events.Category{events.CategoryCompetition, events.CategoryCareer}, []string{"project"}},
	students.InterestArts:                 {[]events.Category{events.CategoryCulture}, []string{"talent_call"}},
	students.InterestCulture:              {[]events.Category{events.CategoryCulture, events.CategoryTrip}, nil},
	students.InterestSports:               {[]events.Category{events.CategorySports}, []string{"talent_call"}},
	students.InterestSocialResponsibility: {[]events.Category{events.CategoryVolunteering}, []string{"volunteer"}},
	students.InterestHobby:                {[]events.Category{events.CategorySocial, events.CategoryTrip}, nil},
}

// interestSignals are what the student's interests match, each value once
// in the order of the interests.
type interestSignals struct {
	events        []events.Category
	clubs         []directory.Category
	opportunities []string
}

func matchesOf(in []students.Interest) interestSignals {
	var out interestSignals
	for _, i := range in {
		m, ok := interestMatches[i]
		if !ok {
			continue
		}
		out.events = appendNew(out.events, m.events...)
		if c := directory.Category(i); c.Valid() && !c.Sensitive() {
			out.clubs = appendNew(out.clubs, c)
		}
		out.opportunities = appendNew(out.opportunities, m.opportunities...)
	}
	return out
}

// appendNew appends the values of vs that dst does not hold yet.
func appendNew[T comparable](dst []T, vs ...T) []T {
	for _, v := range vs {
		if !slices.Contains(dst, v) {
			dst = append(dst, v)
		}
	}
	return dst
}
