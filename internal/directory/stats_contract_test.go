package directory_test

import (
	"errors"
	"testing"
	"uuid"

	"koro/internal/directory"
	"koro/internal/orgs"
)

func TestStatsStore_Contract_Mem(t *testing.T) {
	t.Parallel()
	statsContract(t, newMemPair)
}

// statsContract is the reads of the panels (BE-163): the university of an
// org, its clubs by org ID, and CountClaims, which counts what ListClaims
// lists for the same filter (the rep's scope leaves sensitive clubs out).
func statsContract(t *testing.T, newStore func(t *testing.T) (directory.Store, world)) {
	t.Helper()
	t.Run("university clubs and claim counts", func(t *testing.T) {
		t.Parallel()
		s, w := newStore(t)
		ctx := t.Context()
		uni := func(name string) directory.University {
			t.Helper()
			u, err := s.InsertUniversity(ctx, directory.University{ID: uuid.NewV7(), OrgID: w.org(t, orgs.KindUniversity, name),
				YokCode: "Y-" + next(), Name: name, City: "Bursa", Type: directory.UniversityState, IsActive: true})
			if err != nil {
				t.Fatalf("InsertUniversity: %v", err)
			}
			return u
		}
		club := func(u directory.University, name string) directory.Club {
			t.Helper()
			c, err := s.InsertClub(ctx, directory.Club{OrgID: w.org(t, orgs.KindClub, name), UniversityID: u.ID,
				Category: directory.CategoryCulture, JoinPolicy: directory.JoinApproval, ClaimStatus: directory.ClaimUnclaimed})
			if err != nil {
				t.Fatalf("InsertClub: %v", err)
			}
			return c
		}
		u, other := uni("İstatistik Üniversitesi"), uni("Başka Üniversite")
		c1, c2, sens := club(u, "Birinci Kulüp"), club(u, "İkinci Kulüp"), club(u, "Hassas Kulüp")
		cOther := club(other, "Öteki Kulüp")
		reason, by, now := directory.SensitiveReason("religion"), w.user(t), t0
		sens.IsSensitive, sens.SensitiveReason, sens.SensitivitySetBy, sens.SensitivitySetAt = true, &reason, &by, &now
		if _, err := s.UpdateClub(ctx, sens); err != nil {
			t.Fatal(err)
		}
		for _, c := range []uuid.UUID{c1.OrgID, c2.OrgID, sens.OrgID, cOther.OrgID} {
			if _, err := s.InsertClaim(ctx, directory.ClubClaim{ID: uuid.NewV7(), ClubOrgID: c, ClaimantUserID: w.user(t),
				Status: directory.ClaimPending, RoleInClub: "Başkan"}); err != nil {
				t.Fatal(err)
			}
		}

		if got, err := s.UniversityByOrg(ctx, u.OrgID); err != nil || got.ID != u.ID {
			t.Fatalf("UniversityByOrg = %+v, %v", got, err)
		}
		if _, err := s.UniversityByOrg(ctx, c1.OrgID); !errors.Is(err, directory.ErrNotFound) {
			t.Fatalf("UniversityByOrg of a club org: %v", err)
		}
		clubs, err := s.ClubsOf(ctx, u.ID)
		if err != nil || len(clubs) != 3 {
			t.Fatalf("ClubsOf = %d, %v", len(clubs), err)
		}
		for i, c := range clubs {
			if i > 0 && clubs[i-1].OrgID.String() >= c.OrgID.String() {
				t.Fatalf("ClubsOf is not by org ID: %v", clubs)
			}
			if c.IsSensitive != (c.OrgID == sens.OrgID) {
				t.Fatalf("sensitive flag of %v", c)
			}
		}
		for name, f := range map[string]directory.ClaimFilter{
			"every pending":  {Status: directory.ClaimPending},
			"rep scope":      {Status: directory.ClaimPending, RepUniversityOrgs: []uuid.UUID{u.OrgID}},
			"one university": {Status: directory.ClaimPending, UniversityOrg: &u.OrgID},
			"approved":       {Status: directory.ClaimApproved},
		} {
			n, err := s.CountClaims(ctx, f)
			if err != nil {
				t.Fatal(err)
			}
			rows, err := s.ListClaims(ctx, f, nil, 100)
			if err != nil || n != len(rows) {
				t.Fatalf("%s: CountClaims %d, ListClaims %d, %v", name, n, len(rows), err)
			}
		}
		if n, _ := s.CountClaims(ctx, directory.ClaimFilter{Status: directory.ClaimPending, RepUniversityOrgs: []uuid.UUID{u.OrgID}}); n != 2 {
			t.Fatalf("rep scope = %d, want 2 (the sensitive club's claim is platform staff's)", n)
		}
	})
}
