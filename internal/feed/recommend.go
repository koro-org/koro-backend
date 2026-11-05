package feed

import (
	"context"
	"uuid"
)

// MaxFollowedOrgs bounds FollowedOrgIDs.
const MaxFollowedOrgs = 500

// Follows is the follow read of the rule-based recommendations (BE-174;
// recommend, uyum denetimi #25): a system read of one user's own follows
// (org_follows, manual or automatic), without authorisation: the caller
// authorises its own read first (cell O: the student's own data for the
// student only). Org IDs only, in one query; listMyFollows stays the
// paged, authorised HTTP read with the org cards.
type Follows interface {
	// FollowedOrgIDs returns up to limit (at most MaxFollowedOrgs) orgs
	// userID follows, newest follow first.
	FollowedOrgIDs(ctx context.Context, userID uuid.UUID, limit int) ([]uuid.UUID, error)
}

var _ Follows = (*service)(nil)

// FollowedOrgIDs implements Follows.
func (s *service) FollowedOrgIDs(ctx context.Context, userID uuid.UUID, limit int) ([]uuid.UUID, error) {
	limit = min(limit, MaxFollowedOrgs)
	if limit <= 0 || userID == uuid.Nil() {
		return []uuid.UUID{}, nil
	}
	rows, err := s.interactions().ListFollows(ctx, userID, nil, limit)
	if err != nil {
		return nil, err
	}
	out := make([]uuid.UUID, len(rows))
	for i, f := range rows {
		out[i] = f.OrgID
	}
	return out, nil
}
