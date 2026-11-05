package feed_test

import (
	"slices"
	"testing"
	"uuid"

	"koro/internal/feed"
)

// TestFollowedOrgIDs_OwnFollowsNewestFirst (BE-174): the recommendations
// read one user's own follows as org IDs, newest first, bounded by the
// limit; another user's follows are not theirs.
func TestFollowedOrgIDs_OwnFollowsNewestFirst(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	reader, other := persona(t, "student"), persona(t, "student_minor")
	for _, org := range []string{"club_a", "club_b"} {
		if err := e.svc.FollowOrg(ctx, reader, orgID(org), nil); err != nil {
			t.Fatal(err)
		}
		e.clk.Advance(1)
	}
	if err := e.svc.FollowOrg(ctx, other, orgID("uni_a"), nil); err != nil {
		t.Fatal(err)
	}
	f, ok := e.svc.(feed.Follows)
	if !ok {
		t.Fatal("the feed service does not implement Follows")
	}
	got, err := f.FollowedOrgIDs(ctx, reader.UserID, 10)
	if err != nil || !slices.Equal(got, []uuid.UUID{orgID("club_b"), orgID("club_a")}) {
		t.Fatalf("FollowedOrgIDs = %v, %v; want club_b then club_a", got, err)
	}
	if got, err := f.FollowedOrgIDs(ctx, reader.UserID, 1); err != nil || !slices.Equal(got, []uuid.UUID{orgID("club_b")}) {
		t.Fatalf("limit 1 = %v, %v", got, err)
	}
	if got, err := f.FollowedOrgIDs(ctx, reader.UserID, 0); err != nil || len(got) != 0 {
		t.Fatalf("limit 0 = %v, %v", got, err)
	}
}
