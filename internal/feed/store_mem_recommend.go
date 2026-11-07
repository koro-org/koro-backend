package feed

import (
	"context"
	"slices"

	"koro/internal/platform/civil"
)

// RecommendableOpportunities implements Store.
func (s *MemStore) RecommendableOpportunities(_ context.Context, f OpportunityFilter, limit int) ([]Post, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if (len(f.OrgIDs) == 0 && f.ClassYear <= 0 && f.MinInterest <= 0) || limit <= 0 || f.Reader.UserID == nil {
		return []Post{}, nil
	}
	u := *f.Reader.UserID
	var out []Post
	for _, p := range s.posts {
		if p.Status != StatusPublished || p.Kind != KindOpportunityCall || p.PublishedAt.Before(f.Since) ||
			!s.visible(f.Reader, p) || has(s.mutes, u, p.OrgID) || has(s.hides, u, p.ID) || has(s.likes, u, p.ID) ||
			has(s.saves, u, p.ID) {
			continue
		}
		o := decodeOpportunity(p.Opportunity)
		if o == nil || !stillOpen(o.Deadline, f.Today) {
			continue
		}
		match := slices.Contains(f.OrgIDs, p.OrgID) || f.ClassYear > 0 && slices.Contains(o.EligibleYears, f.ClassYear) ||
			f.MinInterest > 0 && (p.LikeCount >= f.MinInterest || p.SaveCount >= f.MinInterest)
		if match {
			out = append(out, p.clone())
		}
	}
	return pagePublished(out, nil, limit), nil
}

// stillOpen reports whether a call with deadline (YYYY-MM-DD, "" for none) is
// still open on today: the GORM store's text comparison of the dates.
func stillOpen(deadline string, today civil.Date) bool {
	return deadline == "" || deadline >= today.String()
}
