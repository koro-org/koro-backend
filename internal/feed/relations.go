package feed

import (
	"context"
	"fmt"
	"slices"
	"uuid"

	"koro/internal/authz"
	"koro/internal/orgs"
	"koro/internal/platform/problem"
)

// MaxRelationOrgs bounds the org IDs of one listMyOrgRelations call (a
// page of org cards).
const MaxRelationOrgs = 50

// MembershipStatus is the reader's membership of an org in OrgRelation.
type MembershipStatus string

// Membership statuses of OrgRelation.
const (
	MembershipNone    MembershipStatus = "none"
	MembershipPending MembershipStatus = "pending" // a join request awaiting the club
	MembershipActive  MembershipStatus = "active"
)

// OrgRelation is the signed-in reader's own relation to one org
// (listMyOrgRelations, BE-122): the state of the follow, mute and join
// buttons of an org page or card.
type OrgRelation struct {
	OrgID      uuid.UUID
	Following  bool
	Muted      bool
	Membership MembershipStatus
}

// OrgRelations implements Service. It answers for every requested ID,
// existing or not, with the reader's own rows only: an unknown org is
// "none", so the call tells nothing about any org (feed.follow.list_own,
// self). The public org reads stay anonymous and cacheable; this is the
// only place the reader's state of an org page comes from.
func (s *service) OrgRelations(ctx context.Context, a authz.Actor, orgIDs []uuid.UUID) ([]OrgRelation, error) {
	if err := authz.Can(ctx, a, authz.FeedFollowListOwn, ownList(a)); err != nil {
		return nil, err
	}
	ids := uniqueIDs(orgIDs)
	if len(ids) == 0 || len(ids) > MaxRelationOrgs {
		return nil, problem.Field("query.org_id", fmt.Sprintf("must hold 1 to %d organization IDs", MaxRelationOrgs))
	}
	vs, err := s.d.Store.ViewerState(ctx, a.UserID, nil, ids)
	if err != nil {
		return nil, err
	}
	open, err := s.d.Orgs.OpenMembershipsOf(ctx, a.UserID)
	if err != nil {
		return nil, err
	}
	out := make([]OrgRelation, len(ids))
	for i, id := range ids {
		m := MembershipNone
		switch open[id] {
		case orgs.MemberActive:
			m = MembershipActive
		case orgs.MemberPending:
			m = MembershipPending
		}
		out[i] = OrgRelation{OrgID: id, Following: vs.Following[id], Muted: vs.Muted[id], Membership: m}
	}
	return out, nil
}

// uniqueIDs drops repeated IDs, keeping the first position of each.
func uniqueIDs(ids []uuid.UUID) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}
