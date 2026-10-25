package identity_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/platform/problem/problemtest"
)

func TestSuspendUser_RevokesSessionsWithReason(t *testing.T) {
	t.Parallel()
	s := newStaffEnv(t)
	ctx := t.Context()
	_, _ = s.login(t, "target@example.com", identity.ClientWeb)

	res, err := s.svc.SuspendUser(ctx, s.a, s.target.ID, "  spam reports  ")
	if err != nil || res.Pending != nil {
		t.Fatal(err, res.Pending)
	}
	v := res.User
	if v.User.Status != identity.StatusSuspended || v.User.SuspendedAt == nil || v.User.SuspensionReason != "spam reports" {
		t.Fatalf("suspended %+v", v.User)
	}
	s.targetSessionClosed(t, identity.RevokeSuspended)
	if n := len(s.sessionsOf(t, s.target.ID)); n != 2 {
		t.Fatalf("%d target sessions, want both closed", n)
	}
	// The account can no longer sign in (the same 401 as a wrong password).
	_, err = s.svc.CreateSession(ctx, authz.Anonymous(), identity.Login{Email: "target@example.com", Password: goodPassword})
	problemtest.WantErr(t, err, identity.ErrInvalidCredentials)
	ev := s.audit.of(string(authz.IdentityUserSuspend))
	if len(ev) != 1 || ev[0].Reason != "spam reports" || ev[0].Metadata["sessions_revoked"] != 2 {
		t.Fatalf("audit %+v", ev)
	}
	// Twice: a state conflict; a too short reason: 422; own account: 409.
	_, err = s.svc.SuspendUser(ctx, s.a, s.target.ID, "again please")
	problemtest.WantErr(t, err, identity.ErrUserInvalidState)
	_, err = s.svc.UnsuspendUser(ctx, s.a, s.target.ID, "ok")
	wantStatus(t, err, http.StatusUnprocessableEntity)
	_, err = s.svc.SuspendUser(ctx, s.a, s.a.UserID, "testing myself")
	problemtest.WantErr(t, err, identity.ErrUserInvalidState)
	// support may not suspend (03 §5.1: pad only).
	sup := s.admin(t, "sup@koro.test", authz.PlatformSupport)
	_, err = s.svc.SuspendUser(ctx, sup, s.target.ID, "support tries this")
	problemtest.WantErr(t, err, authz.ErrForbidden)

	v, err = s.svc.UnsuspendUser(ctx, s.a, s.target.ID, "appeal accepted")
	if err != nil || v.User.Status != identity.StatusActive || v.User.SuspendedAt != nil {
		t.Fatalf("unsuspend %+v %v", v.User, err)
	}
	if _, err := s.svc.CreateSession(ctx, authz.Anonymous(), identity.Login{Email: "target@example.com", Password: goodPassword}); err != nil {
		t.Fatalf("login after unsuspend: %v", err)
	}
}

func TestAdminSearchUsers_Audited(t *testing.T) {
	t.Parallel()
	s := newStaffEnv(t)
	ctx := t.Context()
	sup := s.admin(t, "sup@koro.test", authz.PlatformSupport)

	p, err := s.svc.SearchUsers(ctx, sup, identity.AdminUserQuery{Q: "TARGET@example.com"})
	if err != nil || len(p.Items) != 1 || p.Items[0].ID != s.target.ID {
		t.Fatalf("search by e-mail: %+v %v", p, err)
	}
	p, err = s.svc.SearchUsers(ctx, sup, identity.AdminUserQuery{Q: "yılm", Status: identity.StatusActive})
	if err != nil || len(p.Items) < 3 {
		t.Fatalf("search by name: %d %v", len(p.Items), err)
	}
	ev := s.audit.of(string(authz.IdentityUserSearch))
	if len(ev) != 2 {
		t.Fatalf("%d search audits, want 2", len(ev))
	}
	if ev[0].ActorUserID == nil || *ev[0].ActorUserID != sup.UserID || ev[0].Metadata["query_kind"] != "email" ||
		!slices.Equal(ev[0].Metadata["result_user_ids"].([]string), []string{s.target.ID.String()}) {
		t.Fatalf("audit %+v", ev[0])
	}
	if ev[1].Metadata["query_kind"] != "name" || ev[1].Metadata["status"] != "active" {
		t.Fatalf("audit %+v", ev[1].Metadata)
	}
	// The query itself (an address) never lands in the audit log.
	b, _ := json.Marshal(ev)
	if strings.Contains(strings.ToLower(string(b)), "target@example.com") {
		t.Fatal("the searched address is in the audit log")
	}
	// Paging: one per page.
	first, _ := s.svc.SearchUsers(ctx, sup, identity.AdminUserQuery{Limit: 1})
	if len(first.Items) != 1 || !first.More {
		t.Fatalf("page %+v", first)
	}
	// Bad queries: 422; a non-staff caller: 403 before anything is read or audited.
	_, err = s.svc.SearchUsers(ctx, sup, identity.AdminUserQuery{Q: "x"})
	wantStatus(t, err, http.StatusUnprocessableEntity)
	n := len(s.audit.of(string(authz.IdentityUserSearch)))
	_, a := s.login(t, "target@example.com", identity.ClientMobile)
	_, err = s.svc.SearchUsers(ctx, a, identity.AdminUserQuery{Q: "admin"})
	problemtest.WantErr(t, err, authz.ErrForbidden)
	noMFA := sup
	noMFA.MFAVerified = false
	_, err = s.svc.SearchUsers(ctx, noMFA, identity.AdminUserQuery{Q: "admin"})
	problemtest.WantErr(t, err, authz.ErrMFARequired)
	if len(s.audit.of(string(authz.IdentityUserSearch))) != n {
		t.Fatal("a refused search was audited as a search")
	}
}

func TestAdminGetUser_NoClubMembership(t *testing.T) {
	t.Parallel()
	s := newStaffEnv(t)
	ctx := t.Context()
	company := authz.Membership{OrgID: uuid.NewV7(), OrgKind: authz.OrgCompany, Role: authz.MemberRecruiter}
	s.members[s.target.ID] = []authz.Membership{
		{OrgID: uuid.NewV7(), OrgKind: authz.OrgClub, Role: authz.MemberMember}, company,
	}
	v, err := s.svc.AdminUser(ctx, s.a, s.target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(v.Memberships, []authz.Membership{company}) || v.ShowBirthDate {
		t.Fatalf("admin view memberships %v (birth date shown: %v)", v.Memberships, v.ShowBirthDate)
	}
	dpo := s.admin(t, "dpo@koro.test", authz.PlatformDPO)
	v, err = s.svc.AdminUser(ctx, dpo, s.target.ID)
	if err != nil || !v.ShowBirthDate {
		t.Fatalf("dpo view: %v %v", v.ShowBirthDate, err)
	}
	_, err = s.svc.AdminUser(ctx, s.a, uuid.NewV7())
	problemtest.WantErr(t, err, identity.ErrNotFound)
	fin := s.admin(t, "fin@koro.test", authz.PlatformFinanceAdmin)
	_, err = s.svc.AdminUser(ctx, fin, uuid.NewV7())
	problemtest.WantErr(t, err, authz.ErrForbidden) // decided before the read: no ID oracle
}

func TestAdminResetUserMfa_NotifiesAndClosesSessions(t *testing.T) {
	t.Parallel()
	s := newStaffEnv(t)
	ctx := t.Context()
	u, a, _, _ := s.mfaUser(t, "lost-phone@example.com")
	s.q.Reset()

	_, err := s.svc.ResetUserMFA(ctx, s.a, u.ID, "lost phone, id checked", identity.ReauthProof{})
	problemtest.WantErr(t, err, identity.ErrReauthRequired)
	if p, err := s.svc.ResetUserMFA(ctx, s.a, u.ID, "lost phone, id checked", proof); err != nil || p != nil {
		t.Fatal(err, p)
	}
	cur, _ := s.store.User(ctx, u.ID)
	_, found, _ := s.store.TOTP(ctx, u.ID)
	sess, _ := s.store.Session(ctx, a.SessionID)
	if cur.MFAEnabled || found || sess.RevokeReason != identity.RevokeMFAReset {
		t.Fatalf("after reset: mfa %v, totp %v, session %q", cur.MFAEnabled, found, sess.RevokeReason)
	}
	s.runNotices(t)
	if !slices.ContainsFunc(s.mail.sent(), func(m notify.Message) bool {
		return m.ToEmail == "lost-phone@example.com" && m.Data["kind"] == identity.NoticeMFAReset
	}) {
		t.Fatal("no mfa_reset notice to the user")
	}
	// Own MFA: 403 (k2, Y6).
	_, err = s.svc.ResetUserMFA(ctx, s.a, s.a.UserID, "my own phone", proof)
	problemtest.WantErr(t, err, authz.ErrForbidden)
}

func TestAdminRevokeUserSessions(t *testing.T) {
	t.Parallel()
	s := newStaffEnv(t)
	sup := s.admin(t, "sup@koro.test", authz.PlatformSupport)
	if p, err := s.svc.RevokeUserSessions(t.Context(), sup, s.target.ID, "account takeover suspected"); err != nil || p != nil {
		t.Fatal(err, p)
	}
	s.targetSessionClosed(t, identity.RevokeAdmin)
	dpo := s.admin(t, "dpo@koro.test", authz.PlatformDPO)
	_, err := s.svc.RevokeUserSessions(t.Context(), dpo, s.target.ID, "dpo cannot do this")
	problemtest.WantErr(t, err, authz.ErrForbidden)
}
