package students

import (
	"context"
	"encoding/json"
	"errors"
	"time"
	"uuid"
)

// MaxPopularClubs bounds each list of PopularClubs.
const MaxPopularClubs = 50

// PopularClubsTTL is how long a department's or a university's popular
// clubs stay cached (Deps.Cache, BE-197): a membership change shows in the
// counts at most this late.
const PopularClubsTTL = 10 * time.Minute

// popularCacheKey is the cache key of a dimension's popular clubs (a
// department or university key: no personal data). v1 is the encoding
// (a JSON array of club org IDs, at most MaxPopularClubs).
func popularCacheKey(dim string) string { return "students:popular_clubs:v1:" + dim }

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
// hold the dimension key. With Deps.Cache each count is cached per
// dimension for PopularClubsTTL; a cached list is checked against the
// current sensitive clubs on every read, so a club that became sensitive
// after it was cached never leaves (03 Y2, the same answer as without the
// cache).
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
		if out.Department, err = s.popularClubs(ctx, keyDept+p.DepartmentID.String(), limit); err != nil {
			return PopularClubs{}, err
		}
	}
	if out.University, err = s.popularClubs(ctx, keyUni+p.UniversityID.String(), limit); err != nil {
		return PopularClubs{}, err
	}
	if s.d.Cache != nil {
		if out, err = s.dropSensitive(ctx, out); err != nil {
			return PopularClubs{}, err
		}
	}
	return out, nil
}

// popularClubs returns up to limit popular clubs of dim: from the cache
// when it holds dim, otherwise counted (MaxPopularClubs of them, the list
// every limit is a prefix of) and cached. The cache fails open: an error
// or an undecodable entry is logged and the count is made.
func (s *service) popularClubs(ctx context.Context, dim string, limit int) ([]uuid.UUID, error) {
	if s.d.Cache == nil {
		return s.recommendations().PopularClubs(ctx, dim, KThreshold, limit)
	}
	key := popularCacheKey(dim)
	if raw, found, err := s.d.Cache.Get(ctx, key); err != nil {
		s.d.Log.WarnContext(ctx, "students: popular clubs cache read failed", "err", err)
	} else if found {
		var ids []uuid.UUID
		if err := json.Unmarshal(raw, &ids); err == nil {
			return ids[:min(limit, len(ids))], nil
		}
		s.d.Log.WarnContext(ctx, "students: popular clubs cache entry undecodable", "err", err)
	}
	ids, err := s.recommendations().PopularClubs(ctx, dim, KThreshold, MaxPopularClubs)
	if err != nil {
		return nil, err
	}
	if raw, err := json.Marshal(ids); err != nil {
		s.d.Log.WarnContext(ctx, "students: popular clubs cache encode failed", "err", err)
	} else if err := s.d.Cache.Set(ctx, key, raw, PopularClubsTTL); err != nil {
		s.d.Log.WarnContext(ctx, "students: popular clubs cache write failed", "err", err)
	}
	return ids[:min(limit, len(ids))], nil
}

// dropSensitive removes the clubs that are sensitive now from both lists
// (one directory read for both).
func (s *service) dropSensitive(ctx context.Context, pc PopularClubs) (PopularClubs, error) {
	all := append(append([]uuid.UUID{}, pc.Department...), pc.University...)
	if len(all) == 0 {
		return pc, nil
	}
	sensitive, err := s.d.Catalog.SensitiveOrgIDs(ctx, all)
	if err != nil {
		return PopularClubs{}, err
	}
	keep := func(ids []uuid.UUID) []uuid.UUID {
		out := make([]uuid.UUID, 0, len(ids))
		for _, id := range ids {
			if !sensitive[id] {
				out = append(out, id)
			}
		}
		return out
	}
	return PopularClubs{Department: keep(pc.Department), University: keep(pc.University)}, nil
}
