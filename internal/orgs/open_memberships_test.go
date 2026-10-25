package orgs_test

import (
	"testing"

	"koro/internal/orgs"
)

// TestOpenMembershipsOf_PendingAndActive: the reader's relations read
// (feed listMyOrgRelations, BE-122) sees a pending join request and an
// active membership, and nothing once the request is withdrawn.
func TestOpenMembershipsOf_PendingAndActive(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	open := e.club(t, "Açık Kulüp", orgs.JoinOpen, false)
	approval := e.club(t, "Onaylı Kulüp", orgs.JoinApproval, false)
	s := e.student(false)
	if _, err := e.svc.JoinClub(ctx, s, open, ""); err != nil {
		t.Fatal(err)
	}
	req, err := e.svc.JoinClub(ctx, s, approval, "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := e.svc.OpenMembershipsOf(ctx, s.UserID)
	if err != nil || len(got) != 2 || got[open] != orgs.MemberActive || got[approval] != orgs.MemberPending {
		t.Fatalf("OpenMembershipsOf = %v, %v", got, err)
	}
	if err := e.svc.LeaveMembership(ctx, s, req.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := e.svc.OpenMembershipsOf(ctx, s.UserID); err != nil || len(got) != 1 || got[open] != orgs.MemberActive {
		t.Fatalf("after withdrawing = %v, %v", got, err)
	}
	if got, err := e.svc.OpenMembershipsOf(ctx, e.student(false).UserID); err != nil || len(got) != 0 {
		t.Fatalf("stranger = %v, %v", got, err)
	}
}
