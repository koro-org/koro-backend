package identity_test

import (
	"net/http"
	"testing"
	"time"

	"koro/internal/authz"
	"koro/internal/identity"
)

// TestEnrollTotp_RequiresPasswordReauth is 06 H-46: a stolen session cannot
// put TOTP on the thief's device. enrollTotp and confirmTotp take the
// password (the account has no TOTP yet); disableTotp and
// regenerateRecoveryCodes take the TOTP proof.
func TestEnrollTotp_RequiresPasswordReauth(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	u := e.activeUser(t, "h46@example.com")
	_, a := e.login(t, "h46@example.com", identity.ClientMobile)

	_, err := e.svc.EnrollTOTP(ctx, a, identity.ReauthProof{})
	wantErr(t, err, identity.ErrReauthRequired)
	_, err = e.svc.EnrollTOTP(ctx, a, identity.ReauthProof{Password: "Yanlis-Parola-Deneme-1"})
	wantErr(t, err, identity.ErrReauthFailed)
	if _, found, _ := e.store.TOTP(ctx, u.ID); found {
		t.Fatal("an enrollment was stored without the password")
	}
	enr, err := e.svc.EnrollTOTP(ctx, a, pwProof)
	if err != nil {
		t.Fatal(err)
	}
	code := totpCode(t, enr.Secret, e.clk.Now())
	_, err = e.svc.ConfirmTOTP(ctx, a, code, identity.ReauthProof{})
	wantErr(t, err, identity.ErrReauthRequired)
	_, err = e.svc.ConfirmTOTP(ctx, a, code, identity.ReauthProof{Password: "Yanlis-Parola-Deneme-1"})
	wantErr(t, err, identity.ErrReauthFailed)
	wantStatus(t, err, http.StatusForbidden)
	if cur, _ := e.store.User(ctx, u.ID); cur.MFAEnabled {
		t.Fatal("MFA turned on without the password")
	}
	if _, err := e.svc.ConfirmTOTP(ctx, a, code, pwProof); err != nil {
		t.Fatalf("confirm with the password: %v", err)
	}
	for _, act := range []string{string(authz.IdentityMfaEnroll), string(authz.IdentityMfaConfirm)} {
		ev := e.audit.of(act)
		if len(ev) == 0 || ev[len(ev)-1].Metadata["reauth"] != "password" {
			t.Fatalf("%s audit without the reauth kind: %+v", act, ev)
		}
	}
	// Turning it off and new recovery codes need the TOTP proof (R).
	a.MFAVerified = true
	wantErr(t, e.svc.DisableTOTP(ctx, a, identity.ReauthProof{}), identity.ErrReauthRequired)
	wantErr(t, e.svc.DisableTOTP(ctx, a, pwProof), identity.ErrReauthRequired)
	_, err = e.svc.RegenerateRecoveryCodes(ctx, a, pwProof)
	wantErr(t, err, identity.ErrReauthRequired)
	if _, err := e.svc.RegenerateRecoveryCodes(ctx, a, identity.ReauthProof{TOTPCode: e.nextCode(t, enr.Secret)}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.DisableTOTP(ctx, a, identity.ReauthProof{TOTPCode: e.nextCode(t, enr.Secret)}); err != nil {
		t.Fatal(err)
	}
}

// privilegedTarget gives the staff env's target a finance_admin role in
// effect (a second finance_admin keeps the last-holder rule out of the way)
// and a fresh live session.
func (s *staffEnv) privilegedTarget(t *testing.T) {
	t.Helper()
	ra, err := s.grant(t, s.a, s.target.ID, authz.PlatformFinanceAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.svc.ApprovePlatformRole(t.Context(), s.b, ra.ID, proof); err != nil {
		t.Fatal(err)
	}
	s.admin(t, "fin-2@koro.test", authz.PlatformFinanceAdmin)
	res, _ := s.login(t, "target@example.com", identity.ClientMobile)
	s.targetTk = res.Token
}

func (s *staffEnv) targetSessionLive(t *testing.T) {
	t.Helper()
	for _, x := range s.sessionsOf(t, s.target.ID) {
		if x.RevokedAt == nil {
			return
		}
	}
	t.Fatal("the target has no live session left: the first eye acted alone")
}

// TestSuspendPrivilegedUser_FourEyes is 06 H-47 for adminSuspendUser.
func TestSuspendPrivilegedUser_FourEyes(t *testing.T) {
	t.Parallel()
	s := newStaffEnv(t)
	ctx := t.Context()
	s.privilegedTarget(t)

	res, err := s.svc.SuspendUser(ctx, s.a, s.target.ID, "suspicious payouts")
	if err != nil || res.Pending == nil || res.Pending.Status != identity.AccountActionPending || res.Pending.RequestedBy != s.a.UserID {
		t.Fatalf("first eye: %+v %v", res.Pending, err)
	}
	if res.User.User.Status != identity.StatusActive {
		t.Fatal("the first eye suspended the account alone")
	}
	s.targetSessionLive(t)
	// The requester again: 403 four eyes; the request stays pending.
	_, err = s.svc.SuspendUser(ctx, s.a, s.target.ID, "suspicious payouts")
	wantErr(t, err, authz.ErrFourEyes)
	// The second platform_admin approves and the suspension happens.
	res, err = s.svc.SuspendUser(ctx, s.b, s.target.ID, "confirmed, suspend now")
	if err != nil || res.Pending != nil || res.User.User.Status != identity.StatusSuspended {
		t.Fatalf("second eye: %+v %v", res, err)
	}
	s.targetSessionClosed(t, identity.RevokeSuspended)
	rows, _ := s.store.AccountActionsOf(ctx, s.target.ID)
	if len(rows) != 1 || rows[0].Status != identity.AccountActionApproved || *rows[0].ApprovedBy != s.b.UserID {
		t.Fatalf("request rows %+v", rows)
	}
	ev := s.audit.of(string(authz.IdentityUserSuspend))
	if len(ev) != 2 || ev[0].Metadata["result"] != "four_eyes_requested" || ev[1].Metadata["requested_by"] != s.a.UserID.String() {
		t.Fatalf("audit %+v", ev)
	}
	// The DB is the second layer: an approval by the requester is refused.
	r := rows[0]
	r.ApprovedBy = &r.RequestedBy
	wantErr(t, s.store.UpdateAccountAction(ctx, r), authz.ErrFourEyes)
}

// TestRevokeSessionsPrivilegedUser_FourEyes: support may open the request,
// only a platform_admin other than the requester approves it (06 H-47).
func TestRevokeSessionsPrivilegedUser_FourEyes(t *testing.T) {
	t.Parallel()
	s := newStaffEnv(t)
	ctx := t.Context()
	s.privilegedTarget(t)
	sup := s.admin(t, "sup@koro.test", authz.PlatformSupport)

	p, err := s.svc.RevokeUserSessions(ctx, sup, s.target.ID, "account takeover suspected")
	if err != nil || p == nil {
		t.Fatalf("first eye: %+v %v", p, err)
	}
	s.targetSessionLive(t)
	_, err = s.svc.RevokeUserSessions(ctx, sup, s.target.ID, "again from support")
	wantErr(t, err, authz.ErrForbidden) // the second eye is a platform_admin only
	// The request expires after 24 hours: a later call opens a new one.
	s.clk.Advance(25 * time.Hour)
	p2, err := s.svc.RevokeUserSessions(ctx, s.a, s.target.ID, "takeover, second try")
	if err != nil || p2 == nil || p2.ID == p.ID || p2.RequestedBy != s.a.UserID {
		t.Fatalf("after expiry: %+v %v", p2, err)
	}
	s.targetSessionLive(t)
	if p, err := s.svc.RevokeUserSessions(ctx, s.b, s.target.ID, "confirmed takeover"); err != nil || p != nil {
		t.Fatalf("second eye: %+v %v", p, err)
	}
	s.targetSessionClosed(t, identity.RevokeAdmin)
	rows, _ := s.store.AccountActionsOf(ctx, s.target.ID)
	if len(rows) != 2 || rows[0].Status != identity.AccountActionExpired || rows[1].Status != identity.AccountActionApproved {
		t.Fatalf("rows %+v", rows)
	}
	// Against a user without a privileged role: one eye, as before.
	u := s.activeUser(t, "plain@example.com")
	if p, err := s.svc.RevokeUserSessions(ctx, sup, u.ID, "plain account cleanup"); err != nil || p != nil {
		t.Fatalf("plain account: %+v %v", p, err)
	}
}

// TestResetMFAPrivilegedUser_FourEyes: adminResetUserMfa against a
// privileged holder (06 H-47).
func TestResetMFAPrivilegedUser_FourEyes(t *testing.T) {
	t.Parallel()
	s := newStaffEnv(t)
	ctx := t.Context()
	ra, _ := s.grant(t, s.a, s.target.ID, authz.PlatformTalentAuditor)
	if _, err := s.svc.ApprovePlatformRole(ctx, s.b, ra.ID, proof); err != nil {
		t.Fatal(err)
	}
	_, a := s.login(t, "target@example.com", identity.ClientMobile)
	secret, _ := s.enableMFA(t, a)
	_ = secret

	p, err := s.svc.ResetUserMFA(ctx, s.a, s.target.ID, "lost phone, identity checked", proof)
	if err != nil || p == nil || p.Action != identity.AccountResetMFA {
		t.Fatalf("first eye: %+v %v", p, err)
	}
	if cur, _ := s.store.User(ctx, s.target.ID); !cur.MFAEnabled {
		t.Fatal("the first eye reset MFA alone")
	}
	_, err = s.svc.ResetUserMFA(ctx, s.a, s.target.ID, "lost phone, identity checked", proof)
	wantErr(t, err, authz.ErrFourEyes)
	if p, err := s.svc.ResetUserMFA(ctx, s.b, s.target.ID, "confirmed after a call", proof); err != nil || p != nil {
		t.Fatalf("second eye: %+v %v", p, err)
	}
	if cur, _ := s.store.User(ctx, s.target.ID); cur.MFAEnabled {
		t.Fatal("MFA not reset after the approval")
	}
}

// TestSuspend_LastActivePrivilegedHolder_409: the last active holder of a
// privileged role cannot be suspended (06 H-47), not even with two eyes.
func TestSuspend_LastActivePrivilegedHolder_409(t *testing.T) {
	t.Parallel()
	s := newStaffEnv(t)
	ctx := t.Context()
	ra, _ := s.grant(t, s.a, s.target.ID, authz.PlatformFinanceAdmin)
	if _, err := s.svc.ApprovePlatformRole(ctx, s.b, ra.ID, proof); err != nil {
		t.Fatal(err)
	}
	_, err := s.svc.SuspendUser(ctx, s.a, s.target.ID, "only finance admin")
	wantErr(t, err, identity.ErrLastPrivilegedHolder)
	wantStatus(t, err, http.StatusConflict)
	// A second, active finance_admin lifts the block (the request opens).
	s.admin(t, "fin-2@koro.test", authz.PlatformFinanceAdmin)
	res, err := s.svc.SuspendUser(ctx, s.a, s.target.ID, "now there is a second")
	if err != nil || res.Pending == nil {
		t.Fatalf("with a second holder: %+v %v", res, err)
	}
	// An admin cannot open a request against their own privileged account.
	_, err = s.svc.RevokeUserSessions(ctx, s.a, s.a.UserID, "my own sessions")
	wantErr(t, err, identity.ErrUserInvalidState)
}
