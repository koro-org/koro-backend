package students

import (
	"context"
	"errors"
	"uuid"
)

// MaxPopularClubs bounds each list of PopularClubs.
const MaxPopularClubs = 50

// Recommendations is the student read of the rule-based recommendations
// (BE-174; recommend, uyum denetimi #25): system reads without
// authorisation; recommend authorises its own caller first (cell O: the
// student's own data for the student only). Nothing about another student
// leaves students: a popular club is one that at least KThreshold audience
// eligible students (verified, adult, active) of the caller's department or
// university are members of, counted in the database over the stored
// audience keys, which never hold a sensitive club (03 Y2); no count and no
// member leaves the module.
type Recommendations interface {
	// RecommendationChoice returns the recommendation choice of userID
	// (default without a settings row).
	RecommendationChoice(ctx context.Context, userID uuid.UUID) (RecommendationChoice, error)
	// PopularClubs returns up to limit (at most MaxPopularClubs) club orgs
	// popular in the department and in the university of userID's student
	// profile, most members first; none without a profile (or, for the
	// department list, without a department).
	PopularClubs(ctx context.Context, userID uuid.UUID, limit int) (PopularClubs, error)
}

// PopularClubs are the clubs popular around a student (Recommendations):
// club org IDs, most members first. A club may be in both lists.
type PopularClubs struct {
	Department []uuid.UUID
	University []uuid.UUID
}

var _ Recommendations = (*service)(nil)

// RecommendationChoice implements Recommendations.
func (s *service) RecommendationChoice(ctx context.Context, userID uuid.UUID) (RecommendationChoice, error) {
	p, err := s.privacy().GetPrivacy(ctx, userID)
	switch {
	case errors.Is(err, ErrNotFound):
		return RankingDefault, nil
	case err != nil:
		return "", err
	}
	return p.RecommendationChoice, nil
}

// PopularClubs implements Recommendations: two counts in the database (the
// department's, the university's), each over the eligible profiles that
// hold the dimension key.
func (s *service) PopularClubs(ctx context.Context, userID uuid.UUID, limit int) (PopularClubs, error) {
	out := PopularClubs{Department: []uuid.UUID{}, University: []uuid.UUID{}}
	limit = min(max(limit, 0), MaxPopularClubs)
	if limit == 0 {
		return out, nil
	}
	p, err := s.profiles().GetProfile(ctx, userID)
	switch {
	case errors.Is(err, ErrNotFound):
		return out, nil
	case err != nil:
		return PopularClubs{}, err
	}
	if p.DepartmentID != nil {
		if out.Department, err = s.recommendations().PopularClubs(ctx, keyDept+p.DepartmentID.String(), KThreshold, limit); err != nil {
			return PopularClubs{}, err
		}
	}
	if out.University, err = s.recommendations().PopularClubs(ctx, keyUni+p.UniversityID.String(), KThreshold, limit); err != nil {
		return PopularClubs{}, err
	}
	return out, nil
}
