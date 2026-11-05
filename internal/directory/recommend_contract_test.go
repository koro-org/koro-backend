package directory_test

import (
	"slices"
	"testing"
	"uuid"

	"koro/internal/directory"
	"koro/internal/orgs"
)

func TestRecommendStore_Contract_Mem(t *testing.T) {
	t.Parallel()
	recommendContract(t, newMemPair)
}

// recommendContract is the store side of the recommendations' club cards
// (BE-174): ListClubs restricted to given club orgs.
func recommendContract(t *testing.T, newStore func(t *testing.T) (directory.Store, world)) {
	t.Helper()
	t.Run("clubs: restricted to org IDs", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		u, err := s.InsertUniversity(ctx, directory.University{
			ID: uuid.NewV7(), OrgID: w.org(t, orgs.KindUniversity, "R Üniversitesi"), YokCode: "Y-" + next(), Name: "R Üniversitesi",
			City: "Ankara", Type: directory.UniversityState, IsActive: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		var ids []uuid.UUID
		for _, n := range []string{"Bilim Kulübü", "Dans Kulübü", "Satranç Kulübü"} {
			c, err := s.InsertClub(ctx, directory.Club{OrgID: w.org(t, orgs.KindClub, n), UniversityID: u.ID,
				Category: directory.CategoryCulture, JoinPolicy: directory.JoinApproval, ClaimStatus: directory.ClaimUnclaimed})
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, c.OrgID)
		}
		rows, err := s.ListClubs(ctx, directory.ClubFilter{OrgIDs: []uuid.UUID{ids[2], ids[0], uuid.NewV7()}, Sort: directory.SortName}, nil, 10)
		if err != nil {
			t.Fatal(err)
		}
		got := make([]uuid.UUID, len(rows))
		for i, r := range rows {
			got[i] = r.OrgID
		}
		if !slices.Equal(got, []uuid.UUID{ids[0], ids[2]}) || rows[0].Name != "Bilim Kulübü" || rows[0].OrgStatus != "active" {
			t.Fatalf("ListClubs(OrgIDs) = %v %+v, want Bilim then Satranç", got, rows)
		}
		if rows, err := s.ListClubs(ctx, directory.ClubFilter{Sort: directory.SortName}, nil, 10); err != nil || len(rows) != 3 {
			t.Fatalf("without OrgIDs = %d rows, %v", len(rows), err)
		}
	})
}
