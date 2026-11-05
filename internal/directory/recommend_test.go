package directory_test

import (
	"slices"
	"testing"
	"uuid"

	"koro/internal/directory"
	"koro/internal/orgs"
)

// TestRecommendableClubs_NeverSensitiveOrInactive (BE-174, 03 Y2): the
// recommendations' club cards keep the order they were asked in, leave out
// a sensitive club (whatever named it), a club whose org is not active and
// an unknown ID, show the member count from 10 on and stop at the limit.
func TestRecommendableClubs_NeverSensitiveOrInactive(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	uni := e.university(t, "U-R", "Rize Üniversitesi", "Rize")
	photo := e.club(t, uni, "Fotoğraf Kulübü", directory.CategoryArts)
	chess := e.club(t, uni, "Satranç Kulübü", directory.CategoryHobby)
	faith := e.club(t, uni, "İnanç Topluluğu", directory.CategoryReligious)
	closed := e.club(t, uni, "Kapanan Kulüp", directory.CategoryHobby)
	e.members.set(photo.Club.OrgID, 42)
	e.members.set(chess.Club.OrgID, directory.MemberThreshold-1)
	o, err := e.orgsMem.GetOrg(ctx, closed.Club.OrgID)
	if err != nil {
		t.Fatal(err)
	}
	o.Status = orgs.StatusSuspended
	if _, err := e.orgsMem.UpdateOrg(ctx, o); err != nil {
		t.Fatal(err)
	}
	rec, ok := e.svc.(directory.Recommendations)
	if !ok {
		t.Fatal("the directory service does not implement Recommendations")
	}
	asked := []uuid.UUID{chess.Club.OrgID, faith.Club.OrgID, uuid.NewV7(), closed.Club.OrgID, photo.Club.OrgID, chess.Club.OrgID}
	got, err := rec.RecommendableClubs(ctx, asked, 10)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]uuid.UUID, len(got))
	for i, c := range got {
		ids[i] = c.Row.OrgID
	}
	if !slices.Equal(ids, []uuid.UUID{chess.Club.OrgID, photo.Club.OrgID}) {
		t.Fatalf("RecommendableClubs = %v, want chess then photo", ids)
	}
	if got[0].MemberCount != nil || got[1].MemberCount == nil || *got[1].MemberCount != 42 {
		t.Fatalf("member counts = %v %v", got[0].MemberCount, got[1].MemberCount)
	}
	if card := directory.ClubCardOf(got[1]); card.ClubID != photo.Club.OrgID.String() || card.Name != "Fotoğraf Kulübü" ||
		card.MembershipPrivate {
		t.Fatalf("ClubCardOf = %+v", card)
	}
	if got, err := rec.RecommendableClubs(ctx, asked, 1); err != nil || len(got) != 1 || got[0].Row.OrgID != chess.Club.OrgID {
		t.Fatalf("limit 1 = %+v, %v", got, err)
	}
	if got, err := rec.RecommendableClubs(ctx, nil, 10); err != nil || len(got) != 0 {
		t.Fatalf("no IDs = %+v, %v", got, err)
	}
}
