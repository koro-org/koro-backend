package identity_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/platform/problem"
	"koro/internal/platform/problem/problemtest"
	"koro/internal/platform/ratelimit"
	"koro/internal/platform/sms"
	"koro/internal/platform/sms/smsfake"
)

// seededPhone is an active account holding number, seeded without spending
// an SMS limit; smsMFA turns the SMS second step on, otherwise the password
// reset by SMS has been on for its wait.
func (e *env) seededPhone(t *testing.T, email, number string, smsMFA bool) identity.User {
	t.Helper()
	u := e.activeUser(t, email)
	now := e.clk.Now()
	p := identity.Phone{UserID: u.ID, Number: number, VerifiedAt: now}
	if smsMFA {
		p.SMSMFAEnabledAt = &now
	} else {
		on := now.Add(-identity.SMSResetWait)
		p.SMSResetEnabledAt = &on
	}
	if err := e.store.PutPhone(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	return u
}

// loggedIn is the actor of a new active account.
func (e *env) loggedIn(t *testing.T, email string) authz.Actor {
	t.Helper()
	e.activeUser(t, email)
	_, a := e.login(t, email, identity.ClientWeb)
	return a
}

// smsCalls returns the limiter calls of the SMS rules made since from.
func smsCalls(l *ratelimit.Mem, from int) []string {
	var out []string
	for _, c := range l.Calls()[from:] {
		if strings.HasPrefix(c, "sms_") {
			out = append(out, c)
		}
	}
	return out
}

// spend takes n requests of (rule, key) from the limiter.
func spend(t *testing.T, l *ratelimit.Mem, name string, rate int, per time.Duration, key string) {
	t.Helper()
	for range rate {
		if _, err := l.Allow(t.Context(), ratelimit.Rule{Name: name, Rate: rate, Per: per}, key); err != nil {
			t.Fatal(err)
		}
	}
}

// smsCount is one value of koro_sms_codes_total of e's process.
func (e *env) smsCount(purpose identity.SMSPurpose, outcome string) uint64 {
	for _, s := range e.mod.SMSCounters() {
		if s.Purpose == string(purpose) && s.Outcome == outcome {
			return s.Value
		}
	}
	return 0
}

// wantRetryAfter checks err carries a wait of at most limit.
func wantRetryAfter(t *testing.T, err error, limit time.Duration) {
	t.Helper()
	pe, ok := errors.AsType[*problem.Error](err)
	if !ok || pe.RetryAfter() <= 0 || pe.RetryAfter() > limit || pe.GetHeaders().Get("Retry-After") == "" {
		t.Fatalf("error %v has no wait of at most %s", err, limit)
	}
}

// TestSMSLimits_Order: a send spends the client IP, the account, the number
// of its purpose, the daily limit of a new number and the budget, in this
// order.
func TestSMSLimits_Order(t *testing.T) {
	t.Parallel()
	e, _ := smsEnv(t)
	a := e.loggedIn(t, "order@example.com")
	from := len(e.lim.Calls())
	if _, err := e.svc.StartPhoneVerification(t.Context(), a, testPhone, pwProof, ""); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range smsCalls(e.lim, from) {
		rule, key, _ := strings.Cut(c, "|")
		kind, _, _ := strings.Cut(key, ":")
		names = append(names, rule+"/"+kind)
	}
	want := []string{"sms_ip/ip", "sms_user/user", "sms_number/phone_verify", "sms_new_number/phone",
		"sms_budget_general_hour/all", "sms_budget_general_day/all", "sms_budget_hour/all", "sms_budget_day/all"}
	if !slices.Equal(names, want) {
		t.Fatalf("limits = %v, want %v", names, want)
	}
}

// TestSMSLimits_RefusalSpendsNothingFurther: a send refused by the client IP
// or by the account limit does not touch the limits after it.
func TestSMSLimits_RefusalSpendsNothingFurther(t *testing.T) {
	t.Parallel()
	e, _ := smsEnv(t)
	ctx := t.Context()
	a := e.loggedIn(t, "account@example.com")
	for i := range 5 {
		if _, err := e.svc.StartPhoneVerification(ctx, a, fmt.Sprintf("+90555000010%d", i), pwProof, ""); err != nil {
			t.Fatalf("send %d: %v", i+1, err)
		}
	}
	from := len(e.lim.Calls())
	_, err := e.svc.StartPhoneVerification(ctx, a, testPhone, pwProof, "")
	problemtest.WantErr(t, err, problem.ErrRateLimited)
	if calls := smsCalls(e.lim, from); len(calls) != 2 || !strings.HasPrefix(calls[1], "sms_user|") {
		t.Fatalf("calls of the refused send = %v", calls)
	}
	// The number kept its whole daily limit for another account.
	b := e.loggedIn(t, "other@example.com")
	for i := range 3 {
		if _, err := e.svc.StartPhoneVerification(ctx, b, testPhone, pwProof, ""); err != nil {
			t.Fatalf("send %d to the untouched number: %v", i+1, err)
		}
	}

	spend(t, e.lim, "sms_ip", 30, time.Hour, "ip:unknown")
	c := e.loggedIn(t, "third@example.com")
	from = len(e.lim.Calls())
	_, err = e.svc.StartPhoneVerification(ctx, c, testPhone2, pwProof, "")
	problemtest.WantErr(t, err, problem.ErrRateLimited)
	if calls := smsCalls(e.lim, from); len(calls) != 1 || !strings.HasPrefix(calls[0], "sms_ip|") {
		t.Fatalf("calls of the send refused by the IP limit = %v", calls)
	}
}

// TestSMSLimits_PurposesKeepTheirOwnBuckets: with the number bucket of phone
// verification and the client IP bucket of the other purposes spent, the
// number's sign-in code still goes.
func TestSMSLimits_PurposesKeepTheirOwnBuckets(t *testing.T) {
	t.Parallel()
	e, f := smsEnv(t)
	ctx := t.Context()
	e.seededPhone(t, "holder@example.com", testPhone, true)
	a := e.loggedIn(t, "verifier@example.com")
	from := len(e.lim.Calls())
	if _, err := e.svc.StartPhoneVerification(ctx, a, testPhone, pwProof, ""); err != nil {
		t.Fatal(err)
	}
	var key string
	for _, c := range smsCalls(e.lim, from) {
		if rest, ok := strings.CutPrefix(c, "sms_number|"); ok {
			key = rest
		}
	}
	if !strings.HasPrefix(key, "phone_verify:") {
		t.Fatalf("number bucket key = %q", key)
	}
	spend(t, e.lim, "sms_number", 5, time.Hour, key)
	_, err := e.svc.StartPhoneVerification(ctx, a, testPhone, pwProof, "")
	problemtest.WantErr(t, err, problem.ErrRateLimited)
	spend(t, e.lim, "sms_ip", 30, time.Hour, "ip:unknown")

	ch := e.passwordStep(t, "holder@example.com", identity.ClientMobile)
	if _, err := e.svc.SendMFALoginSMS(ctx, authz.Anonymous(), ch.ID); err != nil {
		t.Fatalf("the sign-in code = %v", err)
	}
	e.sendSMS(t)
	if m, ok := f.Last(testPhone); !ok || !strings.HasPrefix(m.Text, "Koro giris kodunuz: ") {
		t.Fatalf("last SMS = %v, %v", m, ok)
	}
}

// TestSMSLimits_NewNumberDailyLimit: a number an account does not hold takes
// three verification codes a day, whichever accounts ask; the number an
// account holds is outside this limit.
func TestSMSLimits_NewNumberDailyLimit(t *testing.T) {
	t.Parallel()
	e, _ := smsEnv(t)
	ctx := t.Context()
	a, b := e.loggedIn(t, "a@example.com"), e.loggedIn(t, "b@example.com")
	for _, actor := range []authz.Actor{a, b, a} {
		if _, err := e.svc.StartPhoneVerification(ctx, actor, testPhone, pwProof, ""); err != nil {
			t.Fatal(err)
		}
	}
	_, err := e.svc.StartPhoneVerification(ctx, b, testPhone, pwProof, "")
	problemtest.WantErr(t, err, problem.ErrRateLimited)
	wantRetryAfter(t, err, 24*time.Hour)
	e.clk.Advance(24 * time.Hour)
	if _, err := e.svc.StartPhoneVerification(ctx, b, testPhone, pwProof, ""); err != nil {
		t.Fatalf("a day later = %v", err)
	}

	e.seededPhone(t, "own@example.com", testPhone2, false)
	_, c := e.login(t, "own@example.com", identity.ClientWeb)
	for i := range 5 {
		if _, err := e.svc.StartPhoneVerification(ctx, c, testPhone2, pwProof, ""); err != nil {
			t.Fatalf("send %d to the account's own number: %v", i+1, err)
		}
	}
}

// budgetEnv is an SMS module with a budget of 5 an hour: 4 for the purposes
// other than sign-in, the fifth for sign-in only.
func budgetEnv(t *testing.T, opts ...func(*identity.Deps)) (*env, *smsfake.Fake) {
	t.Helper()
	return smsEnv(t, append([]func(*identity.Deps){func(d *identity.Deps) {
		d.SMSBudget = identity.SMSBudget{PerHour: 5, PerDay: 100}
	}}, opts...)...)
}

// spendGeneralBudget uses up the share of the purposes other than sign-in.
func (e *env) spendGeneralBudget(t *testing.T, n int) {
	t.Helper()
	for i := range n {
		a := e.loggedIn(t, fmt.Sprintf("spender%d@example.com", i))
		if _, err := e.svc.StartPhoneVerification(t.Context(), a, fmt.Sprintf("+90555000020%d", i), pwProof, ""); err != nil {
			t.Fatalf("send %d: %v", i+1, err)
		}
	}
}

// TestSMSBudget_SpentAnswers503: a spent budget answers 503
// identity.sms_unavailable with Retry-After, opens no code, counts the
// refusal and reopens as the limiter refills.
func TestSMSBudget_SpentAnswers503(t *testing.T) {
	t.Parallel()
	e, f := budgetEnv(t)
	ctx := t.Context()
	e.spendGeneralBudget(t, 4)
	a := e.loggedIn(t, "late@example.com")
	_, err := e.svc.StartPhoneVerification(ctx, a, testPhone, pwProof, "")
	problemtest.WantErr(t, err, identity.ErrSMSUnavailable)
	wantStatus(t, err, http.StatusServiceUnavailable)
	wantRetryAfter(t, err, time.Hour)
	if n := e.smsCount(identity.SMSPhoneVerify, identity.SMSOutcomeBudgetRefused); n != 1 {
		t.Fatalf("budget_refused = %d", n)
	}
	e.sendSMS(t)
	if len(f.Sent()) != 4 {
		t.Fatalf("%d SMS sent, want the 4 the budget allowed", len(f.Sent()))
	}
	if n := e.smsCount(identity.SMSPhoneVerify, identity.SMSOutcomeSent); n != 4 {
		t.Fatalf("sent = %d", n)
	}
	e.clk.Advance(time.Hour)
	if _, err := e.svc.StartPhoneVerification(ctx, a, testPhone, pwProof, ""); err != nil {
		t.Fatalf("after the refill = %v", err)
	}
}

// TestSMSBudget_DailyCeiling: the daily value is a ceiling of its own.
func TestSMSBudget_DailyCeiling(t *testing.T) {
	t.Parallel()
	e, _ := smsEnv(t, func(d *identity.Deps) { d.SMSBudget = identity.SMSBudget{PerHour: 100, PerDay: 5} })
	e.spendGeneralBudget(t, 4)
	a := e.loggedIn(t, "late@example.com")
	_, err := e.svc.StartPhoneVerification(t.Context(), a, testPhone, pwProof, "")
	problemtest.WantErr(t, err, identity.ErrSMSUnavailable)
	wantRetryAfter(t, err, 24*time.Hour)
}

// TestSMSBudget_SignInKeepsItsShare: when the other purposes have spent their
// share, sign-in codes go on up to the whole budget; then they answer 503
// and the challenge stays usable.
func TestSMSBudget_SignInKeepsItsShare(t *testing.T) {
	t.Parallel()
	e, _ := budgetEnv(t)
	ctx := t.Context()
	e.seededPhone(t, "first@example.com", testPhone, true)
	e.seededPhone(t, "second@example.com", testPhone2, true)
	e.spendGeneralBudget(t, 4)
	ch := e.passwordStep(t, "first@example.com", identity.ClientMobile)
	if _, err := e.svc.SendMFALoginSMS(ctx, authz.Anonymous(), ch.ID); err != nil {
		t.Fatalf("a sign-in code within its share = %v", err)
	}
	ch = e.passwordStep(t, "second@example.com", identity.ClientMobile)
	for range 2 {
		_, err := e.svc.SendMFALoginSMS(ctx, authz.Anonymous(), ch.ID)
		problemtest.WantErr(t, err, identity.ErrSMSUnavailable)
		wantRetryAfter(t, err, time.Hour)
	}
	if n := e.smsCount(identity.SMSLogin, identity.SMSOutcomeBudgetRefused); n != 2 {
		t.Fatalf("budget_refused of sign-in = %d", n)
	}
}

// TestSMSBudget_ResetGoesByEmail: with the budget spent a password reset
// answers as ever and the link goes by e-mail.
func TestSMSBudget_ResetGoesByEmail(t *testing.T) {
	t.Parallel()
	e, f := budgetEnv(t)
	ctx := t.Context()
	e.seededPhone(t, "reset@example.com", testPhone, false)
	e.spendGeneralBudget(t, 4)
	e.sendSMS(t)
	before := len(f.Sent())
	if err := e.svc.RequestPasswordResetSMS(ctx, authz.Anonymous(), "reset@example.com"); err != nil {
		t.Fatalf("request = %v, want the usual answer", err)
	}
	e.sendSMS(t)
	if len(f.Sent()) != before {
		t.Fatal("an SMS over the budget")
	}
	if tok := e.lastToken(t, "identity.password_reset"); tok == "" {
		t.Fatal("no reset e-mail")
	}
	if n := e.smsCount(identity.SMSPasswordReset, identity.SMSOutcomeBudgetRefused); n != 1 {
		t.Fatalf("budget_refused = %d", n)
	}
}

// TestSMSBudget_SharedByProcesses: two processes of the API over one limiter
// spend one budget.
func TestSMSBudget_SharedByProcesses(t *testing.T) {
	t.Parallel()
	first, _ := budgetEnv(t)
	second, _ := budgetEnv(t, func(d *identity.Deps) { d.Limiter = first.lim })
	first.spendGeneralBudget(t, 4)
	a := second.loggedIn(t, "second@example.com")
	_, err := second.svc.StartPhoneVerification(t.Context(), a, testPhone, pwProof, "")
	problemtest.WantErr(t, err, identity.ErrSMSUnavailable)
}

// budgetDown is a limiter whose budget rules fail.
type budgetDown struct{ identity.Limiter }

func (l budgetDown) Allow(ctx context.Context, rule ratelimit.Rule, key string) (ratelimit.Result, error) {
	if strings.HasPrefix(rule.Name, "sms_budget") {
		return ratelimit.Result{}, ratelimit.ErrUnavailable
	}
	return l.Limiter.Allow(ctx, rule, key)
}

// TestSMSBudget_LimiterDownFailsClosed: without the shared counter no code
// opens: 503 for a phone verification, the e-mail link for a reset.
func TestSMSBudget_LimiterDownFailsClosed(t *testing.T) {
	t.Parallel()
	e, f := smsEnv(t, func(d *identity.Deps) { d.Limiter = budgetDown{d.Limiter} })
	ctx := t.Context()
	e.seededPhone(t, "reset@example.com", testPhone, false)
	a := e.loggedIn(t, "down@example.com")
	_, err := e.svc.StartPhoneVerification(ctx, a, testPhone2, pwProof, "")
	problemtest.WantErr(t, err, problem.ErrUnavailable)
	if err := e.svc.RequestPasswordResetSMS(ctx, authz.Anonymous(), "reset@example.com"); err != nil {
		t.Fatal(err)
	}
	e.sendSMS(t)
	if len(f.Sent()) != 0 {
		t.Fatal("an SMS without the budget's counter")
	}
	if tok := e.lastToken(t, "identity.password_reset"); tok == "" {
		t.Fatal("no reset e-mail")
	}
}

// TestSMSBudget_ZeroTakesTheDefault: a budget left at zero is not unlimited.
func TestSMSBudget_ZeroTakesTheDefault(t *testing.T) {
	t.Parallel()
	e, _ := smsEnv(t)
	spend(t, e.lim, "sms_budget_general_hour", identity.DefaultSMSBudget.PerHour-identity.DefaultSMSBudget.PerHour/5, time.Hour, "all")
	a := e.loggedIn(t, "zero@example.com")
	_, err := e.svc.StartPhoneVerification(t.Context(), a, testPhone, pwProof, "")
	problemtest.WantErr(t, err, identity.ErrSMSUnavailable)
}

// TestSMSCounters_WorkerOutcomes: the worker counts a refusal of the provider
// and a passing failure by purpose.
func TestSMSCounters_WorkerOutcomes(t *testing.T) {
	t.Parallel()
	e, f := smsEnv(t)
	a := e.loggedIn(t, "counts@example.com")
	if _, err := e.svc.StartPhoneVerification(t.Context(), a, testPhone, pwProof, ""); err != nil {
		t.Fatal(err)
	}
	var payload []byte
	for _, x := range e.q.Tasks() {
		if x.Type == identity.SendSMSCodeTaskName {
			payload = x.Payload
		}
	}
	h, wctx := e.smsTask(t), workerCtx(t)
	f.FailNext(fmt.Errorf("x: %w", sms.ErrThrottled))
	if err := h(wctx, payload); err == nil {
		t.Fatal("a throttled send succeeded")
	}
	f.FailNext(fmt.Errorf("x: %w", sms.ErrRejected))
	if err := h(wctx, payload); err == nil {
		t.Fatal("a refused send succeeded")
	}
	if err := h(wctx, payload); err != nil {
		t.Fatal(err)
	}
	for outcome, want := range map[string]uint64{identity.SMSOutcomeRetried: 1, identity.SMSOutcomeRejected: 1, identity.SMSOutcomeSent: 1} {
		if n := e.smsCount(identity.SMSPhoneVerify, outcome); n != want {
			t.Fatalf("%s = %d, want %d", outcome, n, want)
		}
	}
}

// resetNotices counts the e-mails about the SMS reset ceiling.
func (e *env) resetNotices(t *testing.T) int {
	t.Helper()
	e.runNotices(t)
	n := 0
	for _, m := range e.mail.sent() {
		if m.Template == "identity.phone_notice" && m.Data["kind"] == identity.NoticeSMSResetLimit {
			n++
		}
	}
	return n
}

// TestPasswordResetSMS_DailyCodeCeiling: an account gets ten reset codes in a
// rolling day; the holder is told once by e-mail; the next requests answer as
// ever and the link goes by e-mail; a day later SMS is back.
func TestPasswordResetSMS_DailyCodeCeiling(t *testing.T) {
	t.Parallel()
	e, f := smsEnv(t)
	ctx := t.Context()
	e.seededPhone(t, "codes@example.com", testPhone, false)
	request := func() {
		t.Helper()
		e.clk.Advance(time.Hour) // the hourly limits are not what this test is about
		if err := e.svc.RequestPasswordResetSMS(ctx, authz.Anonymous(), "codes@example.com"); err != nil {
			t.Fatalf("request = %v", err)
		}
		e.sendSMS(t) //nolint:contextcheck // the worker runs with its own task context
	}
	for i := range 10 {
		request()
		if len(f.Sent()) != i+1 {
			t.Fatalf("after request %d: %d SMS", i+1, len(f.Sent()))
		}
		if want := btoi(i == 9); e.resetNotices(t) != want {
			t.Fatalf("after request %d: the notice count is not %d", i+1, want)
		}
	}
	for range 2 {
		request()
	}
	if len(f.Sent()) != 10 {
		t.Fatalf("%d SMS over the ceiling", len(f.Sent())-10)
	}
	if tok := e.lastToken(t, "identity.password_reset"); tok == "" {
		t.Fatal("no reset e-mail over the ceiling")
	}
	if n := e.resetNotices(t); n != 1 {
		t.Fatalf("%d notices, want one", n)
	}
	if n := e.smsCount(identity.SMSPasswordReset, identity.SMSOutcomeResetClosed); n != 2 {
		t.Fatalf("reset_closed = %d", n)
	}
	e.clk.Advance(24 * time.Hour)
	request()
	if len(f.Sent()) != 11 {
		t.Fatal("no SMS a day later")
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// TestPasswordResetSMS_DailyTryCeiling: fifteen tries in a rolling day, over
// any codes, close the SMS reset of the account: the right code of an open
// code is refused with the usual answer and audited, requests go by e-mail,
// the holder is told once, sign-in is not touched; a day later it works again.
func TestPasswordResetSMS_DailyTryCeiling(t *testing.T) {
	t.Parallel()
	e, f := smsEnv(t)
	ctx := t.Context()
	e.seededPhone(t, "tries@example.com", testPhone, false)
	newCode := func() string {
		t.Helper()
		e.clk.Advance(time.Hour)
		if err := e.svc.RequestPasswordResetSMS(ctx, authz.Anonymous(), "tries@example.com"); err != nil {
			t.Fatal(err)
		}
		e.sendSMS(t) //nolint:contextcheck // the worker runs with its own task context
		return lastSMSCode(t, f, testPhone)
	}
	confirm := func(code string) error {
		return e.svc.ConfirmPasswordResetSMS(ctx, authz.Anonymous(), identity.PasswordResetSMS{
			Email: "tries@example.com", Code: code, NewPassword: "Yepyeni-Uzun-Parola-2026"})
	}
	wrong := func(code string, n int) {
		t.Helper()
		bad := fmt.Sprintf("%06d", (mustAtoi(t, code)+1)%1_000_000)
		for range n {
			problemtest.WantErr(t, confirm(bad), identity.ErrInvalidSMSCode)
		}
	}
	wrong(newCode(), 5)
	wrong(newCode(), 5)
	wrong(newCode(), 4)
	if n := e.resetNotices(t); n != 0 {
		t.Fatalf("%d notices below the ceiling", n)
	}
	open := newCode()
	wrong(open, 1)
	// 15 tries: this code took one of its five, yet the right value is refused.
	problemtest.WantErr(t, confirm(open), identity.ErrInvalidSMSCode)
	var limited bool
	for _, ev := range e.audit.of(string(authz.IdentityPasswordReset)) {
		limited = limited || (ev.Outcome == audit.Denied && ev.Reason == "sms_reset_limit")
	}
	if !limited {
		t.Fatal("no denied record of the ceiling")
	}
	if n := e.resetNotices(t); n != 1 {
		t.Fatalf("%d notices, want one", n)
	}
	sent := len(f.Sent())
	e.clk.Advance(time.Hour)
	if err := e.svc.RequestPasswordResetSMS(ctx, authz.Anonymous(), "tries@example.com"); err != nil {
		t.Fatal(err)
	}
	e.sendSMS(t)
	if len(f.Sent()) != sent {
		t.Fatal("an SMS over the ceiling of tries")
	}
	if tok := e.lastToken(t, "identity.password_reset"); tok == "" {
		t.Fatal("no reset e-mail")
	}
	if n := e.resetNotices(t); n != 1 {
		t.Fatalf("%d notices, want one", n)
	}
	if res, err := e.svc.CreateSession(ctx, authz.Anonymous(), identity.Login{Email: "tries@example.com", Password: goodPassword}); err != nil || res.MFARequired {
		t.Fatalf("sign-in over the ceiling = %+v, %v", res, err)
	}
	e.clk.Advance(24 * time.Hour)
	if err := confirm(newCode()); err != nil {
		t.Fatalf("a day later = %v", err)
	}
}

// TestSMSCodeCleanup_KeepsTheCeilingWindow: whatever retention sms_code says,
// a code stays for the day the SMS reset ceiling counts it.
func TestSMSCodeCleanup_KeepsTheCeilingWindow(t *testing.T) {
	t.Parallel()
	e, _ := smsEnv(t)
	u := e.activeUser(t, "clean@example.com")
	ctx := t.Context()
	insert := func(age time.Duration) identity.SMSCode {
		t.Helper()
		created := e.clk.Now().Add(-age)
		c := identity.SMSCode{ID: uuid.NewV7(), UserID: u.ID, Purpose: identity.SMSPasswordReset, Number: testPhone, KeyID: "k1",
			CodeHash: make([]byte, 32), ExpiresAt: created.Add(10 * time.Minute), CreatedAt: created}
		if err := e.store.InsertSMSCode(ctx, c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	young, old := insert(2*time.Hour), insert(25*time.Hour)
	short := func(context.Context) (time.Duration, error) { return time.Minute, nil }
	jobs := e.mod.SMSCodeJobs(short)
	if len(jobs) != 1 {
		t.Fatalf("%d jobs", len(jobs))
	}
	if err := jobs[0].Handle(workerCtx(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.SMSCode(ctx, young.ID); err != nil {
		t.Fatalf("a code inside the window went: %v", err)
	}
	if _, err := e.store.SMSCode(ctx, old.ID); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("a code past the window = %v", err)
	}
}

// TestPhoneNotice_ResetLimitText: the notice has its own text in both
// languages, not the generic one.
func TestPhoneNotice_ResetLimitText(t *testing.T) {
	t.Parallel()
	tpl, err := notify.EmbeddedTemplates()
	if err != nil {
		t.Fatal(err)
	}
	data := map[string]any{"name": "Ada", "kind": identity.NoticeSMSResetLimit, "link": "https://app.koro.test/x", "phone_hint": ""}
	for locale, want := range map[string]string{identity.LocaleTR: "SMS ile parola sıfırlamada", identity.LocaleEN: "reset the password of your Koro account by SMS"} {
		r, err := tpl.Render("identity.phone_notice", locale, notify.RenderInput{Data: data})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(r.Text, want) || !strings.Contains(r.HTML, want) {
			t.Fatalf("%s: text %q", locale, r.Text)
		}
	}
}
