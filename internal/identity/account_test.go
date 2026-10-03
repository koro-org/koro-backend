package identity_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/identity"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/problem"
	"koro/internal/platform/ratelimit"
	"koro/internal/platform/token"
)

func TestConfirmEmail_TokenSingleUse(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	if err := e.svc.Register(ctx, authz.Anonymous(), e.registration("once@example.com")); err != nil {
		t.Fatal(err)
	}
	tok := e.lastToken(t, "identity.verify_email")
	// Pasted with spaces and in lower case it still works (once).
	if err := e.svc.ConfirmEmail(ctx, authz.Anonymous(), " "+strings.ToLower(tok[:10])+" "+tok[10:]); err != nil {
		t.Fatal(err)
	}
	id, _, _ := e.svc.FindByEmail(ctx, "once@example.com")
	u, _ := e.store.User(ctx, id)
	if u.Status != identity.StatusActive || u.EmailVerifiedAt == nil {
		t.Fatalf("after confirmation: %s", u.Status)
	}
	err := e.svc.ConfirmEmail(ctx, authz.Anonymous(), tok)
	wantErr(t, err, identity.ErrInvalidToken)
	wantStatus(t, err, http.StatusUnprocessableEntity)
	// Garbage and a token of another purpose are the same answer.
	wantErr(t, e.svc.ConfirmEmail(ctx, authz.Anonymous(), "nope"), identity.ErrInvalidToken)
	if err := e.svc.RequestPasswordReset(ctx, authz.Anonymous(), "once@example.com"); err != nil {
		t.Fatal(err)
	}
	wantErr(t, e.svc.ConfirmEmail(ctx, authz.Anonymous(), e.lastToken(t, "identity.password_reset")), identity.ErrInvalidToken)
}

func TestConfirmEmail_ExpiredToken(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	if err := e.svc.Register(ctx, authz.Anonymous(), e.registration("late@example.com")); err != nil {
		t.Fatal(err)
	}
	tok := e.lastToken(t, "identity.verify_email")
	e.clk.Advance(24*time.Hour + time.Second)
	wantErr(t, e.svc.ConfirmEmail(ctx, authz.Anonymous(), tok), identity.ErrInvalidToken)
	id, _, _ := e.svc.FindByEmail(ctx, "late@example.com")
	if u, _ := e.store.User(ctx, id); u.Status != identity.StatusPendingEmail {
		t.Fatalf("expired token activated the account: %s", u.Status)
	}
	// The worker does not send an expired token either.
	before := len(e.mail.sent())
	if err := e.svc.ResendVerification(ctx, authz.Anonymous(), "late@example.com"); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(25 * time.Hour)
	e.deliver(t)
	if len(e.mail.sent()) != before {
		t.Fatal("an expired token was e-mailed")
	}
}

func TestPasswordReset_ClosesAllSessions(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	u := e.activeUser(t, "reset@example.com")
	web, _ := e.login(t, "reset@example.com", identity.ClientWeb)
	mob, _ := e.login(t, "reset@example.com", identity.ClientMobile)
	if err := e.svc.RequestPasswordReset(ctx, authz.Anonymous(), "reset@example.com"); err != nil {
		t.Fatal(err)
	}
	// Unknown addresses get the same answer.
	if err := e.svc.RequestPasswordReset(ctx, authz.Anonymous(), "ghost@example.com"); err != nil {
		t.Fatal(err)
	}
	tok := e.lastToken(t, "identity.password_reset")
	wantErr(t, e.svc.ConfirmPasswordReset(ctx, authz.Anonymous(), tok, "short"), identity.ErrWeakPassword)
	newPW := "Yeni-Ve-Uzun-Parola-4821"
	if err := e.svc.ConfirmPasswordReset(ctx, authz.Anonymous(), tok, newPW); err != nil {
		t.Fatal(err)
	}
	for _, r := range []identity.LoginResult{web, mob} {
		s, _ := e.store.Session(ctx, r.Session.ID)
		if s.RevokedAt == nil || s.RevokeReason != identity.RevokePasswordChange {
			t.Fatalf("session %s after reset: %+v", r.Session.ClientKind, s.RevokeReason)
		}
	}
	wantErr(t, e.svc.ConfirmPasswordReset(ctx, authz.Anonymous(), tok, newPW), identity.ErrInvalidToken)
	if _, err := e.svc.CreateSession(ctx, authz.Anonymous(), identity.Login{Email: "reset@example.com", Password: newPW}); err != nil {
		t.Fatalf("login with the new password: %v", err)
	}
	e.runNotices(t)
	if last := e.mail.sent()[len(e.mail.sent())-1]; last.Data["kind"] != identity.NoticePasswordChanged || *last.UserID != u.ID {
		t.Fatalf("no password_changed notice: %+v", last)
	}
	// The reset token expires after 30 minutes (02 §5.3).
	if err := e.svc.RequestPasswordReset(ctx, authz.Anonymous(), "reset@example.com"); err != nil {
		t.Fatal(err)
	}
	tok = e.lastToken(t, "identity.password_reset")
	e.clk.Advance(31 * time.Minute)
	wantErr(t, e.svc.ConfirmPasswordReset(ctx, authz.Anonymous(), tok, "Baska-Uzun-Parola-9090"), identity.ErrInvalidToken)
}

func TestChangePassword_ReauthMissing_403(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.activeUser(t, "c@example.com")
	_, a := e.login(t, "c@example.com", identity.ClientWeb)
	err := e.svc.ChangePassword(t.Context(), a, identity.PasswordChange{NewPassword: "Yeni-Ve-Uzun-Parola-4821"})
	wantErr(t, err, identity.ErrReauthRequired)
	wantStatus(t, err, http.StatusForbidden)
	// The same proof is required by e-mail change.
	wantErr(t, e.svc.RequestEmailChange(t.Context(), a, "new@example.com", identity.ReauthProof{}), identity.ErrReauthRequired)
}

func TestChangePassword_ReauthWrong_403AndRateLimited(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	e.activeUser(t, "w@example.com")
	other, a := e.login(t, "w@example.com", identity.ClientMobile)
	keep, keepActor := e.login(t, "w@example.com", identity.ClientWeb)
	_ = keepActor
	wrong := identity.PasswordChange{CurrentPassword: "not-the-password-1", NewPassword: "Yeni-Ve-Uzun-Parola-4821"}
	for i := range 5 {
		err := e.svc.ChangePassword(ctx, a, wrong)
		wantErr(t, err, identity.ErrReauthFailed)
		wantStatus(t, err, http.StatusForbidden) // 403, not 401: the client keeps its session
		if i == 0 {
			denied := e.audit.of(string(authz.IdentityPasswordChange))
			if len(denied) != 1 || denied[0].Outcome != audit.Denied {
				t.Fatalf("denied audit = %+v", denied)
			}
		}
	}
	// Locked: even the right password is refused until the lock ends.
	err := e.svc.ChangePassword(ctx, a, identity.PasswordChange{CurrentPassword: goodPassword, NewPassword: "Yeni-Ve-Uzun-Parola-4821"})
	wantErr(t, err, problem.ErrRateLimited)
	wantStatus(t, err, http.StatusTooManyRequests)
	// The reauth class counts failures per user in the service (06 H-6).
	if !slices.ContainsFunc(e.lim.Calls(), func(c string) bool { return strings.HasPrefix(c, "reauth|user:") }) {
		t.Fatalf("reauth class not applied: %v", e.lim.Calls())
	}
	e.clk.Advance(16 * time.Minute)
	if err := e.svc.ChangePassword(ctx, a, identity.PasswordChange{CurrentPassword: goodPassword, NewPassword: "Yeni-Ve-Uzun-Parola-4821"}); err != nil {
		t.Fatalf("after the lock: %v", err)
	}
	// The current session stays, the others close.
	if s, _ := e.store.Session(ctx, other.Session.ID); s.RevokedAt != nil {
		t.Fatal("the current session was closed")
	}
	if s, _ := e.store.Session(ctx, keep.Session.ID); s.RevokeReason != identity.RevokePasswordChange {
		t.Fatalf("other session: %+v", s.RevokeReason)
	}
	if ok := e.audit.of(string(authz.IdentityPasswordChange)); ok[len(ok)-1].Metadata[audit.MetaReauth] != "password" {
		t.Fatalf("audit reauth kind = %v", ok[len(ok)-1].Metadata)
	}
	// The consent hook (Verify) counts towards the same lockout.
	for range 5 {
		_, _ = e.svc.Verify(ctx, a.UserID, identity.ReauthProof{Password: "still-wrong-123"})
	}
	_, err = e.svc.Verify(ctx, a.UserID, identity.ReauthProof{Password: "Yeni-Ve-Uzun-Parola-4821"})
	wantErr(t, err, problem.ErrRateLimited)
}

func TestEmailChange_NotifiesOldAddress(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	u := e.activeUser(t, "old@example.com")
	_, a := e.login(t, "old@example.com", identity.ClientWeb)
	if err := e.svc.RequestEmailChange(ctx, a, "New@Example.com", identity.ReauthProof{Password: goodPassword}); err != nil {
		t.Fatal(err)
	}
	e.runNotices(t)
	notice := e.mail.sent()[len(e.mail.sent())-1]
	if notice.ToEmail != "old@example.com" || notice.Data["kind"] != identity.NoticeEmailChangeRequested {
		t.Fatalf("old address notice = to %q kind %v", notice.ToEmail, notice.Data["kind"])
	}
	tok := e.lastToken(t, "identity.email_change")
	if last := e.mail.sent()[len(e.mail.sent())-1]; last.ToEmail != "New@Example.com" {
		t.Fatalf("confirmation went to %q, want the new address", last.ToEmail)
	}
	if cur, _ := e.store.User(ctx, u.ID); cur.Email != "old@example.com" {
		t.Fatal("the address changed before confirmation")
	}
	if err := e.svc.ConfirmEmailChange(ctx, authz.Anonymous(), tok); err != nil {
		t.Fatal(err)
	}
	cur, _ := e.store.User(ctx, u.ID)
	if cur.Email != "New@Example.com" || cur.EmailNormalized != "new@example.com" {
		t.Fatalf("address after confirmation: %q", cur.Email)
	}
	wantErr(t, e.svc.ConfirmEmailChange(ctx, authz.Anonymous(), tok), identity.ErrInvalidToken)

	// An address of another account is answered alike (202) and gets nothing.
	e.activeUser(t, "taken@example.com")
	before := len(e.out.Events())
	if err := e.svc.RequestEmailChange(ctx, a, "taken@example.com", identity.ReauthProof{Password: goodPassword}); err != nil {
		t.Fatalf("taken address = %v, want nil (202)", err)
	}
	if len(e.out.Events()) != before {
		t.Fatal("a token was issued for a taken address")
	}
}

func TestRevokeMySession_OtherUsersSession_404(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	e.activeUser(t, "a@example.com")
	e.activeUser(t, "b@example.com")
	sa, a := e.login(t, "a@example.com", identity.ClientWeb)
	_, b := e.login(t, "b@example.com", identity.ClientWeb)
	err := e.svc.RevokeMySession(ctx, b, sa.Session.ID)
	wantErr(t, err, identity.ErrNotFound)
	wantStatus(t, err, http.StatusNotFound)
	if s, _ := e.store.Session(ctx, sa.Session.ID); s.RevokedAt != nil {
		t.Fatal("another user's session was revoked")
	}
	wantErr(t, e.svc.RevokeMySession(ctx, b, uuid.NewV7()), identity.ErrNotFound)
	// Its owner may, and sees only live sessions afterwards.
	second, _ := e.login(t, "a@example.com", identity.ClientMobile)
	p, err := e.svc.ListMySessions(ctx, a, identity.SessionQuery{})
	if err != nil || len(p.Items) != 2 {
		t.Fatalf("ListMySessions = %d, %v", len(p.Items), err)
	}
	if err := e.svc.RevokeMySession(ctx, a, second.Session.ID); err != nil {
		t.Fatal(err)
	}
	if p, _ := e.svc.ListMySessions(ctx, a, identity.SessionQuery{}); len(p.Items) != 1 || p.Items[0].ID != sa.Session.ID {
		t.Fatalf("after revoke: %+v", p.Items)
	}
	// Logout closes the current session.
	if err := e.svc.DeleteCurrentSession(ctx, a); err != nil {
		t.Fatal(err)
	}
	if s, _ := e.store.Session(ctx, sa.Session.ID); s.RevokeReason != identity.RevokeLogout {
		t.Fatalf("logout: %+v", s.RevokeReason)
	}
}

func TestGetMe_CapabilitiesForRoles(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	u := e.activeUser(t, "me@example.com")
	base := authz.Actor{Kind: authz.KindUser, UserID: u.ID, StudentStatus: authz.StudentNone}
	me, err := e.svc.Me(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	if me.Email != "me@example.com" || me.IsMinor || me.BirthDate == nil || len(me.PlatformRoles) != 0 {
		t.Fatalf("me = %+v", me)
	}
	caps := func(a authz.Actor) []authz.Action { return identity.Capabilities(a) }
	user := caps(base)
	if !slices.Contains(user, authz.IdentityPasswordChange) || slices.Contains(user, authz.IdentityUserSearch) ||
		!slices.Contains(user, authz.IdentityEduVerificationStart) {
		t.Fatalf("plain user capabilities %v", user)
	}
	pad, _ := authztest.Get("platform_admin")
	admin := caps(pad.Actor)
	if !slices.Contains(admin, authz.IdentityUserSearch) || !slices.Contains(admin, authz.IdentityPlatformRoleList) {
		t.Fatalf("platform_admin capabilities %v", admin)
	}
	cma, _ := authztest.Get("company_admin")
	if c := caps(cma.Actor); !slices.Contains(c, authz.OrdersOrderCreate) || slices.Contains(c, authz.IdentityUserSearch) {
		t.Fatalf("company_admin capabilities %v", c)
	}
	minor, _ := authztest.Get("student_minor")
	if c := caps(minor.Actor); slices.Contains(c, authz.DirectoryClubClaimCreate) || !slices.Contains(c, authz.EventsRegistrationListOwn) {
		t.Fatalf("student_minor capabilities %v", c)
	}
	tal, _ := authztest.Get("talent_auditor")
	for _, c := range caps(tal.Actor) {
		if strings.HasPrefix(string(c), "talent.") {
			t.Fatalf("a talent action is a menu hint: %s", c)
		}
	}
	// getMe returns the actor's roles and memberships with the hints.
	a := base
	a.PlatformRoles = []authz.PlatformRole{authz.PlatformSupport}
	me, _ = e.svc.Me(t.Context(), a)
	if !slices.Equal(me.PlatformRoles, a.PlatformRoles) || !slices.Contains(me.Capabilities, authz.IdentityEduVerificationList) {
		t.Fatalf("me roles %v caps %v", me.PlatformRoles, me.Capabilities)
	}
	// Another user's account is not reachable through getMe.
	_, err = e.svc.Me(t.Context(), authz.Actor{Kind: authz.KindUser, UserID: uuid.NewV7()})
	wantErr(t, err, problem.ErrNotFound)
	// updateMe changes names and locale.
	en := identity.LocaleEN
	me, err = e.svc.UpdateMe(t.Context(), base, identity.MePatch{Locale: &en})
	if err != nil || me.Locale != identity.LocaleEN {
		t.Fatalf("UpdateMe = %+v, %v", me.Locale, err)
	}
	_, err = e.svc.UpdateMe(t.Context(), base, identity.MePatch{})
	wantErr(t, err, identity.ErrEmptyPatch)
}

func TestEmailTokenEvent_PayloadHasNoToken(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if err := e.svc.Register(t.Context(), authz.Anonymous(), e.registration("evt@example.com")); err != nil {
		t.Fatal(err)
	}
	evs := e.out.Events()
	if len(evs) != 1 || evs[0].Topic != domainevent.TopicIdentityEmailTokenIssued {
		t.Fatalf("events %+v", e.out.Topics())
	}
	p, err := domainevent.Decode[domainevent.IdentityEmailTokenIssued](evs[0])
	if err != nil || p.Purpose != string(identity.PurposeAccountEmail) {
		t.Fatalf("payload %+v, %v", p, err)
	}
	tok := e.lastToken(t, "identity.verify_email")
	stored, err := e.store.EmailToken(t.Context(), p.TokenID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(evs[0])
	hasNoSecret(t, "outbox envelope", string(raw), tok, string(stored.TokenHash))
	if strings.Contains(strings.ToUpper(string(raw)), tok[:20]) {
		t.Fatal("the envelope carries the token")
	}
	// The worker re-derived exactly the stored token (only its hash is kept).
	if !token.Verify(tok, stored.TokenHash) {
		t.Fatal("the e-mailed token does not match the stored hash")
	}
	last := e.mail.sent()[len(e.mail.sent())-1]
	if last.DedupeKey != "identity.email_token:"+p.TokenID.String() || !strings.HasPrefix(last.Data["link"].(string), "https://app.koro.test/auth/verify-email#token=") {
		t.Fatalf("mail = %+v", last.DedupeKey)
	}
	// Delivering the same event twice sends the same deduplicated message.
	e.delivered = 0
	e.deliver(t)
	if again := e.mail.sent()[len(e.mail.sent())-1]; again.DedupeKey != last.DedupeKey {
		t.Fatal("redelivery changed the dedupe key")
	}
}

func TestAuthRateLimits_StrictClasses(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := httpx.WithClientIP(t.Context(), "203.0.113.7")
	if err := e.svc.Register(ctx, authz.Anonymous(), e.registration("rl@example.com")); err != nil {
		t.Fatal(err)
	}
	// auth_email: 3 per hour per address (and per IP + address), in the service.
	for i := range 3 {
		if err := e.svc.ResendVerification(ctx, authz.Anonymous(), "rl@example.com"); err != nil {
			t.Fatalf("resend %d: %v", i+1, err)
		}
	}
	err := e.svc.ResendVerification(ctx, authz.Anonymous(), "RL@example.com")
	wantErr(t, err, problem.ErrRateLimited)
	// Another IP does not reset the per-address count.
	err = e.svc.ResendVerification(httpx.WithClientIP(t.Context(), "198.51.100.1"), authz.Anonymous(), "rl@example.com")
	wantErr(t, err, problem.ErrRateLimited)
	// The keys never contain the address.
	for _, c := range e.lim.Calls() {
		hasNoSecret(t, "rate limit key", strings.ToLower(c), "rl@example.com")
	}
	// An unknown address counts too (no enumeration through 429).
	for range 3 {
		_ = e.svc.ResendVerification(ctx, authz.Anonymous(), "ghost@example.com")
	}
	wantErr(t, e.svc.ResendVerification(ctx, authz.Anonymous(), "ghost@example.com"), problem.ErrRateLimited)
	// Password reset: 5 per hour per address.
	for range 5 {
		if err := e.svc.RequestPasswordReset(ctx, authz.Anonymous(), "rl@example.com"); err != nil {
			t.Fatal(err)
		}
	}
	wantErr(t, e.svc.RequestPasswordReset(ctx, authz.Anonymous(), "rl@example.com"), problem.ErrRateLimited)

	// Strict: an unavailable limiter fails closed (503), never open.
	e.lim.Fail(ratelimit.ErrUnavailable)
	err = e.svc.RequestPasswordReset(ctx, authz.Anonymous(), "other@example.com")
	wantErr(t, err, problem.ErrUnavailable)
	wantStatus(t, err, http.StatusServiceUnavailable)
	e.lim.Fail(nil)

	// The classes of the operations are the strict ones of 04 §1.9.
	rules := ratelimit.Rules()
	for _, c := range []string{"auth_login", "auth_register", "auth_password_reset", "auth_email", "token_confirm", "reauth"} {
		if r := rules[op.RateLimit(c)]; !r.Strict {
			t.Errorf("rate limit class %s is not strict", c)
		}
	}
}

func TestPlatformRoles_PendingRequestGrantsNothing(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	u := e.activeUser(t, "role@example.com")
	approver := e.activeUser(t, "approver@example.com")
	second := e.activeUser(t, "second@example.com")
	now := e.clk.Now()
	// A pending platform_admin request (06 H-33): grants nothing.
	pending := identity.RoleAssignment{ID: uuid.NewV7(), UserID: u.ID, Role: authz.PlatformAdmin, Reason: "needs admin access",
		Status: identity.RolePending, RequestedBy: &approver.ID, RequestedAt: now}
	if err := e.store.InsertRoleAssignment(ctx, pending); err != nil {
		t.Fatal(err)
	}
	res, _ := e.login(t, "role@example.com", identity.ClientMobile)
	if res.Session.AbsoluteExpiresAt.Sub(res.Session.CreatedAt) != 90*24*time.Hour {
		t.Fatal("a pending request gave the privileged session policy")
	}
	p, err := e.svc.Authenticate(ctx, httpx.Credential{Transport: httpx.TransportBearer, TokenHash: [32]byte(token.Hash(res.Token))})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.PlatformRoles) != 0 {
		t.Fatalf("pending request in effect: %v", p.PlatformRoles)
	}
	// moderator active, finance_admin revoke_pending: both in effect.
	mod := identity.RoleAssignment{ID: uuid.NewV7(), UserID: u.ID, Role: authz.PlatformModerator, Reason: "moderation team",
		Status: identity.RoleActive, RequestedBy: &approver.ID, RequestedAt: now, GrantedAt: &now}
	fin := identity.RoleAssignment{ID: uuid.NewV7(), UserID: u.ID, Role: authz.PlatformFinanceAdmin, Reason: "finance team",
		Status: identity.RoleRevokePending, RequestedBy: &approver.ID, RequestedAt: now, ApprovedBy: &second.ID, ApprovedAt: &now,
		GrantedAt: &now, RevokeReason: "left the team", RevokeRequestedBy: &approver.ID, RevokeRequestedAt: &now}
	rejected := identity.RoleAssignment{ID: uuid.NewV7(), UserID: u.ID, Role: authz.PlatformDPO, Reason: "dpo request",
		Status: identity.RoleRejected, RequestedBy: &approver.ID, RequestedAt: now, RejectedBy: &second.ID, RejectedAt: &now}
	for _, a := range []identity.RoleAssignment{mod, fin, rejected} {
		if err := e.store.InsertRoleAssignment(ctx, a); err != nil {
			t.Fatalf("%s: %v", a.Role, err)
		}
	}
	got, err := e.store.PlatformRoles(ctx, u.ID)
	want := []authz.PlatformRole{authz.PlatformFinanceAdmin, authz.PlatformModerator}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("PlatformRoles = %v, %v; want %v (pending and rejected excluded)", got, err, want)
	}
	if holders, _ := e.store.UsersWithRole(ctx, authz.PlatformAdmin); len(holders) != 0 {
		t.Fatalf("a pending request counted as platform_admin: %v", holders)
	}
	if !identity.RoleRevokePending.InEffect() || identity.RolePending.InEffect() || identity.RoleRevoked.InEffect() {
		t.Fatal("RoleStatus.InEffect does not match 06 H-33")
	}
}
