package identity_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/platform/problem"
	"koro/internal/platform/problem/problemtest"
	"koro/internal/platform/queue"
	"koro/internal/platform/sms"
	"koro/internal/platform/sms/smsfake"
)

// Example numbers of the range the project uses in tests (never a real
// subscriber).
const (
	testPhone      = "+905550000001"
	testPhoneTyped = "0555 000 00 01"
	testPhone2     = "+905550000002"
)

// smsEnv is an identity module with the in-memory SMS adapter.
func smsEnv(t *testing.T, opts ...func(*identity.Deps)) (*env, *smsfake.Fake) {
	t.Helper()
	f := smsfake.New()
	return newEnv(t, append([]func(*identity.Deps){func(d *identity.Deps) { d.SMS = f }}, opts...)...), f
}

// sendSMS runs the queued identity.send_sms_code tasks (the worker) and
// leaves the other tasks queued.
func (e *env) sendSMS(t *testing.T) {
	t.Helper()
	h := e.smsTask(t)
	for _, x := range e.q.Tasks() {
		if x.Type == identity.SendSMSCodeTaskName {
			if err := h(workerCtx(t), x.Payload); err != nil {
				t.Fatalf("send SMS: %v", err)
			}
		}
	}
	var keep []queue.Enqueued
	for _, x := range e.q.Tasks() {
		if x.Type != identity.SendSMSCodeTaskName {
			keep = append(keep, x)
		}
	}
	e.q.Reset()
	for _, x := range keep {
		var p json.RawMessage = x.Payload
		if err := e.q.Enqueue(t.Context(), x.Type, p, ""); err != nil {
			t.Fatal(err)
		}
	}
}

// smsTask is the identity.send_sms_code handler.
func (e *env) smsTask(t *testing.T) func(ctx context.Context, payload []byte) error {
	t.Helper()
	for _, task := range e.mod.Tasks() {
		if task.Name == identity.SendSMSCodeTaskName {
			return task.Handle
		}
	}
	t.Fatal("identity.send_sms_code is not a task of identity")
	return nil
}

// lastSMSCode returns the code of the last SMS to number.
func lastSMSCode(t *testing.T, f *smsfake.Fake, number string) string {
	t.Helper()
	m, ok := f.Last(number)
	if !ok {
		t.Fatalf("no SMS to %s", identity.MaskPhone(number))
	}
	i := strings.Index(m.Text, ": ")
	if i < 0 || len(m.Text) < i+8 {
		t.Fatalf("SMS text %q has no code", m.Text)
	}
	return m.Text[i+2 : i+8]
}

// verifiedPhone gives the actor's account number by the public flow.
func (e *env) verifiedPhone(t *testing.T, f *smsfake.Fake, a authz.Actor, number string) {
	t.Helper()
	if _, err := e.svc.StartPhoneVerification(t.Context(), a, number, pwProof); err != nil {
		t.Fatalf("StartPhoneVerification: %v", err)
	}
	e.sendSMS(t)
	if _, err := e.svc.ConfirmPhoneVerification(t.Context(), a, lastSMSCode(t, f, number)); err != nil {
		t.Fatalf("ConfirmPhoneVerification: %v", err)
	}
}

// TestPhone_VerifyWithSMSCode: a typed number is normalized, a code goes by
// SMS (the worker; the task holds the code's row ID only), a wrong code is
// refused and audited, the right one verifies the number; /me shows it
// masked and a notice goes by e-mail.
func TestPhone_VerifyWithSMSCode(t *testing.T) {
	t.Parallel()
	e, f := smsEnv(t)
	u := e.activeUser(t, "phone@example.com")
	_, a := e.login(t, "phone@example.com", identity.ClientWeb)
	ctx := t.Context()
	sent, err := e.svc.StartPhoneVerification(ctx, a, testPhoneTyped, pwProof)
	if err != nil {
		t.Fatal(err)
	}
	if sent.PhoneHint != "***01" || !sent.ExpiresAt.Equal(e.clk.Now().Add(10*time.Minute)) {
		t.Fatalf("sent = %+v", sent)
	}
	if len(f.Sent()) != 0 {
		t.Fatal("an SMS left on the request path")
	}
	for _, x := range e.q.Tasks() {
		if x.Type == identity.SendSMSCodeTaskName {
			var p map[string]any
			if err := json.Unmarshal(x.Payload, &p); err != nil || len(p) != 1 || p["code_id"] == nil {
				t.Fatalf("task payload %s, want the code's row ID only", x.Payload)
			}
		}
	}
	e.sendSMS(t)
	code := lastSMSCode(t, f, testPhone)
	if m, _ := f.Last(testPhone); !strings.HasPrefix(m.Text, "Koro telefon dogrulama kodunuz: ") || len(m.Text) > sms.MaxText {
		t.Fatalf("SMS text %q", m.Text)
	}
	wrong := fmt.Sprintf("%06d", (mustAtoi(t, code)+1)%1_000_000)
	_, err = e.svc.ConfirmPhoneVerification(ctx, a, wrong)
	problemtest.WantErr(t, err, identity.ErrInvalidSMSCode)
	if d := e.audit.of(string(authz.IdentityPhoneVerify)); len(d) != 1 || d[0].Outcome != audit.Denied || d[0].Reason != "invalid_sms_code" {
		t.Fatalf("denied records %+v", d)
	}
	v, err := e.svc.ConfirmPhoneVerification(ctx, a, code)
	if err != nil || v.Hint != "***01" || v.SMSMFAEnabled {
		t.Fatalf("Confirm = %+v, %v", v, err)
	}
	me, err := e.svc.Me(ctx, a)
	if err != nil || me.Phone == nil || me.Phone.Hint != "***01" || !me.SMSAvailable {
		t.Fatalf("Me = %+v, %v", me.Phone, err)
	}
	// Single use.
	_, err = e.svc.ConfirmPhoneVerification(ctx, a, code)
	problemtest.WantErr(t, err, identity.ErrInvalidSMSCode)
	// The notice (masked number) and no full number or code in any audit record.
	e.runNotices(t)
	var notice bool
	for _, m := range e.mail.sent() {
		if m.Template == "identity.phone_notice" && m.Data["kind"] == identity.NoticePhoneVerified && m.Data["phone_hint"] == "***01" {
			notice = true
		}
	}
	if !notice {
		t.Fatal("no phone_verified notice")
	}
	for _, ev := range e.audit.events {
		b, _ := json.Marshal(ev)
		hasNoSecret(t, "audit "+ev.Action, string(b), testPhone, "5550000001", code)
	}
	if p, found, _ := e.store.Phone(ctx, u.ID); !found || p.Number != testPhone {
		t.Fatalf("stored phone %+v", p)
	}
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestPhone_StartRules: the proof is required (R), only Turkish mobile
// numbers, 503 without an SMS adapter, and the send limits per number.
func TestPhone_StartRules(t *testing.T) {
	t.Parallel()
	e, _ := smsEnv(t)
	e.activeUser(t, "rules@example.com")
	_, a := e.login(t, "rules@example.com", identity.ClientWeb)
	ctx := t.Context()
	_, err := e.svc.StartPhoneVerification(ctx, a, testPhone, identity.ReauthProof{})
	problemtest.WantErr(t, err, identity.ErrReauthRequired)
	_, err = e.svc.StartPhoneVerification(ctx, a, "+90 212 000 00 01", pwProof)
	problemtest.WantErr(t, err, identity.ErrInvalidPhone)
	for i := range 5 {
		if _, err := e.svc.StartPhoneVerification(ctx, a, testPhone, pwProof); err != nil {
			t.Fatalf("send %d: %v", i+1, err)
		}
	}
	_, err = e.svc.StartPhoneVerification(ctx, a, testPhone, pwProof)
	problemtest.WantErr(t, err, problem.ErrRateLimited)

	off := newEnv(t)
	off.activeUser(t, "off@example.com")
	_, b := off.login(t, "off@example.com", identity.ClientWeb)
	_, err = off.svc.StartPhoneVerification(ctx, b, testPhone, pwProof)
	problemtest.WantErr(t, err, identity.ErrSMSUnavailable)
	problemtest.WantErr(t, off.svc.EnableSMSMFA(ctx, b, pwProof), identity.ErrSMSUnavailable)
}

// TestPhone_TakenNumberToldOnlyAtConfirm: starting with another account's
// number answers like any start; only the holder, at confirmation, learns
// it is taken.
func TestPhone_TakenNumberToldOnlyAtConfirm(t *testing.T) {
	t.Parallel()
	e, f := smsEnv(t)
	e.activeUser(t, "first@example.com")
	_, a := e.login(t, "first@example.com", identity.ClientWeb)
	e.verifiedPhone(t, f, a, testPhone)
	e.activeUser(t, "second@example.com")
	_, b := e.login(t, "second@example.com", identity.ClientWeb)
	if _, err := e.svc.StartPhoneVerification(t.Context(), b, testPhone, pwProof); err != nil {
		t.Fatalf("start with a taken number = %v, want the same answer", err)
	}
	e.sendSMS(t)
	_, err := e.svc.ConfirmPhoneVerification(t.Context(), b, lastSMSCode(t, f, testPhone))
	problemtest.WantErr(t, err, identity.ErrPhoneTaken)
}

// TestSMSCode_AttemptsAndExpiry: a code allows five verifications (the right
// one after five wrong ones is refused), a new code replaces the open one,
// and a code expires after 10 minutes.
func TestSMSCode_AttemptsAndExpiry(t *testing.T) {
	t.Parallel()
	e, f := smsEnv(t)
	e.activeUser(t, "tries@example.com")
	_, a := e.login(t, "tries@example.com", identity.ClientWeb)
	ctx := t.Context()
	start := func() string {
		t.Helper()
		e.clk.Advance(time.Second)
		if _, err := e.svc.StartPhoneVerification(ctx, a, testPhone, pwProof); err != nil {
			t.Fatal(err)
		}
		e.sendSMS(t) //nolint:contextcheck // the worker runs with its own task context
		return lastSMSCode(t, f, testPhone)
	}
	code := start()
	for range 5 {
		_, err := e.svc.ConfirmPhoneVerification(ctx, a, "000000")
		problemtest.WantErr(t, err, identity.ErrInvalidSMSCode)
	}
	if code != "000000" {
		_, err := e.svc.ConfirmPhoneVerification(ctx, a, code)
		problemtest.WantErr(t, err, identity.ErrInvalidSMSCode)
	}
	old := start()
	code = start()
	if old != code {
		_, err := e.svc.ConfirmPhoneVerification(ctx, a, old)
		problemtest.WantErr(t, err, identity.ErrInvalidSMSCode)
	}
	e.clk.Advance(10 * time.Minute)
	_, err := e.svc.ConfirmPhoneVerification(ctx, a, code)
	problemtest.WantErr(t, err, identity.ErrInvalidSMSCode)
	code = start()
	if _, err := e.svc.ConfirmPhoneVerification(ctx, a, code[:3]+" "+code[3:]); err != nil {
		t.Fatalf("a spaced code: %v", err)
	}
}

// TestSMSMFA_RefusedForMFARoles: k-sms — a holder of an MFA role cannot
// turn the SMS second step on (409, before the proof); another user can,
// needs a verified number, and turns it off again (R).
func TestSMSMFA_RefusedForMFARoles(t *testing.T) {
	t.Parallel()
	e, f := smsEnv(t)
	ctx := t.Context()
	e.activeUser(t, "member@example.com")
	_, a := e.login(t, "member@example.com", identity.ClientWeb)
	problemtest.WantErr(t, e.svc.EnableSMSMFA(ctx, a, pwProof), identity.ErrPhoneNotVerified)
	e.verifiedPhone(t, f, a, testPhone)
	if err := e.svc.EnableSMSMFA(ctx, a, identity.ReauthProof{}); !errors.Is(err, identity.ErrReauthRequired) {
		t.Fatalf("enable without proof = %v", err)
	}
	if err := e.svc.EnableSMSMFA(ctx, a, pwProof); err != nil {
		t.Fatal(err)
	}
	if me, _ := e.svc.Me(ctx, a); me.Phone == nil || !me.Phone.SMSMFAEnabled {
		t.Fatalf("Me.Phone = %+v", me.Phone)
	}
	if err := e.svc.DisableSMSMFA(ctx, a, pwProof); err != nil {
		t.Fatal(err)
	}

	admin := e.activeUser(t, "owner@example.com")
	e.members[admin.ID] = []authz.Membership{{OrgID: uuid.NewV7(), OrgKind: authz.OrgCompany, Role: authz.MemberCompanyOwner}}
	_, b := e.login(t, "owner@example.com", identity.ClientWeb)
	b.Memberships = e.members[admin.ID]
	e.verifiedPhone(t, f, b, testPhone2)
	problemtest.WantErr(t, e.svc.EnableSMSMFA(ctx, b, identity.ReauthProof{}), identity.ErrSMSMFANotAllowed)
}

// smsMFAUser is an active account with a verified number and the SMS second
// step on.
func (e *env) smsMFAUser(t *testing.T, f *smsfake.Fake, email, number string) identity.User {
	t.Helper()
	u := e.activeUser(t, email)
	_, a := e.login(t, email, identity.ClientWeb)
	e.verifiedPhone(t, f, a, number)
	if err := e.svc.EnableSMSMFA(t.Context(), a, pwProof); err != nil {
		t.Fatal(err)
	}
	return u
}

// TestSMSMFA_LoginNeverMFAVerified: the password step of an account with the
// SMS second step answers a challenge (methods [sms], the masked number);
// the SMS code opens a session that is not MFA-verified; a code is single
// use; a wrong one counts towards the lockout like a TOTP code.
func TestSMSMFA_LoginNeverMFAVerified(t *testing.T) {
	t.Parallel()
	e, f := smsEnv(t)
	ctx := t.Context()
	u := e.smsMFAUser(t, f, "smslogin@example.com", testPhone)
	ch := e.passwordStep(t, "smslogin@example.com", identity.ClientMobile)
	if strings.Join(ch.Methods, ",") != "sms" || ch.PhoneHint != "***01" {
		t.Fatalf("challenge = %+v", ch)
	}
	// No TOTP: a TOTP code is a wrong code, not a second path.
	_, err := e.svc.CompleteMFALogin(ctx, authz.Anonymous(), identity.MFALogin{ChallengeID: ch.ID, TOTPCode: "123456"})
	problemtest.WantErr(t, err, identity.ErrMFALoginFailed)
	sent, err := e.svc.SendMFALoginSMS(ctx, authz.Anonymous(), ch.ID)
	if err != nil || sent.PhoneHint != "***01" || sent.ExpiresAt.After(e.clk.Now().Add(identity.ChallengeTTL)) {
		t.Fatalf("SendMFALoginSMS = %+v, %v", sent, err)
	}
	e.sendSMS(t)
	code := lastSMSCode(t, f, testPhone)
	res, err := e.svc.CompleteMFALogin(ctx, authz.Anonymous(), identity.MFALogin{ChallengeID: ch.ID, SMSCode: code})
	if err != nil {
		t.Fatalf("CompleteMFALogin(sms) = %v", err)
	}
	if res.Session.MFAVerifiedAt != nil || res.Token == "" {
		t.Fatalf("an SMS sign-in is MFA-verified: %+v", res.Session)
	}
	created := e.audit.of(string(authz.IdentitySessionCreate))
	if last := created[len(created)-1]; last.Outcome != audit.Success || last.Metadata["mfa"] != "sms" {
		t.Fatalf("sign-in record %+v", last)
	}
	// Single use: the code of a used challenge does not open another session.
	ch2 := e.passwordStep(t, "smslogin@example.com", identity.ClientMobile)
	_, err = e.svc.CompleteMFALogin(ctx, authz.Anonymous(), identity.MFALogin{ChallengeID: ch2.ID, SMSCode: code})
	problemtest.WantErr(t, err, identity.ErrMFALoginFailed)
	if cur, _ := e.store.User(ctx, u.ID); cur.FailedLogins == 0 {
		t.Fatal("a wrong SMS code did not count towards the lockout")
	}
	// Exactly one factor per body.
	_, err = e.svc.CompleteMFALogin(ctx, authz.Anonymous(), identity.MFALogin{ChallengeID: ch2.ID, SMSCode: code, TOTPCode: "123456"})
	problemtest.WantErr(t, err, problem.ErrValidation)
}

// TestSMSMFA_ChallengeSendLimit: at most three SMS codes per challenge; an
// unknown challenge or an account without the SMS second step answers like
// completeMfaLogin.
func TestSMSMFA_ChallengeSendLimit(t *testing.T) {
	t.Parallel()
	e, f := smsEnv(t)
	ctx := t.Context()
	e.smsMFAUser(t, f, "limit@example.com", testPhone)
	ch := e.passwordStep(t, "limit@example.com", identity.ClientMobile)
	for i := range 3 {
		if _, err := e.svc.SendMFALoginSMS(ctx, authz.Anonymous(), ch.ID); err != nil {
			t.Fatalf("send %d: %v", i+1, err)
		}
	}
	_, err := e.svc.SendMFALoginSMS(ctx, authz.Anonymous(), ch.ID)
	problemtest.WantErr(t, err, problem.ErrRateLimited)
	_, err = e.svc.SendMFALoginSMS(ctx, authz.Anonymous(), uuid.NewV4())
	problemtest.WantErr(t, err, identity.ErrMFALoginFailed)

	_, _, _, _ = e.mfaUser(t, "totp@example.com")
	tch := e.passwordStep(t, "totp@example.com", identity.ClientMobile)
	if strings.Join(tch.Methods, ",") != "totp,recovery_code" || tch.PhoneHint != "" {
		t.Fatalf("TOTP challenge = %+v", tch)
	}
	_, err = e.svc.SendMFALoginSMS(ctx, authz.Anonymous(), tch.ID)
	problemtest.WantErr(t, err, identity.ErrMFALoginFailed)
}

// TestSMSMFA_NotOfferedNextToTOTPForMFARole: k-sms at sign-in — a holder of
// an MFA role with TOTP on signs in with TOTP alone, even if the SMS second
// step was on before the role arrived; without the role, both are offered.
func TestSMSMFA_NotOfferedNextToTOTPForMFARole(t *testing.T) {
	t.Parallel()
	e, f := smsEnv(t)
	u := e.smsMFAUser(t, f, "both@example.com", testPhone)
	_, a := e.login2(t, f, "both@example.com")
	e.enableMFA(t, a)
	ch := e.passwordStep(t, "both@example.com", identity.ClientMobile)
	if strings.Join(ch.Methods, ",") != "totp,recovery_code,sms" {
		t.Fatalf("methods without a role = %v", ch.Methods)
	}
	e.members[u.ID] = []authz.Membership{{OrgID: uuid.NewV7(), OrgKind: authz.OrgCompany, Role: authz.MemberCompanyAdmin}}
	ch = e.passwordStep(t, "both@example.com", identity.ClientMobile)
	if strings.Join(ch.Methods, ",") != "totp,recovery_code" {
		t.Fatalf("methods of an MFA role = %v", ch.Methods)
	}
	_, err := e.svc.SendMFALoginSMS(t.Context(), authz.Anonymous(), ch.ID)
	problemtest.WantErr(t, err, identity.ErrMFALoginFailed)
}

// login2 opens a session of an account whose sign-in may need a second
// factor: the SMS one, through the public flow.
func (e *env) login2(t *testing.T, f *smsfake.Fake, email string) (identity.LoginResult, authz.Actor) {
	t.Helper()
	ctx := t.Context()
	res, err := e.svc.CreateSession(ctx, authz.Anonymous(), identity.Login{Email: email, Password: goodPassword, Client: identity.ClientMobile})
	if err != nil {
		t.Fatal(err)
	}
	if res.MFARequired {
		if _, err := e.svc.SendMFALoginSMS(ctx, authz.Anonymous(), res.Challenge.ID); err != nil {
			t.Fatal(err)
		}
		u, _, _ := e.svc.FindByEmail(ctx, email)
		p, _, _ := e.store.Phone(ctx, u)
		e.sendSMS(t)
		if res, err = e.svc.CompleteMFALogin(ctx, authz.Anonymous(), identity.MFALogin{ChallengeID: res.Challenge.ID,
			SMSCode: lastSMSCode(t, f, p.Number)}); err != nil {
			t.Fatal(err)
		}
	}
	return res, authz.Actor{Kind: authz.KindUser, UserID: res.Session.UserID, SessionID: res.Session.ID, StudentStatus: authz.StudentNone}
}

// TestPasswordResetSMS_EligibleOnly: an eligible account resets with the SMS
// code (every session closes); an account with the SMS second step, or of
// an MFA role, gets the e-mail link instead; an unknown address answers
// alike; a wrong code is refused.
func TestPasswordResetSMS_EligibleOnly(t *testing.T) {
	t.Parallel()
	e, f := smsEnv(t)
	ctx := t.Context()
	u := e.activeUser(t, "reset@example.com")
	_, a := e.login(t, "reset@example.com", identity.ClientWeb)
	e.verifiedPhone(t, f, a, testPhone)
	before := len(f.Sent())
	if err := e.svc.RequestPasswordResetSMS(ctx, authz.Anonymous(), "reset@example.com"); err != nil {
		t.Fatal(err)
	}
	e.sendSMS(t)
	if len(f.Sent()) != before+1 {
		t.Fatal("no reset SMS")
	}
	code := lastSMSCode(t, f, testPhone)
	newPW := "Yepyeni-Uzun-Parola-2026"
	err := e.svc.ConfirmPasswordResetSMS(ctx, authz.Anonymous(), identity.PasswordResetSMS{Email: "reset@example.com", Code: "000000", NewPassword: newPW})
	if code != "000000" {
		problemtest.WantErr(t, err, identity.ErrInvalidSMSCode)
	}
	err = e.svc.ConfirmPasswordResetSMS(ctx, authz.Anonymous(), identity.PasswordResetSMS{Email: "ghost@example.com", Code: code, NewPassword: newPW})
	problemtest.WantErr(t, err, identity.ErrInvalidSMSCode)
	if err := e.svc.ConfirmPasswordResetSMS(ctx, authz.Anonymous(), identity.PasswordResetSMS{Email: "reset@example.com", Code: code, NewPassword: newPW}); err != nil {
		t.Fatalf("reset by SMS: %v", err)
	}
	for _, s := range e.sessionsOf(t, u.ID) {
		if s.RevokedAt == nil {
			t.Fatal("a session survived the reset")
		}
	}
	if _, err := e.svc.CreateSession(ctx, authz.Anonymous(), identity.Login{Email: "reset@example.com", Password: newPW}); err != nil {
		t.Fatalf("login with the new password: %v", err)
	}

	// The SMS second step on: the reset goes by e-mail (one SIM swap must not open the account).
	e.smsMFAUser(t, f, "mfa.reset@example.com", testPhone2)
	before = len(f.Sent())
	if err := e.svc.RequestPasswordResetSMS(ctx, authz.Anonymous(), "mfa.reset@example.com"); err != nil {
		t.Fatal(err)
	}
	e.sendSMS(t)
	if len(f.Sent()) != before {
		t.Fatal("a reset SMS to an account whose second step is SMS")
	}
	if tok := e.lastToken(t, "identity.password_reset"); tok == "" {
		t.Fatal("no reset e-mail instead")
	}
	if err := e.svc.RequestPasswordResetSMS(ctx, authz.Anonymous(), "nobody@example.com"); err != nil {
		t.Fatalf("unknown address = %v, want 202", err)
	}
}

// TestSMSCode_WorkerSendsOnceAndHidesTheCode: the worker re-derives the code
// (never stored), sends it once, maps a refusal of the provider to a
// permanent failure, retries anything else, and logs neither the number
// nor the code.
func TestSMSCode_WorkerSendsOnceAndHidesTheCode(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	e, f := smsEnv(t, func(d *identity.Deps) {
		d.Log = slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	e.activeUser(t, "worker@example.com")
	_, a := e.login(t, "worker@example.com", identity.ClientWeb)
	ctx := t.Context()
	if _, err := e.svc.StartPhoneVerification(ctx, a, testPhone, pwProof); err != nil {
		t.Fatal(err)
	}
	var task queue.Enqueued
	for _, x := range e.q.Tasks() {
		if x.Type == identity.SendSMSCodeTaskName {
			task = x
		}
	}
	h, wctx := e.smsTask(t), workerCtx(t)
	f.FailNext(fmt.Errorf("x: %w", sms.ErrThrottled))
	if err := h(wctx, task.Payload); err == nil || queue.IsPermanent(err) {
		t.Fatalf("throttled send = %v, want a retry", err)
	}
	f.FailNext(fmt.Errorf("x: %w: %w", sms.ErrRejected, sms.ErrInvalidNumber))
	if err := h(wctx, task.Payload); !queue.IsPermanent(err) {
		t.Fatalf("refused send = %v, want permanent", err)
	}
	if err := h(wctx, task.Payload); err != nil {
		t.Fatal(err)
	}
	if err := h(wctx, task.Payload); err != nil || len(f.Sent()) != 1 {
		t.Fatalf("a second run sent again (%d sent, %v)", len(f.Sent()), err)
	}
	code := lastSMSCode(t, f, testPhone)
	hasNoSecret(t, "logs", logs.String(), testPhone, "5550000001", code)
	if !strings.Contains(logs.String(), "SMS code sent") {
		t.Fatal("no send log line")
	}
}

// TestPhone_RemoveAndPrivacy: removing the number (R) ends the SMS second
// step; the KVKK export holds the subject's own number, erasure deletes the
// number and the codes.
func TestPhone_RemoveAndPrivacy(t *testing.T) {
	t.Parallel()
	e, f := smsEnv(t)
	ctx := t.Context()
	u := e.smsMFAUser(t, f, "kvkk@example.com", testPhone)
	sec, err := e.mod.Privacy.Export(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	var exported bool
	for _, it := range sec.Items {
		if it["type"] == "phone" && it["phone"] == testPhone {
			exported = true
		}
	}
	if !exported {
		t.Fatalf("export items %v", sec.Items)
	}
	if _, err := e.mod.Privacy.Erase(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := e.store.Phone(ctx, u.ID); found {
		t.Fatal("the number survived the erasure")
	}
	if _, found, _ := e.store.OpenSMSCode(ctx, u.ID, identity.SMSLogin); found {
		t.Fatal("an SMS code survived the erasure")
	}

	v := e.smsMFAUser(t, f, "remove@example.com", testPhone2)
	_, a := e.login2(t, f, "remove@example.com")
	problemtest.WantErr(t, e.svc.RemovePhone(ctx, a, identity.ReauthProof{}), identity.ErrReauthRequired)
	if err := e.svc.RemovePhone(ctx, a, pwProof); err != nil {
		t.Fatal(err)
	}
	problemtest.WantErr(t, e.svc.RemovePhone(ctx, a, pwProof), identity.ErrPhoneNotVerified)
	res, err := e.svc.CreateSession(ctx, authz.Anonymous(), identity.Login{Email: "remove@example.com", Password: goodPassword})
	if err != nil || res.MFARequired || res.Session.UserID != v.ID {
		t.Fatalf("login after removing the number = %+v, %v", res, err)
	}
}

// TestAdminResetUserMfa_ClearsSMSSecondStep: the admin MFA reset (k2) lets a
// user who lost the phone in again: the SMS second step goes off, the
// number stays.
func TestAdminResetUserMfa_ClearsSMSSecondStep(t *testing.T) {
	t.Parallel()
	s := newStaffEnv(t)
	ctx := t.Context()
	now := s.clk.Now()
	if err := s.store.PutPhone(ctx, identity.Phone{UserID: s.target.ID, Number: testPhone, VerifiedAt: now, SMSMFAEnabledAt: &now}); err != nil {
		t.Fatal(err)
	}
	if p, err := s.svc.ResetUserMFA(ctx, s.a, s.target.ID, "lost phone, id checked", proof); err != nil || p != nil {
		t.Fatal(err, p)
	}
	p, found, _ := s.store.Phone(ctx, s.target.ID)
	if !found || p.SMSMFAEnabledAt != nil || p.Number != testPhone {
		t.Fatalf("phone after the reset = %+v, %v", p, found)
	}
	ev := s.audit.of(string(authz.IdentityUserResetMfa))
	if len(ev) != 1 || ev[0].Metadata["sms_mfa_reset"] != true {
		t.Fatalf("reset record %+v", ev)
	}
}
