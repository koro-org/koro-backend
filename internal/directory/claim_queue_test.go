package directory_test

import (
	"encoding/json"
	"net/http"
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

// TestAdminListClubClaims_Approver (BE-184): every row of the platform
// queue names the approver adminGetClubClaim names (a rep's club, a
// sensitive club, a rep's own claim, a university without a rep, the
// escalation), from one read of the universities and one of their reps;
// the representative's own list leaves it out.
func TestAdminListClubClaims_Approver(t *testing.T) {
	t.Parallel()
	w := newClaimWorld(t)
	w.rep(t, w.uni)
	otherClub := w.env.club(t, w.other, "Öteki Kulüp", directory.CategoryHobby)
	onlyRep := w.rep(t, w.other)
	w.ver.set(onlyRep.UserID, w.other.ID)
	repStudent := onlyRep
	repStudent.StudentStatus, repStudent.UniversityID = authz.StudentVerified, new(w.other.ID)
	bare := w.university(t, "C-3", "Temsilcisiz Üniversite", "Van")
	bareClub := w.env.club(t, bare, "Temsilcisiz Kulüp", directory.CategoryHobby)

	want := map[uuid.UUID]string{
		w.claim(t, w.student(t, w.uni, false), w.club.Club.OrgID).ID:      authz.ClaimApproverRep,
		w.claim(t, w.student(t, w.uni, false), w.sens.Club.OrgID).ID:      authz.ClaimApproverPlatformAdmin,
		w.claim(t, w.student(t, w.other, false), otherClub.Club.OrgID).ID: authz.ClaimApproverRep,
		w.claim(t, repStudent, otherClub.Club.OrgID).ID:                   authz.ClaimApproverPlatformAdmin, // the only rep is the claimant
		w.claim(t, w.student(t, bare, false), bareClub.Club.OrgID).ID:     authz.ClaimApproverPlatformAdmin,
	}
	list := func(a authz.Actor) []directory.ClaimView {
		t.Helper()
		p, err := w.svc.AdminListClubClaims(t.Context(), a, directory.AdminClaimQuery{})
		if err != nil {
			t.Fatalf("AdminListClubClaims: %v", err)
		}
		return p.Items
	}

	w.orgs.roleReads, w.store.uniReads = 0, 0
	rows := list(pad(t))
	if w.orgs.roleReads != 1 || w.store.uniReads != 1 {
		t.Fatalf("reads for %d rows: %d of the reps, %d of the universities; want 1 each", len(rows), w.orgs.roleReads, w.store.uniReads)
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %d, want %d", len(rows), len(want))
	}
	for _, v := range rows {
		if v.Approver != want[v.Claim.ID] {
			t.Errorf("claim of %s: approver %q, want %q", v.ClubName, v.Approver, want[v.Claim.ID])
		}
		got, err := w.svc.AdminGetClubClaim(t.Context(), pad(t), v.Claim.ID)
		if err != nil || got.Approver != v.Approver {
			t.Errorf("claim of %s: list says %q, detail %q, %v", v.ClubName, v.Approver, got.Approver, err)
		}
	}
	for _, v := range list(persona(t, "support")) {
		if v.Approver != want[v.Claim.ID] {
			t.Errorf("support, claim of %s: approver %q", v.ClubName, v.Approver)
		}
	}

	// The representative's list has its rows, without the field or the reads.
	w.orgs.roleReads, w.store.uniReads = 0, 0
	repRows := list(onlyRep)
	if len(repRows) == 0 || w.orgs.roleReads != 0 || w.store.uniReads != 0 {
		t.Fatalf("rep list: %d rows, %d rep reads, %d university reads", len(repRows), w.orgs.roleReads, w.store.uniReads)
	}
	for _, v := range repRows {
		if v.Approver != "" {
			t.Fatalf("rep list approver = %q", v.Approver)
		}
	}

	// The wire shape of the platform queue.
	app := newHTTP(t, w.env)
	rec := call(t, app, "platform_admin", http.MethodGet, "/api/v1/admin/club-claims", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("adminListClubClaims = %d %s", rec.Code, rec.Body)
	}
	var body directory.AdminClubClaimList
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != len(want) {
		t.Fatalf("items = %d", len(body.Items))
	}
	for _, it := range body.Items {
		if it.Approver != want[uuid.MustParse(it.ClaimID)] {
			t.Errorf("claim %s: approver %q", it.ClaimID, it.Approver)
		}
	}

	// Past the escalation platform staff decide them all (06 H-58).
	w.clk.Set(w.clk.Now().Add(directory.ClaimEscalateAfter))
	for _, v := range list(pad(t)) {
		if v.Approver != authz.ClaimApproverPlatformAdmin {
			t.Errorf("escalated claim of %s: approver %q", v.ClubName, v.Approver)
		}
	}
}
