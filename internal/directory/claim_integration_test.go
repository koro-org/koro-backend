//go:build integration

package directory_test

import (
	"errors"
	"testing"
	"uuid"

	"koro/internal/audit/audittest"
	"koro/internal/authz"
	"koro/internal/directory"
	"koro/internal/identity"
	"koro/internal/orgs"
	"koro/internal/platform/clock"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/problem/problemtest"
	"koro/internal/platform/queue"
	"koro/internal/platform/testutil"
)

// TestDecideClaim_GrantFailure_RollsBackClaim runs the claim approval on
// PostgreSQL with the real orgs module: a grant orgs refuses (the claimant
// is a minor) and a failure after the grant (the audit write) both leave
// nothing behind — no membership, no claim review, no claimed club, no
// outbox row; then the approval commits all of them together (01 §6.3).
func TestDecideClaim_GrantFailure_RollsBackClaim(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	ctx := t.Context()
	clk := clock.NewFake(t0)
	log := obs.Discard()
	out := outbox.New(outbox.Deps{Store: outbox.NewGormStore(d.ORM), Tx: d.Tx, Subs: outbox.NewRegistry(), Queue: queue.NewMem(), Clock: clk, Log: log})
	idem := idempotency.New(idempotency.Deps{Tx: d.Tx, Store: idempotency.NewGormStore(d.ORM), Clock: clk, Log: log})
	al := &audittest.Recorder{}
	ver := &verifications{m: map[uuid.UUID]identity.EduVerification{}}
	users := &userDir{m: map[uuid.UUID]identity.UserSummary{}}
	om, err := orgs.New(orgs.Deps{Store: orgs.NewGormStore(d.ORM), Tx: d.Tx, Outbox: out, Audit: al, Idempotency: idem, Clock: clk,
		Log: log, Students: ver, Users: users})
	if err != nil {
		t.Fatal(err)
	}
	store := directory.NewGormStore(d.ORM)
	mod, err := directory.New(directory.Deps{
		Store: store, Tx: d.Tx, Outbox: out, Queue: queue.NewMem(), Audit: al, Orgs: om.Service, Idempotency: idem,
		Clock: clk, Log: log, Reauth: reauth{}, Students: ver, Users: users,
	})
	if err != nil {
		t.Fatal(err)
	}
	svc := mod.Service
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := d.Pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if _, err := svc.ImportReference(ctx, directory.ReferenceData{
		Universities: []directory.RefUniversity{{YokCode: "PG-C", Name: "Pg Talep Üniversitesi", City: "Bursa", Type: directory.UniversityState}},
	}); err != nil {
		t.Fatal(err)
	}
	u, err := store.UniversityByYokCode(ctx, "PG-C")
	if err != nil {
		t.Fatal(err)
	}
	pad := authz.Actor{Kind: authz.KindUser, UserID: pgUser(d)(t), MFAVerified: true, PlatformRoles: []authz.PlatformRole{authz.PlatformAdmin}}
	club, err := svc.AdminCreateClub(ctx, pad, directory.ClubInput{UniversityID: u.ID, Name: "Pg Satranç Kulübü",
		Category: directory.CategoryHobby, JoinPolicy: directory.JoinApproval})
	if err != nil {
		t.Fatal(err)
	}
	clubID := club.Club.OrgID
	claimant := pgUser(d)(t)
	ver.set(claimant, u.ID)
	users.set(identity.UserSummary{ID: claimant, Status: identity.StatusActive})
	student := authz.Actor{Kind: authz.KindUser, UserID: claimant, SessionID: uuid.NewV7(), StudentStatus: authz.StudentVerified,
		UniversityID: new(u.ID)}
	v, err := svc.CreateClubClaim(ctx, student, clubID, directory.ClaimInput{RoleInClub: "Başkan", EvidenceText: "Tutanak"})
	if err != nil {
		t.Fatal(err)
	}
	claimID := v.Claim.ID

	untouched := func(stage string) {
		t.Helper()
		if n := count(`SELECT count(*) FROM org_members WHERE org_id = $1 AND user_id = $2`, clubID, claimant); n != 0 {
			t.Fatalf("%s: %d membership rows", stage, n)
		}
		if n := count(`SELECT count(*) FROM club_claims WHERE id = $1 AND status = 'pending' AND reviewed_by IS NULL`, claimID); n != 1 {
			t.Fatalf("%s: the claim is not pending", stage)
		}
		if n := count(`SELECT count(*) FROM clubs WHERE org_id = $1 AND claim_status = 'unclaimed'`, clubID); n != 1 {
			t.Fatalf("%s: the club is claimed", stage)
		}
		if n := count(`SELECT count(*) FROM outbox WHERE topic IN ('club_claim.decided', 'membership.started')`); n != 0 {
			t.Fatalf("%s: %d outbox rows", stage, n)
		}
	}

	// 1. orgs refuses the grant (the claimant is a minor after all).
	users.set(identity.UserSummary{ID: claimant, Status: identity.StatusActive, IsMinor: true})
	_, err = svc.AdminDecideClubClaim(ctx, pad, claimID, directory.ClaimDecision{Decision: directory.ClaimApproved})
	problemtest.WantProblem(t, err, 422, "orgs.club_admin_requires_adult_student")
	untouched("grant refused")

	// 2. The grant succeeds, then the decision's audit write fails: the
	// membership orgs wrote rolls back with the rest.
	users.set(identity.UserSummary{ID: claimant, Status: identity.StatusActive})
	boom := errors.New("audit down")
	al.Fail(boom)
	_, err = svc.AdminDecideClubClaim(ctx, pad, claimID, directory.ClaimDecision{Decision: directory.ClaimApproved})
	if !errors.Is(err, boom) {
		t.Fatalf("decision with a failing audit = %v", err)
	}
	al.Fail(nil)
	untouched("audit failed after the grant")

	// 3. The approval commits the membership, the review, the club and the event together.
	got, err := svc.AdminDecideClubClaim(ctx, pad, claimID, directory.ClaimDecision{Decision: directory.ClaimApproved, Note: "Tamam"})
	if err != nil || got.Claim.Status != directory.ClaimApproved {
		t.Fatalf("approval = %+v, %v", got.Claim, err)
	}
	if n := count(`SELECT count(*) FROM org_members WHERE org_id = $1 AND user_id = $2 AND role = 'club_admin' AND status = 'active'
		AND source = 'claim'`, clubID, claimant); n != 1 {
		t.Fatalf("club_admin rows = %d", n)
	}
	if n := count(`SELECT count(*) FROM club_claims WHERE id = $1 AND status = 'approved' AND reviewed_by = $2`, claimID, pad.UserID); n != 1 {
		t.Fatal("the claim is not approved by the admin")
	}
	if n := count(`SELECT count(*) FROM clubs WHERE org_id = $1 AND claim_status = 'claimed'`, clubID); n != 1 {
		t.Fatal("the club is not claimed")
	}
	if n := count(`SELECT count(*) FROM outbox WHERE topic = 'club_claim.decided' AND aggregate_id = $1`, claimID); n != 1 {
		t.Fatalf("club_claim.decided rows = %d", n)
	}

	// The next request's Actor carries the new role: no second claim (409).
	student.Memberships = []authz.Membership{{OrgID: clubID, OrgKind: authz.OrgClub, Role: authz.MemberClubAdmin}}
	_, err = svc.CreateClubClaim(ctx, student, clubID, directory.ClaimInput{RoleInClub: "YK üyesi", EvidenceText: "Tutanak"})
	problemtest.WantProblem(t, err, 409, "directory.invalid_state")

	// The CHECK backs Y6: the claimant cannot be the reviewer.
	other := pgUser(d)(t)
	ver.set(other, u.ID)
	users.set(identity.UserSummary{ID: other, Status: identity.StatusActive})
	oc, err := svc.CreateClubClaim(ctx, authz.Actor{Kind: authz.KindUser, UserID: other, SessionID: uuid.NewV7(),
		StudentStatus: authz.StudentVerified, UniversityID: new(u.ID)}, clubID, directory.ClaimInput{RoleInClub: "YK üyesi", EvidenceText: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, `UPDATE club_claims SET status = 'rejected', reviewed_by = claimant_user_id, reviewed_at = now()
		WHERE id = $1`, oc.Claim.ID); err == nil {
		t.Fatal("club_claims_reviewer_chk let the claimant review their own claim")
	}
}
