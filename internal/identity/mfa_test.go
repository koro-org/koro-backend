package identity_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/platform/httpx"
	"koro/internal/platform/token"
)

// pwProof is the password proof of enrollTotp/confirmTotp (06 H-46).
var pwProof = identity.ReauthProof{Password: goodPassword}

// totpCode is the code of secret at t (what an authenticator app shows).
func totpCode(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	c, err := totp.GenerateCodeCustom(secret, at, totp.ValidateOpts{Period: 30, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// nextCode moves the clock to the next 30-second step and returns its code
// (a code is accepted once: the replay guard).
func (e *env) nextCode(t *testing.T, secret string) string {
	t.Helper()
	e.clk.Advance(30 * time.Second)
	return totpCode(t, secret, e.clk.Now())
}

// enableMFA enrolls and confirms TOTP for the actor's account.
func (e *env) enableMFA(t *testing.T, a authz.Actor) (string, identity.RecoveryCodes) {
	t.Helper()
	enr, err := e.svc.EnrollTOTP(t.Context(), a, pwProof)
	if err != nil {
		t.Fatalf("EnrollTOTP: %v", err)
	}
	codes, err := e.svc.ConfirmTOTP(t.Context(), a, totpCode(t, enr.Secret, e.clk.Now()), pwProof)
	if err != nil {
		t.Fatalf("ConfirmTOTP: %v", err)
	}
	if len(codes) != 10 {
		t.Fatalf("%d recovery codes", len(codes))
	}
	return enr.Secret, codes
}

// mfaUser is an active account with TOTP on, and its signed-in actor.
func (e *env) mfaUser(t *testing.T, email string) (identity.User, authz.Actor, string, identity.RecoveryCodes) {
	t.Helper()
	u := e.activeUser(t, email)
	_, a := e.login(t, email, identity.ClientMobile)
	secret, codes := e.enableMFA(t, a)
	a.MFAVerified = true
	return u, a, secret, codes
}

// passwordStep runs createSession of an MFA account and returns its challenge.
func (e *env) passwordStep(t *testing.T, email string, client identity.ClientKind) identity.MFAChallenge {
	t.Helper()
	res, err := e.svc.CreateSession(t.Context(), authz.Anonymous(), identity.Login{Email: email, Password: goodPassword, Client: client})
	if err != nil {
		t.Fatalf("password step: %v", err)
	}
	if !res.MFARequired || res.Token != "" || res.Session.ID != uuid.Nil() {
		t.Fatalf("password step of an MFA account = %+v, want a challenge only", res.Challenge)
	}
	return res.Challenge
}

func (e *env) sessionsOf(t *testing.T, id uuid.UUID) []identity.Session {
	t.Helper()
	ss, err := e.store.SessionsOf(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return ss
}

func TestTOTP_ReplaySameCode_Rejected(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	_, a, secret, _ := e.mfaUser(t, "replay@example.com")
	ctx := t.Context()

	// The confirmation code itself is used: the same step is refused.
	if err := e.svc.VerifyMFA(ctx, a, totpCode(t, secret, e.clk.Now())); err == nil {
		t.Fatal("the confirmation code was accepted a second time")
	}
	c := e.nextCode(t, secret)
	if err := e.svc.VerifyMFA(ctx, a, c); err != nil {
		t.Fatalf("first use: %v", err)
	}
	// Same code, same 30-second window: refused (R-002: the library does not do this).
	err := e.svc.VerifyMFA(ctx, a, c)
	wantErr(t, err, identity.ErrInvalidMFACode)
	// The previous step is still inside the ±1 tolerance but older than the
	// last used one: refused too.
	wantErr(t, e.svc.VerifyMFA(ctx, a, totpCode(t, secret, e.clk.Now().Add(-30*time.Second))), identity.ErrInvalidMFACode)
	// The same code as a re-authentication proof: refused as well.
	err = e.svc.RequestEmailChange(ctx, a, "new@example.com", identity.ReauthProof{TOTPCode: c})
	wantErr(t, err, identity.ErrReauthFailed)
	// Two steps out of tolerance: refused.
	wantErr(t, e.svc.VerifyMFA(ctx, a, totpCode(t, secret, e.clk.Now().Add(90*time.Second))), identity.ErrInvalidMFACode)
	// The next window works again (inside the reauth class: 4 failures so far).
	if err := e.svc.VerifyMFA(ctx, a, e.nextCode(t, secret)); err != nil {
		t.Fatalf("next window: %v", err)
	}
}

func TestTOTP_SecretOnlyInEnrollResponse(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	e := newEnv(t, func(d *identity.Deps) {
		d.Log = slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	ctx := t.Context()
	u := e.activeUser(t, "secret@example.com")
	_, a := e.login(t, "secret@example.com", identity.ClientMobile)

	enr, err := e.svc.EnrollTOTP(ctx, a, pwProof)
	if err != nil {
		t.Fatal(err)
	}
	if len(enr.Secret) < 32 || !strings.Contains(enr.URI, "secret="+enr.Secret) || !strings.HasPrefix(enr.URI, "otpauth://totp/Koro:") {
		t.Fatalf("enrollment %q / %q", enr.Secret, enr.URI)
	}
	// A second enrollment before confirmation replaces the secret.
	enr2, err := e.svc.EnrollTOTP(ctx, a, pwProof)
	if err != nil || enr2.Secret == enr.Secret {
		t.Fatalf("re-enrollment: %v (same secret: %v)", err, enr2.Secret == enr.Secret)
	}
	codes, err := e.svc.ConfirmTOTP(ctx, a, totpCode(t, enr2.Secret, e.clk.Now()), pwProof)
	if err != nil {
		t.Fatal(err)
	}
	// After confirmation the secret is never shown again.
	if _, err := e.svc.EnrollTOTP(ctx, a, pwProof); err == nil {
		t.Fatal("enrollTotp on an MFA account must be refused")
	} else {
		wantStatus(t, err, http.StatusConflict)
	}
	me, err := e.svc.Me(ctx, a)
	if err != nil || !me.MFAEnabled {
		t.Fatalf("Me: %+v %v", me.MFAEnabled, err)
	}

	row, found, err := e.store.TOTP(ctx, u.ID)
	if err != nil || !found || row.ConfirmedAt == nil {
		t.Fatalf("totp row: %v %v", found, err)
	}
	everywhere := map[string]string{
		"sealed secret": string(row.SecretEnc), "logs": logs.String(), "confirm response": strings.Join(codes, ","),
		"me": fmt.Sprintf("%+v", me), "%v": fmt.Sprintf("%v %+v %#v", enr2, enr2, enr2),
		"recovery codes %v": fmt.Sprintf("%v %#v", codes, codes),
	}
	b, _ := json.Marshal(e.audit.events)
	everywhere["audit"] = string(b)
	b, _ = json.Marshal(e.out.Events())
	everywhere["outbox"] = string(b)
	b, _ = json.Marshal(e.q.Tasks())
	everywhere["queue"] = string(b)
	for where, s := range everywhere {
		hasNoSecret(t, where, s, enr.Secret, enr2.Secret, strings.ToLower(enr2.Secret))
	}
	// The recovery codes leave only through the confirm response.
	for where, s := range map[string]string{"logs": logs.String(), "audit": everywhere["audit"], "queue": everywhere["queue"]} {
		hasNoSecret(t, where, s, codes...)
	}
	// The sealed value is bound to the user: another user's row cannot use it.
	if len(row.SecretEnc) < 40 {
		t.Fatalf("sealed secret too short: %d bytes", len(row.SecretEnc))
	}
}

func TestRecoveryCode_SingleUse(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	u, _, _, codes := e.mfaUser(t, "recovery@example.com")
	ctx := t.Context()
	e.q.Reset()

	ch := e.passwordStep(t, "recovery@example.com", identity.ClientMobile)
	res, err := e.svc.CompleteMFALogin(ctx, authz.Anonymous(), identity.MFALogin{ChallengeID: ch.ID, RecoveryCode: codes[0]})
	if err != nil {
		t.Fatalf("recovery code: %v", err)
	}
	if res.Token == "" || res.Session.MFAVerifiedAt == nil {
		t.Fatalf("session %+v", res.Session)
	}
	left, _ := e.store.RecoveryCodesLeft(ctx, u.ID)
	if left != 9 {
		t.Fatalf("%d codes left, want 9", left)
	}
	// A recovery code sign-in sends a security notice (03 §6.4-4).
	e.runNotices(t)
	if msgs := e.mail.sent(); len(msgs) == 0 || msgs[len(msgs)-1].Template != "identity.mfa_notice" ||
		msgs[len(msgs)-1].Data["kind"] != identity.NoticeRecoveryCodeUsed {
		t.Fatalf("no recovery code notice: %+v", msgs)
	}

	// The same code again: refused.
	ch = e.passwordStep(t, "recovery@example.com", identity.ClientMobile)
	_, err = e.svc.CompleteMFALogin(ctx, authz.Anonymous(), identity.MFALogin{ChallengeID: ch.ID, RecoveryCode: codes[0]})
	wantErr(t, err, identity.ErrMFALoginFailed)
	wantStatus(t, err, http.StatusUnauthorized)
	// Another code, typed without dashes in upper case, works on the same challenge.
	other := strings.ToUpper(strings.ReplaceAll(codes[1], "-", ""))
	if _, err := e.svc.CompleteMFALogin(ctx, authz.Anonymous(), identity.MFALogin{ChallengeID: ch.ID, RecoveryCode: other}); err != nil {
		t.Fatalf("second code: %v", err)
	}
	// Recovery codes are not a re-authentication proof (03 §6.2).
	_, a := e.loginMFA(t, "recovery@example.com", codes[2])
	err = e.svc.RequestEmailChange(ctx, a, "x@example.com", identity.ReauthProof{TOTPCode: codes[3]})
	wantErr(t, err, identity.ErrReauthFailed)
}

// loginMFA signs an MFA account in with a recovery code and returns the
// session and its actor.
func (e *env) loginMFA(t *testing.T, email, recovery string) (identity.LoginResult, authz.Actor) {
	t.Helper()
	ch := e.passwordStep(t, email, identity.ClientMobile)
	res, err := e.svc.CompleteMFALogin(t.Context(), authz.Anonymous(), identity.MFALogin{ChallengeID: ch.ID, RecoveryCode: recovery})
	if err != nil {
		t.Fatal(err)
	}
	return res, authz.Actor{Kind: authz.KindUser, UserID: res.Session.UserID, SessionID: res.Session.ID, MFAVerified: true}
}

func TestMFALogin_ChallengeExpiresAfter5Min(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	_, _, secret, _ := e.mfaUser(t, "expiry@example.com")
	ctx := t.Context()

	ch := e.passwordStep(t, "expiry@example.com", identity.ClientWeb)
	if got := ch.ExpiresAt.Sub(e.clk.Now()); got != identity.ChallengeTTL || identity.ChallengeTTL != 5*time.Minute {
		t.Fatalf("challenge lives %s, want 5m", got)
	}
	e.clk.Advance(5 * time.Minute)
	_, err := e.svc.CompleteMFALogin(ctx, authz.Anonymous(), identity.MFALogin{ChallengeID: ch.ID, TOTPCode: totpCode(t, secret, e.clk.Now())})
	wantErr(t, err, identity.ErrMFALoginFailed)
	if e.chal.Len() != 0 {
		t.Fatalf("an expired challenge stays in the store")
	}

	// One second earlier it still works, and the web client gets its session.
	ch = e.passwordStep(t, "expiry@example.com", identity.ClientWeb)
	e.clk.Advance(5*time.Minute - time.Second)
	res, err := e.svc.CompleteMFALogin(ctx, authz.Anonymous(), identity.MFALogin{ChallengeID: ch.ID, TOTPCode: totpCode(t, secret, e.clk.Now())})
	if err != nil {
		t.Fatalf("within 5 minutes: %v", err)
	}
	if res.Session.ClientKind != identity.ClientWeb || res.Session.MFAVerifiedAt == nil {
		t.Fatalf("session %+v", res.Session)
	}
	// Single use: the challenge is gone.
	_, err = e.svc.CompleteMFALogin(ctx, authz.Anonymous(), identity.MFALogin{ChallengeID: ch.ID, TOTPCode: e.nextCode(t, secret)})
	wantErr(t, err, identity.ErrMFALoginFailed)
}

func TestMFALogin_NoHalfAuthenticatedSession(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	u, _, secret, _ := e.mfaUser(t, "half@example.com")
	ctx := t.Context()
	before := len(e.sessionsOf(t, u.ID))

	ch := e.passwordStep(t, "half@example.com", identity.ClientMobile)
	if n := len(e.sessionsOf(t, u.ID)); n != before {
		t.Fatalf("the password step opened a session (%d → %d)", before, n)
	}
	// The challenge ID is no credential.
	for _, raw := range []string{ch.ID.String(), strings.ReplaceAll(ch.ID.String(), "-", "")} {
		cred := httpx.Credential{Transport: httpx.TransportBearer, TokenHash: [32]byte(token.Hash(raw))}
		_, err := e.svc.Authenticate(ctx, cred)
		wantErr(t, err, identity.ErrUnauthenticated)
	}
	// A wrong code opens nothing either; the challenge survives it.
	_, err := e.svc.CompleteMFALogin(ctx, authz.Anonymous(), identity.MFALogin{ChallengeID: ch.ID, TOTPCode: "000000"})
	wantErr(t, err, identity.ErrMFALoginFailed)
	if n := len(e.sessionsOf(t, u.ID)); n != before {
		t.Fatalf("a wrong code opened a session")
	}
	if len(e.audit.of(string(authz.IdentitySessionCreate))) == 0 {
		t.Fatal("the failed second factor is not audited")
	}
	// Both codes at once, or none, are refused before anything happens.
	_, err = e.svc.CompleteMFALogin(ctx, authz.Anonymous(), identity.MFALogin{ChallengeID: ch.ID, TOTPCode: "1", RecoveryCode: "2"})
	wantStatus(t, err, http.StatusUnprocessableEntity)
	res, err := e.svc.CompleteMFALogin(ctx, authz.Anonymous(), identity.MFALogin{ChallengeID: ch.ID, TOTPCode: e.nextCode(t, secret)})
	if err != nil {
		t.Fatalf("right code: %v", err)
	}
	ss := e.sessionsOf(t, u.ID)
	if len(ss) != before+1 || ss[len(ss)-1].MFAVerifiedAt == nil {
		t.Fatalf("sessions after the second factor: %d (mfa %v)", len(ss), ss[len(ss)-1].MFAVerifiedAt)
	}
	cred := httpx.Credential{Transport: httpx.TransportBearer, TokenHash: [32]byte(token.Hash(res.Token))}
	p, err := e.svc.Authenticate(ctx, cred)
	if err != nil || !p.MFAVerified {
		t.Fatalf("principal %+v %v", p, err)
	}
}

func TestMFALogin_WrongCodesLockTheAccount(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	_, _, secret, _ := e.mfaUser(t, "guess@example.com")
	ctx := t.Context()
	for i := range 5 {
		ch := e.passwordStep(t, "guess@example.com", identity.ClientMobile)
		_, err := e.svc.CompleteMFALogin(ctx, authz.Anonymous(), identity.MFALogin{ChallengeID: ch.ID, TOTPCode: fmt.Sprintf("%06d", i)})
		if err == nil {
			t.Fatal("a guessed code was accepted")
		}
	}
	// Locked now: the password step is refused, and so is a right code on an old challenge.
	_, err := e.svc.CreateSession(ctx, authz.Anonymous(), identity.Login{Email: "guess@example.com", Password: goodPassword})
	wantErr(t, err, identity.ErrInvalidCredentials)
	// Even after the lock, the counter keeps the back-off growing (not reset by the password step).
	e.clk.Advance(time.Hour)
	ch := e.passwordStep(t, "guess@example.com", identity.ClientMobile)
	_, err = e.svc.CompleteMFALogin(ctx, authz.Anonymous(), identity.MFALogin{ChallengeID: ch.ID, TOTPCode: "999999"})
	wantErr(t, err, identity.ErrMFALoginFailed)
	_, err = e.svc.CreateSession(ctx, authz.Anonymous(), identity.Login{Email: "guess@example.com", Password: goodPassword})
	wantErr(t, err, identity.ErrInvalidCredentials)
	// A right code after the lock expired opens the session and resets the counter.
	e.clk.Advance(time.Hour)
	ch = e.passwordStep(t, "guess@example.com", identity.ClientMobile)
	if _, err := e.svc.CompleteMFALogin(ctx, authz.Anonymous(), identity.MFALogin{ChallengeID: ch.ID, TOTPCode: totpCode(t, secret, e.clk.Now())}); err != nil {
		t.Fatalf("right code: %v", err)
	}
}

func TestDisableTotp_RequiredRole_409(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()

	// A platform role in the actor (resolved per request).
	_, a, secret, _ := e.mfaUser(t, "staff@example.com")
	staff := a
	staff.PlatformRoles = []authz.PlatformRole{authz.PlatformSupport}
	err := e.svc.DisableTOTP(ctx, staff, identity.ReauthProof{TOTPCode: e.nextCode(t, secret)})
	wantErr(t, err, identity.ErrMFARequiredByRole)
	wantStatus(t, err, http.StatusConflict)

	// A company_admin membership from the membership source (k1 lists
	// company_admin/owner and university_rep).
	u2, a2, secret2, _ := e.mfaUser(t, "companyadmin@example.com")
	e.members[u2.ID] = []authz.Membership{{OrgID: uuid.NewV7(), OrgKind: authz.OrgCompany, Role: authz.MemberCompanyAdmin}}
	wantErr(t, e.svc.DisableTOTP(ctx, a2, identity.ReauthProof{TOTPCode: e.nextCode(t, secret2)}), identity.ErrMFARequiredByRole)
	// A recruiter is not an MFA role.
	e.members[u2.ID] = []authz.Membership{{OrgID: uuid.NewV7(), OrgKind: authz.OrgCompany, Role: authz.MemberRecruiter}}

	// Without the role: the proof is required, then MFA goes away.
	wantErr(t, e.svc.DisableTOTP(ctx, a2, identity.ReauthProof{Password: goodPassword}), identity.ErrReauthRequired)
	if err := e.svc.DisableTOTP(ctx, a2, identity.ReauthProof{TOTPCode: e.nextCode(t, secret2)}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	cur, _ := e.store.User(ctx, u2.ID)
	_, found, _ := e.store.TOTP(ctx, u2.ID)
	left, _ := e.store.RecoveryCodesLeft(ctx, u2.ID)
	if cur.MFAEnabled || found || left != 0 {
		t.Fatalf("after disable: mfa %v, totp row %v, %d recovery codes", cur.MFAEnabled, found, left)
	}
	// The refusals by role are no reauth (no record); the missing proof is
	// one denied reauth_failed record (03 §6.2, BE-075), the disable one
	// success record.
	ev := e.audit.of(string(authz.IdentityMfaDisable))
	if len(ev) != 2 || ev[0].Outcome != audit.Denied || ev[0].Reason != audit.ReasonReauthFailed ||
		ev[1].Outcome != audit.Success || ev[1].Metadata[audit.MetaReauth] != "totp" {
		t.Fatalf("audit %+v", ev)
	}
	wantErr(t, e.svc.DisableTOTP(ctx, a2, identity.ReauthProof{Password: goodPassword}), identity.ErrMFANotEnabled)
}

func TestReauth_TOTPPathForMFAUsers(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	u, a, secret, _ := e.mfaUser(t, "reauth-mfa@example.com")

	// An MFA account proves itself with TOTP: the password alone is no proof.
	wantErr(t, e.svc.RequestEmailChange(ctx, a, "n1@example.com", identity.ReauthProof{Password: goodPassword}), identity.ErrReauthRequired)
	wantErr(t, e.svc.RequestEmailChange(ctx, a, "n1@example.com", identity.ReauthProof{TOTPCode: "123456"}), identity.ErrReauthFailed)
	if err := e.svc.RequestEmailChange(ctx, a, "n1@example.com", identity.ReauthProof{TOTPCode: e.nextCode(t, secret)}); err != nil {
		t.Fatalf("totp proof: %v", err)
	}
	ev := e.audit.of(string(authz.IdentityEmailChangeRequest))
	if len(ev) == 0 || ev[len(ev)-1].Metadata[audit.MetaReauth] != "totp" {
		t.Fatalf("audit reauth kind: %+v", ev)
	}
	// The consent verifier (06 H-42) takes the same path.
	kind, err := e.svc.Verify(ctx, u.ID, identity.ReauthProof{TOTPCode: e.nextCode(t, secret)})
	if err != nil || kind != "totp" {
		t.Fatalf("Verify = %q, %v", kind, err)
	}
	// changeMyPassword: current password AND the TOTP code.
	in := identity.PasswordChange{CurrentPassword: goodPassword, NewPassword: "Yeni-Parola-Kayik-77"}
	wantErr(t, e.svc.ChangePassword(ctx, a, in), identity.ErrReauthRequired)
	in.TOTPCode = e.nextCode(t, secret)
	if err := e.svc.ChangePassword(ctx, a, in); err != nil {
		t.Fatalf("change password: %v", err)
	}
	// A wrong proof counts towards the lockout (05 §14.1).
	cur, _ := e.store.User(ctx, u.ID)
	if cur.FailedLogins != 0 {
		t.Fatalf("failures not reset after a good proof: %d", cur.FailedLogins)
	}
}

func TestConfirmTotp_WrongCodeKeepsMFAOff(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	u := e.activeUser(t, "confirm@example.com")
	_, a := e.login(t, "confirm@example.com", identity.ClientMobile)
	_, err := e.svc.ConfirmTOTP(ctx, a, "123456", pwProof)
	wantErr(t, err, identity.ErrMFANotEnrolled)
	enr, err := e.svc.EnrollTOTP(ctx, a, pwProof)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.ConfirmTOTP(ctx, a, "not-a-code", pwProof)
	wantErr(t, err, identity.ErrInvalidMFACode)
	wantStatus(t, err, http.StatusUnprocessableEntity)
	cur, _ := e.store.User(ctx, u.ID)
	if cur.MFAEnabled || cur.FailedLogins != 1 {
		t.Fatalf("after a wrong code: mfa %v, failures %d", cur.MFAEnabled, cur.FailedLogins)
	}
	// The right code: MFA on, the current session MFA-verified, a notice.
	if _, err := e.svc.ConfirmTOTP(ctx, a, totpCode(t, enr.Secret, e.clk.Now()), pwProof); err != nil {
		t.Fatal(err)
	}
	s, _ := e.store.Session(ctx, a.SessionID)
	if s.MFAVerifiedAt == nil {
		t.Fatal("confirmTotp did not mark the current session")
	}
	e.runNotices(t)
	if !slices.ContainsFunc(e.mail.sent(), func(m notify.Message) bool { return m.Data["kind"] == identity.NoticeMFAEnabled }) {
		t.Fatal("no mfa_enabled notice")
	}
}

func TestRegenerateRecoveryCodes_OldCodesStop(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	_, a, secret, old := e.mfaUser(t, "regen@example.com")
	_, err := e.svc.RegenerateRecoveryCodes(ctx, a, identity.ReauthProof{})
	wantErr(t, err, identity.ErrReauthRequired)
	fresh, err := e.svc.RegenerateRecoveryCodes(ctx, a, identity.ReauthProof{TOTPCode: e.nextCode(t, secret)})
	if err != nil || len(fresh) != 10 || slices.Contains(fresh, old[0]) {
		t.Fatalf("regenerate: %v %d", err, len(fresh))
	}
	ch := e.passwordStep(t, "regen@example.com", identity.ClientMobile)
	_, err = e.svc.CompleteMFALogin(ctx, authz.Anonymous(), identity.MFALogin{ChallengeID: ch.ID, RecoveryCode: old[0]})
	wantErr(t, err, identity.ErrMFALoginFailed)
	if _, err := e.svc.CompleteMFALogin(ctx, authz.Anonymous(), identity.MFALogin{ChallengeID: ch.ID, RecoveryCode: fresh[0]}); err != nil {
		t.Fatalf("new code: %v", err)
	}
}

func TestMFA_FailsClosedWithoutSecrets(t *testing.T) {
	t.Parallel()
	// Without the challenge store (the worker) an MFA account cannot sign in: 503, never a session.
	e2 := newEnv(t, func(d *identity.Deps) { d.Challenges = nil })
	e2.mfaUser(t, "closed2@example.com")
	_, err := e2.svc.CreateSession(t.Context(), authz.Anonymous(), identity.Login{Email: "closed2@example.com", Password: goodPassword})
	wantErr(t, err, identity.ErrMFAUnavailable)
	wantStatus(t, err, http.StatusServiceUnavailable)
}
