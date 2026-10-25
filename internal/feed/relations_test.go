package feed_test

import (
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/feed"
	"koro/internal/platform/problem/problemtest"
)

// TestOrgRelations_ReaderState: listMyOrgRelations answers the reader's own
// follow, mute and membership state of every org asked, in order, repeated
// IDs once; an unknown org reads as no relation (BE-122).
func TestOrgRelations_ReaderState(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	reader := persona(t, "student")
	clubA, clubB, company := orgID("club_a"), orgID("club_b"), orgID("company_a")
	if err := e.svc.FollowOrg(ctx, reader, clubA, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.MuteOrg(ctx, reader, company); err != nil {
		t.Fatal(err)
	}
	e.orgs.mu.Lock()
	e.orgs.members[reader.UserID] = append(e.orgs.members[reader.UserID],
		authz.Membership{OrgID: clubA, OrgKind: authz.OrgClub, Role: authz.MemberMember})
	e.orgs.pending[reader.UserID] = append(e.orgs.pending[reader.UserID], clubB)
	e.orgs.mu.Unlock()
	unknown := uuid.NewV7()

	got, err := e.svc.OrgRelations(ctx, reader, []uuid.UUID{company, clubA, unknown, clubB, clubA})
	if err != nil {
		t.Fatal(err)
	}
	want := []feed.OrgRelation{
		{OrgID: company, Muted: true, Membership: feed.MembershipNone},
		{OrgID: clubA, Following: true, Membership: feed.MembershipActive},
		{OrgID: unknown, Membership: feed.MembershipNone},
		{OrgID: clubB, Membership: feed.MembershipPending},
	}
	if len(got) != len(want) {
		t.Fatalf("OrgRelations = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("OrgRelations[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	// Another reader sees only their own (empty) state.
	other, err := e.svc.OrgRelations(ctx, persona(t, "student_minor"), []uuid.UUID{clubA, company})
	if err != nil || len(other) != 2 || other[0] != (feed.OrgRelation{OrgID: clubA, Membership: feed.MembershipNone}) ||
		other[1] != (feed.OrgRelation{OrgID: company, Membership: feed.MembershipNone}) {
		t.Fatalf("other reader = %+v, %v", other, err)
	}
}

// TestOrgRelations_SignedInOnlyAndBounded: the reader's state is never
// served anonymously (the public org reads stay cacheable) and one call
// carries 1 to MaxRelationOrgs IDs.
func TestOrgRelations_SignedInOnlyAndBounded(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	// Over HTTP the self surface answers 401 first (matrix: anonymous 401).
	_, err := e.svc.OrgRelations(ctx, authz.Anonymous(), []uuid.UUID{orgID("club_a")})
	problemtest.WantProblem(t, err, 403, "forbidden")

	reader := persona(t, "student")
	_, err = e.svc.OrgRelations(ctx, reader, nil)
	problemtest.WantProblem(t, err, 422, "validation_failed")
	many := make([]uuid.UUID, feed.MaxRelationOrgs+1)
	for i := range many {
		many[i] = uuid.NewV7()
	}
	_, err = e.svc.OrgRelations(ctx, reader, many)
	problemtest.WantProblem(t, err, 422, "validation_failed")
	if _, err := e.svc.OrgRelations(ctx, reader, many[:feed.MaxRelationOrgs]); err != nil {
		t.Fatalf("OrgRelations(%d IDs) = %v", feed.MaxRelationOrgs, err)
	}
}
