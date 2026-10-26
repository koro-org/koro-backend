package directory_test

import (
	"slices"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/directory"
	"koro/internal/platform/problem"
	"koro/internal/platform/problem/problemtest"
)

// TestAdminListClubClaims_ByUniversity (BE-127): university_org_id narrows
// the queue to one university's clubs. A representative of two
// universities sees only that panel's claims; a university they do not
// represent is 404; platform staff see the university's sensitive clubs
// too; an actor who is neither stays 403.
func TestAdminListClubClaims_ByUniversity(t *testing.T) {
	t.Parallel()
	w := newClaimWorld(t)
	otherClub := w.env.club(t, w.other, "Öteki Kulüp", directory.CategoryHobby)
	mine := w.claim(t, w.student(t, w.uni, false), w.club.Club.OrgID)
	theirs := w.claim(t, w.student(t, w.other, false), otherClub.Club.OrgID)
	sens := w.claim(t, w.student(t, w.uni, false), w.sens.Club.OrgID)

	both := w.rep(t, w.uni)
	both.Memberships = append(both.Memberships, w.rep(t, w.other).Memberships...)
	only := w.rep(t, w.uni)
	ids := func(a authz.Actor, uni *uuid.UUID) []uuid.UUID {
		t.Helper()
		p, err := w.svc.AdminListClubClaims(t.Context(), a, directory.AdminClaimQuery{UniversityOrg: uni})
		if err != nil {
			t.Fatalf("AdminListClubClaims(%v): %v", uni, err)
		}
		var out []uuid.UUID
		for _, v := range p.Items {
			out = append(out, v.Claim.ID)
		}
		slices.SortFunc(out, func(a, b uuid.UUID) int { return a.Compare(b) })
		return out
	}
	sorted := func(v ...uuid.UUID) []uuid.UUID {
		slices.SortFunc(v, func(a, b uuid.UUID) int { return a.Compare(b) })
		return v
	}
	if got := ids(both, nil); !slices.Equal(got, sorted(mine.ID, theirs.ID)) {
		t.Fatalf("rep of both, no filter = %v", got)
	}
	if got := ids(both, &w.uni.OrgID); !slices.Equal(got, sorted(mine.ID)) {
		t.Fatalf("rep of both, uni panel = %v", got)
	}
	if got := ids(both, &w.other.OrgID); !slices.Equal(got, sorted(theirs.ID)) {
		t.Fatalf("rep of both, other panel = %v", got)
	}
	_, err := w.svc.AdminListClubClaims(t.Context(), only, directory.AdminClaimQuery{UniversityOrg: &w.other.OrgID})
	problemtest.WantErr(t, err, problem.ErrNotFound)
	if got := ids(pad(t), &w.uni.OrgID); !slices.Equal(got, sorted(mine.ID, sens.ID)) {
		t.Fatalf("platform admin, uni = %v", got)
	}
	if got := ids(pad(t), &w.other.OrgID); !slices.Equal(got, sorted(theirs.ID)) {
		t.Fatalf("platform admin, other = %v", got)
	}
	_, err = w.svc.AdminListClubClaims(t.Context(), w.student(t, w.uni, false), directory.AdminClaimQuery{UniversityOrg: &w.uni.OrgID})
	problemtest.WantErr(t, err, authz.ErrForbidden)
}
