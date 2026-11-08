package recommend

import (
	"koro/internal/events"
	"koro/internal/students"
)

// ToDTOForTest exposes toDTO to the external tests.
var ToDTOForTest = toDTO

// InterestMatch is an interest's matches, exposed to the external tests.
type InterestMatch struct {
	Events        []events.Category
	Opportunities []string
}

// InterestMatchesForTest exposes interestMatches to the external tests.
func InterestMatchesForTest() map[students.Interest]InterestMatch {
	out := make(map[students.Interest]InterestMatch, len(interestMatches))
	for i, m := range interestMatches {
		out[i] = InterestMatch{Events: m.events, Opportunities: m.opportunities}
	}
	return out
}
