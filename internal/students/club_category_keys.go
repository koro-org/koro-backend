package students

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"uuid"

	"koro/internal/directory"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/queue"
)

// The club category dimension of the audience (BE-207, Q-58): a company
// targets the students who are active members of a club of a category
// ("club_category:<category>"). Guarded on every side:
//
//   - only targetable categories (directory.Category.Targetable: neither
//     religious, political, identity nor other) ever become keys, and a spec
//     naming any other value is malformed (compile);
//   - a sensitive club gives no key, whatever its category: its members are
//     never counted or reached through this dimension (03 Y2; a student
//     also in a non-sensitive club of the same category is, through that
//     club);
//   - the keys are derived like every other audience key (the membership
//     events, the sensitivity and category changes of a club and the
//     nightly refresh agree), counted only through count_segment (k ≥ 10,
//     no number below) and never shown to a company.
const keyClubCategory = "club_category:"

// RefreshClubCategoryTaskName consumes club.updated: when a club's
// category changed, its active members are recomputed (their
// club_category key follows the new category).
const RefreshClubCategoryTaskName = "students.refresh_club_category"

// ClubCategoryKey is the audience key of a club category.
func ClubCategoryKey(c directory.Category) string { return keyClubCategory + string(c) }

// isClubCategoryKey reports a club category key.
func isClubCategoryKey(k string) bool { return strings.HasPrefix(k, keyClubCategory) }

// validClubCategoryKey reports whether the value of a club_category key is
// a targetable category.
func validClubCategoryKey(val string) bool { return directory.Category(val).Targetable() }

// clubCategoriesOf returns the categories of clubs a student's keys hold:
// the targetable category of each club that is not sensitive, once each.
func clubCategoriesOf(clubs []uuid.UUID, sensitive map[uuid.UUID]bool, cats map[uuid.UUID]directory.Category) []directory.Category {
	var out []directory.Category
	for _, c := range clubs {
		if cat, ok := cats[c]; ok && !sensitive[c] && cat.Targetable() && !slices.Contains(out, cat) {
			out = append(out, cat)
		}
	}
	return out
}

// targetableCategories reads from directory the targetable categories of
// clubs (sorted, without duplicates).
func (s *service) targetableCategories(ctx context.Context, clubs []uuid.UUID) (map[uuid.UUID]directory.Category, error) {
	if len(clubs) == 0 {
		return map[uuid.UUID]directory.Category{}, nil
	}
	cats, err := s.d.Catalog.TargetableClubCategories(ctx, clubs)
	if err != nil {
		return nil, fmt.Errorf("students: club categories: %w", err)
	}
	return cats, nil
}

// handleClubUpdated is students.refresh_club_category (club.updated): a
// category change recomputes the club's active members; any other change
// of the club profile is not about the audience. Idempotent.
func (s *service) handleClubUpdated(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.ClubUpdated](e)
	if err != nil {
		return queue.Permanent(err)
	}
	if !slices.Contains(p.Fields, "category") {
		return nil
	}
	return s.refreshMembers(ctx, p.ClubOrgID, RefreshClubCategoryTaskName)
}

// refreshMembers recomputes the audience of every active member of a club.
func (s *service) refreshMembers(ctx context.Context, clubOrgID uuid.UUID, job string) error {
	members, err := s.d.Orgs.ActiveMemberIDs(ctx, clubOrgID)
	if err != nil {
		return fmt.Errorf("%s: %w", job, err)
	}
	for _, u := range members {
		if err := s.refreshUser(ctx, u, job); err != nil {
			return fmt.Errorf("%s: %w", job, err)
		}
	}
	return nil
}
