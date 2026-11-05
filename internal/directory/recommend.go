package directory

import (
	"context"
	"slices"
	"uuid"
)

// MaxRecommendableClubs bounds the IDs one RecommendableClubs call reads.
const MaxRecommendableClubs = 100

// Recommendations is the club read of the rule-based recommendations
// (BE-174; recommend, uyum denetimi #25): a system read without
// authorisation (the caller authorises its own read; a club card is public,
// 03 §3.6). A sensitive club is never recommended (03 Y2, KVKK md. 6): its
// card does not come back, whatever signal named it.
type Recommendations interface {
	// RecommendableClubs returns the cards of up to limit clubs among
	// orgIDs (the first MaxRecommendableClubs) that may be recommended — a
	// club whose org is active and that is not sensitive — in the order of
	// orgIDs; the others are left out. A card carries the member count
	// from MemberThreshold on and the public logo, like listClubs.
	RecommendableClubs(ctx context.Context, orgIDs []uuid.UUID, limit int) ([]ClubCard, error)
}

var _ Recommendations = (*service)(nil)

// RecommendableClubs implements Recommendations: one store read of the
// rows, one logo read and the member counts of the kept cards only.
func (s *service) RecommendableClubs(ctx context.Context, orgIDs []uuid.UUID, limit int) ([]ClubCard, error) {
	out := []ClubCard{}
	var ids []uuid.UUID
	for _, id := range orgIDs {
		if len(ids) == MaxRecommendableClubs {
			break
		}
		if id != uuid.Nil() && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 || limit <= 0 {
		return out, nil
	}
	rows, err := s.d.Store.ListClubs(ctx, ClubFilter{OrgIDs: ids, Sort: SortName}, nil, len(ids))
	if err != nil {
		return nil, err
	}
	byID := make(map[uuid.UUID]ClubRow, len(rows))
	for _, r := range rows {
		if !r.IsSensitive && r.OrgStatus == "active" {
			byID[r.OrgID] = r
		}
	}
	var kept []ClubRow
	for _, id := range ids {
		if r, ok := byID[id]; ok && len(kept) < limit {
			kept = append(kept, r)
		}
	}
	if len(kept) == 0 {
		return out, nil
	}
	keptIDs := make([]uuid.UUID, len(kept))
	for i, r := range kept {
		keptIDs[i] = r.OrgID
	}
	logos, err := s.d.Orgs.PublicLogoURLs(ctx, keptIDs)
	if err != nil {
		return nil, err
	}
	for _, r := range kept {
		out = append(out, ClubCard{Row: r, MemberCount: s.countOf(ctx, r.Club), LogoPublicURL: logos[r.OrgID]})
	}
	return out, nil
}
