package students

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"uuid"

	"koro/internal/platform/db"
	"koro/internal/platform/problem"
)

// Interest is an entry of student_profiles.interests (BE-204, migration
// 000184; user decision Q-57): a topic the student declared, read only by
// the student's own rule-based recommendations (recommend matches_interest)
// and shown to the student alone. It is never an audience key, never in a
// company's view of the student (levels 0–3) and never a talent signal.
type Interest string

// Interests are the club categories of directory that are not sensitive
// (clubs_category_chk without religious, political, identity and other):
// one vocabulary for the clubs, the events and the opportunity calls they
// are matched with (recommend maps each to event categories and
// opportunity types). A special category of personal data (religion,
// political opinion, health, sexual life, ethnic origin, union; KVKK md. 6)
// is never an interest: student_profiles_interests_chk holds this fixed
// list, and a change is a migration.
const (
	InterestAcademic             Interest = "academic"
	InterestTechnology           Interest = "technology"
	InterestCareer               Interest = "career"
	InterestEntrepreneurship     Interest = "entrepreneurship"
	InterestArts                 Interest = "arts"
	InterestCulture              Interest = "culture"
	InterestSports               Interest = "sports"
	InterestSocialResponsibility Interest = "social_responsibility"
	InterestHobby                Interest = "hobby"
)

// MaxInterests is how many interests a student may hold
// (student_profiles_interests_chk).
const MaxInterests = 5

// interestOrder is the CHECK's order; a profile keeps its interests sorted
// by it.
var interestOrder = []Interest{
	InterestAcademic, InterestTechnology, InterestCareer, InterestEntrepreneurship, InterestArts, InterestCulture,
	InterestSports, InterestSocialResponsibility, InterestHobby,
}

// Interests lists every interest in the CHECK's order (OpenAPI enum,
// recommend's mapping).
func Interests() []Interest { return slices.Clone(interestOrder) }

// Valid reports whether i is an interest of the CHECK.
func (i Interest) Valid() bool { return slices.Contains(interestOrder, i) }

// cleanInterests validates in (body.interests) and returns it without
// duplicates in the CHECK's order: 422 for an unknown value or more than
// MaxInterests.
func cleanInterests(in []Interest) (db.TextArray, error) {
	out := db.TextArray{}
	for i, x := range in {
		if !x.Valid() {
			return nil, problem.Field("body.interests["+strconv.Itoa(i)+"]", "is not an interest")
		}
	}
	for _, x := range interestOrder {
		if slices.Contains(in, x) {
			out = append(out, string(x))
		}
	}
	if len(out) > MaxInterests {
		return nil, problem.Field("body.interests", "must have at most "+strconv.Itoa(MaxInterests)+" items")
	}
	return out, nil
}

// interestsOf returns the stored interests of p (an unknown stored value,
// which the CHECK rules out, is skipped).
func interestsOf(p Profile) []Interest {
	out := []Interest{}
	for _, x := range p.Interests {
		if Interest(x).Valid() {
			out = append(out, Interest(x))
		}
	}
	return out
}

// Interests implements Recommendations.
func (s *service) Interests(ctx context.Context, userID uuid.UUID) ([]Interest, error) {
	p, err := s.profiles().GetProfile(ctx, userID)
	switch {
	case errors.Is(err, ErrNotFound):
		return []Interest{}, nil
	case err != nil:
		return nil, err
	}
	return interestsOf(p), nil
}
