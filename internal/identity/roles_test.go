package identity_test

import (
	"net/http"
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/platform/httpx"
	"koro/internal/platform/problem/problemtest"
	"koro/internal/platform/token"
)

// staffEnv is an identity module with two bootstrap platform admins (the
// minimum of 03 §6.1) and a regular target user with a live session.
type staffEnv struct {
	*env
	a, b     authz.Actor // platform admins (MFA-verified sessions)
	target   identity.User
	targetTk string
}

func newStaffEnv(t *testing.T) *staffEnv {
	t.Helper()
	e := newEnv(t)
	s := &staffEnv{env: e}
	s.a = s.admin(t, "admin-a@koro.test")
	s.b = s.admin(t, "admin-b@koro.test")
	s.target = e.activeUser(t, "target@example.com")
	res, _ := e.login(t, "target@example.com", identity.ClientMobile)
	s.targetTk = res.Token
	e.q.Reset()
	return s
}

// admin creates an account holding role in effect through a bootstrap row
// (the only privileged row without a second approver, 02 §5.7).
func (s *staffEnv) admin(t *testing.T, email string, roles ...authz.PlatformRole) authz.Actor {
	t.Helper()
	if len(roles) == 0 {
		roles = []authz.PlatformRole{authz.PlatformAdmin}
	}
	u := s.activeUser(t, email)
	now := s.clk.Now()
	for _, r := range roles {
		ra := identity.RoleAssignment{ID: uuid.NewV7(), UserID: u.ID, Role: r, Reason: "bootstrap", Status: identity.RoleActive,
			RequestedAt: now, GrantedAt: &now}
		if err := s.store.InsertRoleAssignment(t.Context(), ra); err != nil {
			t.Fatal(err)
		}
	}
	_, a := s.login(t, email, identity.ClientWeb)
	a.PlatformRoles, a.MFAVerified = roles, true
	return a
}

var proof = identity.ReauthProof{Password: goodPassword}

func (s *staffEnv) grant(t *testing.T, by authz.Actor, target uuid.UUID, role authz.PlatformRole) (identity.RoleAssignment, error) {
	t.Helper()
	return s.svc.GrantPlatformRole(t.Context(), by, identity.RoleGrant{UserID: target, Role: role, Reason: "needs the role for the pilot", Proof: proof})
}

// targetRoles is what the actor resolver would see for the target now.
func (s *staffEnv) targetRoles(t *testing.T) []authz.PlatformRole {
	t.Helper()
	roles, err := s.store.PlatformRoles(t.Context(), s.target.ID)
	if err != nil {
		t.Fatal(err)
	}
	return roles
}

func (s *staffEnv) targetSessionClosed(t *testing.T, reason identity.RevokeReason) {
	t.Helper()
	cred := httpx.Credential{Transport: httpx.TransportBearer, TokenHash: [32]byte(token.Hash(s.targetTk))}
	_, err := s.svc.Authenticate(t.Context(), cred)
	problemtest.WantErr(t, err, identity.ErrUnauthenticated)
	cur, found, _ := s.store.SessionByTokenHash(t.Context(), token.Hash(s.targetTk))
	if !found || cur.RevokedAt == nil || cur.RevokeReason != reason {
		t.Fatalf("target session: found %v, revoked %v, reason %q, want %q", found, cur.RevokedAt, cur.RevokeReason, reason)
	}
	for _, x := range s.sessionsOf(t, s.target.ID) {
		if x.RevokedAt == nil {
			t.Fatalf("target session %s still live", x.ID)
		}
	}
}

func TestGrantPrivilegedRole_SameAdminApprove_403FourEyes(t *testing.T) {
	t.Parallel()
	s := newStaffEnv(t)
	ctx := t.Context()
	ra, err := s.grant(t, s.a, s.target.ID, authz.PlatformFinanceAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if ra.Status != identity.RolePending || *ra.RequestedBy != s.a.UserID || ra.GrantedAt != nil {
		t.Fatalf("request %+v", ra)
	}
	_, err = s.svc.ApprovePlatformRole(ctx, s.a, ra.ID, proof)
	problemtest.WantErr(t, err, authz.ErrFourEyes)
	wantStatus(t, err, http.StatusForbidden)
	if got, _ := s.store.RoleAssignment(ctx, ra.ID); got.Status != identity.RolePending {
		t.Fatalf("status after the refused approval: %s", got.Status)
	}
	// The DB is the second layer (02 §5.7 …_four_eyes_chk): the store refuses too.
	now := s.clk.Now()
	bad := ra
	bad.Status, bad.ApprovedBy, bad.ApprovedAt, bad.GrantedAt = identity.RoleActive, ra.RequestedBy, &now, &now
	problemtest.WantErr(t, s.store.UpdateRoleAssignment(ctx, bad), authz.ErrFourEyes)
	if len(s.targetRoles(t)) != 0 {
		t.Fatal("the refused approval granted the role")
	}
}

func TestGrantPrivilegedRole_SecondAdminApprove_Active(t *testing.T) {
	t.Parallel()
	s := newStaffEnv(t)
	ctx := t.Context()
	ra, err := s.grant(t, s.a, s.target.ID, authz.PlatformFinanceAdmin)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.svc.ApprovePlatformRole(ctx, s.b, ra.ID, proof)
	if err != nil {
		t.Fatalf("approval by the second admin: %v", err)
	}
	if got.Status != identity.RoleActive || *got.ApprovedBy != s.b.UserID || got.GrantedAt == nil {
		t.Fatalf("approved %+v", got)
	}
	if roles := s.targetRoles(t); !slices.Equal(roles, []authz.PlatformRole{authz.PlatformFinanceAdmin}) {
		t.Fatalf("target roles %v", roles)
	}
	s.targetSessionClosed(t, identity.RevokeRoleChange)
	// A second approval of the same row is a state conflict.
	_, err = s.svc.ApprovePlatformRole(ctx, s.b, ra.ID, proof)
	problemtest.WantErr(t, err, identity.ErrRoleInvalidState)
	// Both steps are audited with the reauth kind and alert every admin.
	ev := s.audit.of(string(authz.IdentityPlatformRoleGrant))
	if len(ev) != 2 || ev[1].Metadata["result"] != "approved" || ev[1].Metadata["reauth"] != "password" {
		t.Fatalf("audit %+v", ev)
	}
	s.runRoleAlerts(t)
	var to []string
	for _, m := range s.mail.sent() {
		if m.Template == "identity.role_alert" {
			to = append(to, m.ToEmail+"/"+m.Data["kind"].(string))
		}
	}
	slices.Sort(to)
	want := []string{"admin-a@koro.test/role_granted", "admin-a@koro.test/role_requested",
		"admin-b@koro.test/role_granted", "admin-b@koro.test/role_requested"}
	if !slices.Equal(to, want) {
		t.Fatalf("role alerts %v, want %v", to, want)
	}
}

// runRoleAlerts runs the queued role alert tasks.
func (e *env) runRoleAlerts(t *testing.T) {
	t.Helper()
	for _, x := range e.q.Tasks() {
		if x.Type != identity.SendRoleAlertTaskName {
			continue
		}
		for _, tk := range e.mod.Tasks() {
			if tk.Name == identity.SendRoleAlertTaskName {
				if err := tk.Handle(workerCtx(t), x.Payload); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

// TestGrantPrivilegedRole_PendingGrantsNothing is 06 H-33: a request
// waiting for the second admin grants no permission anywhere.
func TestGrantPrivilegedRole_PendingGrantsNothing(t *testing.T) {
	t.Parallel()
	s := newStaffEnv(t)
	ctx := t.Context()
	if _, err := s.grant(t, s.a, s.target.ID, authz.PlatformAdmin); err != nil {
		t.Fatal(err)
	}
	if roles := s.targetRoles(t); len(roles) != 0 {
		t.Fatalf("pending request in effect: %v", roles)
	}
	cred := httpx.Credential{Transport: httpx.TransportBearer, TokenHash: [32]byte(token.Hash(s.targetTk))}
	p, err := s.svc.Authenticate(ctx, cred)
	if err != nil {
		t.Fatalf("a request does not close the target's sessions: %v", err)
	}
	if len(p.PlatformRoles) != 0 {
		t.Fatalf("principal roles %v", p.PlatformRoles)
	}
	admins, _ := s.mod.PlatformAdmins(ctx)
	if slices.Contains(admins, s.target.ID) {
		t.Fatal("pending platform_admin counted as admin")
	}
	// The target, as a would-be admin, is refused by authz.
	a := authz.Actor{Kind: authz.KindUser, UserID: s.target.ID, SessionID: p.SessionID, PlatformRoles: p.PlatformRoles, MFAVerified: true}
	_, err = s.svc.SearchUsers(ctx, a, identity.AdminUserQuery{})
	problemtest.WantErr(t, err, authz.ErrForbidden)
}

func TestGrantRole_Self_403(t *testing.T) {
	t.Parallel()
	s := newStaffEnv(t)
	ctx := t.Context()
	_, err := s.grant(t, s.a, s.a.UserID, authz.PlatformModerator)
	problemtest.WantErr(t, err, authz.ErrForbidden)
	_, err = s.grant(t, s.a, s.a.UserID, authz.PlatformFinanceAdmin)
	problemtest.WantErr(t, err, authz.ErrForbidden)
	// Approving a request for oneself (requested by another admin): 403 (Y6).
	ra, err := s.grant(t, s.b, s.a.UserID, authz.PlatformFinanceAdmin)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.svc.ApprovePlatformRole(ctx, s.a, ra.ID, proof)
	problemtest.WantErr(t, err, authz.ErrForbidden)
	// Nor revoking one's own role.
	own, _ := s.store.RoleAssignmentsOf(ctx, s.a.UserID)
	_, err = s.svc.RevokePlatformRole(ctx, s.a, own[0].ID, "leaving the team now", proof)
	problemtest.WantErr(t, err, authz.ErrForbidden)
	// The DB refuses a self-assignment as well (…_self_chk).
	me := s.a.UserID
	now := s.clk.Now()
	err = s.store.InsertRoleAssignment(ctx, identity.RoleAssignment{ID: uuid.NewV7(), UserID: me, Role: authz.PlatformSupport,
		Reason: "self service", Status: identity.RoleActive, RequestedBy: &me, RequestedAt: now, GrantedAt: &now})
	problemtest.WantErr(t, err, identity.ErrForbidden)
	// A non-admin is refused before anything is read, and nothing is audited.
	_, a := s.login(t, "target@example.com", identity.ClientMobile)
	_, err = s.grant(t, a, s.target.ID, authz.PlatformModerator)
	problemtest.WantErr(t, err, authz.ErrForbidden)
}

func TestGrantRole_RevokesTargetSessions(t *testing.T) {
	t.Parallel()
	s := newStaffEnv(t)
	ra, err := s.grant(t, s.a, s.target.ID, authz.PlatformModerator)
	if err != nil {
		t.Fatal(err)
	}
	// moderator, support, dpo take effect at once (one admin, 02 §5.7).
	if ra.Status != identity.RoleActive || ra.ApprovedBy != nil || ra.GrantedAt == nil {
		t.Fatalf("direct grant %+v", ra)
	}
	s.targetSessionClosed(t, identity.RevokeRoleChange)
	if roles := s.targetRoles(t); !slices.Equal(roles, []authz.PlatformRole{authz.PlatformModerator}) {
		t.Fatalf("roles %v", roles)
	}
	// A second open assignment of the same role: 409.
	_, err = s.grant(t, s.b, s.target.ID, authz.PlatformModerator)
	problemtest.WantErr(t, err, identity.ErrRoleAssignmentOpen)
	// The new session of an MFA role is short (03 §6.4: 30 min idle / 12 h).
	res, _ := s.login(t, "target@example.com", identity.ClientMobile)
	if d := res.Session.AbsoluteExpiresAt.Sub(res.Session.CreatedAt); d != 12*time.Hour {
		t.Fatalf("absolute lifetime %s", d)
	}
	// Direct revocation of a non-privileged role closes the sessions again.
	got, err := s.svc.RevokePlatformRole(t.Context(), s.b, ra.ID, "no longer moderating", proof)
	if err != nil || got.Status != identity.RoleRevoked || *got.RevokedBy != s.b.UserID {
		t.Fatalf("revoke %+v %v", got, err)
	}
	s.targetTk = res.Token
	s.targetSessionClosed(t, identity.RevokeRoleChange)
}

func TestRevokePrivilegedRole_FourEyes(t *testing.T) {
	t.Parallel()
	s := newStaffEnv(t)
	ctx := t.Context()
	ra, _ := s.grant(t, s.a, s.target.ID, authz.PlatformFinanceAdmin)
	if _, err := s.svc.ApprovePlatformRole(ctx, s.b, ra.ID, proof); err != nil {
		t.Fatal(err)
	}
	res, _ := s.login(t, "target@example.com", identity.ClientMobile)
	s.targetTk = res.Token

	req, err := s.svc.RevokePlatformRole(ctx, s.a, ra.ID, "left the finance team", proof)
	if err != nil || req.Status != identity.RoleRevokePending {
		t.Fatalf("revoke request %+v %v", req, err)
	}
	// revoke_pending is still in effect (06 H-33) and the session lives.
	if roles := s.targetRoles(t); !slices.Equal(roles, []authz.PlatformRole{authz.PlatformFinanceAdmin}) {
		t.Fatalf("roles while revoke_pending: %v", roles)
	}
	_, err = s.svc.RevokePlatformRole(ctx, s.a, ra.ID, "confirming myself", proof)
	problemtest.WantErr(t, err, authz.ErrFourEyes)
	got, err := s.svc.RevokePlatformRole(ctx, s.b, ra.ID, "confirmed by the second admin", proof)
	if err != nil || got.Status != identity.RoleRevoked || *got.RevokedBy != s.b.UserID || got.RevokeReason != "left the finance team" {
		t.Fatalf("confirmation %+v %v", got, err)
	}
	if len(s.targetRoles(t)) != 0 {
		t.Fatal("revoked role still in effect")
	}
	s.targetSessionClosed(t, identity.RevokeRoleChange)
	_, err = s.svc.RevokePlatformRole(ctx, s.b, ra.ID, "once more please", proof)
	problemtest.WantErr(t, err, identity.ErrRoleInvalidState)
}

func TestRevokePendingRequest_Rejected(t *testing.T) {
	t.Parallel()
	s := newStaffEnv(t)
	ra, _ := s.grant(t, s.a, s.target.ID, authz.PlatformTalentAuditor)
	got, err := s.svc.RevokePlatformRole(t.Context(), s.b, ra.ID, "not needed after all", proof)
	if err != nil || got.Status != identity.RoleRejected || *got.RejectedBy != s.b.UserID {
		t.Fatalf("reject %+v %v", got, err)
	}
	_, err = s.svc.ApprovePlatformRole(t.Context(), s.b, ra.ID, proof)
	problemtest.WantErr(t, err, identity.ErrRoleInvalidState) // a rejected request cannot be approved
}

func TestRevokeRole_LastPlatformAdmin_409(t *testing.T) {
	t.Parallel()
	s := newStaffEnv(t)
	ctx := t.Context()
	bRows, _ := s.store.RoleAssignmentsOf(ctx, s.b.UserID)
	aRows, _ := s.store.RoleAssignmentsOf(ctx, s.a.UserID)
	if _, err := s.svc.RevokePlatformRole(ctx, s.a, bRows[0].ID, "admin b is leaving", proof); err != nil {
		t.Fatalf("first revocation request: %v", err)
	}
	// Only a is left who is not on the way out: a's revocation is refused.
	_, err := s.svc.RevokePlatformRole(ctx, s.b, aRows[0].ID, "admin a is leaving", proof)
	problemtest.WantErr(t, err, identity.ErrLastPlatformAdmin)
	wantStatus(t, err, http.StatusConflict)
}

func TestGrantRole_TalentAuditorLimit_409(t *testing.T) {
	t.Parallel()
	s := newStaffEnv(t)
	u1 := s.activeUser(t, "aud1@example.com")
	u2 := s.activeUser(t, "aud2@example.com")
	for _, u := range []identity.User{u1, u2} {
		if _, err := s.grant(t, s.a, u.ID, authz.PlatformTalentAuditor); err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.grant(t, s.a, s.target.ID, authz.PlatformTalentAuditor)
	problemtest.WantErr(t, err, identity.ErrTalentAuditorLimit)
}

func TestListPlatformRoles_NamesAndFilters(t *testing.T) {
	t.Parallel()
	s := newStaffEnv(t)
	ctx := t.Context()
	ra, _ := s.grant(t, s.a, s.target.ID, authz.PlatformSupport)
	p, err := s.svc.PlatformRoleAssignments(ctx, s.a, identity.RoleQuery{Role: authz.PlatformSupport})
	if err != nil || len(p.Items) != 1 || p.Items[0].ID != ra.ID || p.Users[s.target.ID].FirstName == "" {
		t.Fatalf("list %+v %v", p, err)
	}
	all, _ := s.svc.PlatformRoleAssignments(ctx, s.a, identity.RoleQuery{})
	if len(all.Items) != 3 {
		t.Fatalf("%d open assignments, want 3", len(all.Items))
	}
	support := s.admin(t, "support@koro.test", authz.PlatformSupport)
	_, err = s.svc.PlatformRoleAssignments(ctx, support, identity.RoleQuery{})
	problemtest.WantErr(t, err, authz.ErrForbidden)
}
