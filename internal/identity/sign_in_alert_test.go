package identity_test

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/platform/clock"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/httpx"
	"koro/internal/platform/ratelimit"
)

// User-Agents of the new sign-in alert tests (BE-209).
const (
	uaChrome  = "Mozilla/5.0 (Windows NT 10.0) AppleWebKit/537.36 Chrome/141.0 Safari/537.36"
	uaFirefox = "Mozilla/5.0 (X11; Linux x86_64; rv:140.0) Gecko/20100101 Firefox/140.0"
	uaApp     = "Koro/1.4 (iOS 19)"
)

// origin is where a sign-in comes from: the client kind the body names and
// the request's address and User-Agent.
type origin struct {
	client identity.ClientKind
	ip, ua string
}

var (
	homeChrome = origin{identity.ClientWeb, "203.0.113.7", uaChrome}
	phoneApp   = origin{identity.ClientMobile, "198.51.100.20", uaApp}
)

// signInFrom opens a session of email from o (the password step only).
func (e *env) signInFrom(t *testing.T, email string, o origin) identity.LoginResult {
	t.Helper()
	ctx := httpx.WithUserAgent(httpx.WithClientIP(t.Context(), o.ip), o.ua)
	res, err := e.svc.CreateSession(ctx, authz.Anonymous(), identity.Login{Email: email, Password: goodPassword, Client: o.client})
	if err != nil || res.Session.ID == uuid.Nil() {
		t.Fatalf("sign-in from %+v: %+v, %v", o, res, err)
	}
	return res
}

// newSignIns returns the identity.new_sign_in_detected events committed so far.
func (e *env) newSignIns(t *testing.T) []domainevent.IdentityNewSignInDetected {
	t.Helper()
	var out []domainevent.IdentityNewSignInDetected
	for _, ev := range e.out.Events() {
		if ev.Topic != domainevent.TopicIdentityNewSignInDetected {
			continue
		}
		p, err := domainevent.Decode[domainevent.IdentityNewSignInDetected](ev)
		if err != nil {
			t.Fatal(err)
		}
		if ev.AggregateType != "session" || ev.AggregateID != p.SessionID {
			t.Fatalf("event aggregate %s %s, want the session %s", ev.AggregateType, ev.AggregateID, p.SessionID)
		}
		out = append(out, p)
	}
	return out
}

// alerted reports whether the sign-in res raised the alert: its event and
// the metadata of its audit record.
func (e *env) alerted(t *testing.T, res identity.LoginResult) bool {
	t.Helper()
	event := false
	for _, p := range e.newSignIns(t) {
		if p.SessionID == res.Session.ID {
			if event || p.UserID != res.Session.UserID {
				t.Fatalf("alert of %s: %+v (twice, or of another user)", res.Session.ID, p)
			}
			event = true
		}
	}
	for _, ev := range e.audit.of(string(authz.IdentitySessionCreate)) {
		if ev.Outcome == audit.Success && ev.ResourceID != nil && *ev.ResourceID == res.Session.ID {
			if marked := ev.Metadata["new_sign_in"] == true; marked != event {
				t.Fatalf("audit metadata new_sign_in = %v, event %v", ev.Metadata["new_sign_in"], event)
			}
			return event
		}
	}
	t.Fatalf("no audit record of the session %s", res.Session.ID)
	return false
}

// TestNewSignIn_FirstSignInRaisesNothing: the account's first sign-in (the
// one after the registration) has nothing to be compared with and raises
// no alert, nor spends the account's daily allowance.
func TestNewSignIn_FirstSignInRaisesNothing(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.activeUser(t, "first@example.com")
	if e.alerted(t, e.signInFrom(t, "first@example.com", homeChrome)) {
		t.Fatal("the first sign-in alerted")
	}
	for _, c := range e.lim.Calls() {
		if strings.HasPrefix(c, "new_sign_in_alert|") {
			t.Fatalf("the first sign-in asked the alert limit: %v", e.lim.Calls())
		}
	}
}

// TestNewSignIn_UnrecognizedDeviceOrNetworkAlerts: a sign-in is recognized
// when an earlier session had the same device (client kind + browser or
// app family) and one had the same network (/24, /48); a new family, a new
// client kind or a new network alerts. The parts may come from different
// sessions: the holder's laptop on the network the phone used is no news.
func TestNewSignIn_UnrecognizedDeviceOrNetworkAlerts(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		from origin
		want bool
	}{
		{"same device and network", homeChrome, false},
		{"another address of the same /24", origin{identity.ClientWeb, "203.0.113.200", uaChrome}, false},
		{"another version of the same browser", origin{identity.ClientWeb, "203.0.113.7", strings.Replace(uaChrome, "141.0", "142.0", 1)}, false},
		{"device of one session, network of another", origin{identity.ClientWeb, phoneApp.ip, uaChrome}, false},
		{"the same /48 of IPv6", origin{identity.ClientWeb, "2001:db8:7:9::1", uaChrome}, false},
		{"new browser family", origin{identity.ClientWeb, "203.0.113.7", uaFirefox}, true},
		{"the app's User-Agent on the web client", origin{identity.ClientWeb, "203.0.113.7", uaApp}, true},
		{"new network", origin{identity.ClientWeb, "192.0.2.44", uaChrome}, true},
		{"new IPv6 network", origin{identity.ClientWeb, "2001:db8:8::1", uaChrome}, true},
		{"the holder's app from a stranger's network", origin{identity.ClientMobile, "192.0.2.44", uaApp}, true},
		{"no User-Agent", origin{identity.ClientWeb, "203.0.113.7", ""}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			e.activeUser(t, "known@example.com")
			e.signInFrom(t, "known@example.com", homeChrome)
			e.signInFrom(t, "known@example.com", phoneApp)                                                // a new device: alerts
			e.signInFrom(t, "known@example.com", origin{identity.ClientWeb, "2001:db8:7:1::5", uaChrome}) // a new network
			before := len(e.newSignIns(t))
			if got := e.alerted(t, e.signInFrom(t, "known@example.com", tt.from)); got != tt.want {
				t.Fatalf("alerted = %v, want %v", got, tt.want)
			}
			want := 0
			if tt.want {
				want = 1
			}
			if n := len(e.newSignIns(t)) - before; n != want {
				t.Fatalf("%d new events, want %d", n, want)
			}
		})
	}
}

// TestNewSignIn_KnownOnceStaysKnown: the same device family on the same
// network alerts once; it stays known as long as its session rows are kept
// (30 days after they end), revoked ones included, and an account whose
// rows are all gone alerts again (a dormant account).
func TestNewSignIn_KnownOnceStaysKnown(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	u := e.activeUser(t, "repeat@example.com")
	e.signInFrom(t, "repeat@example.com", homeChrome)
	laptop := origin{identity.ClientWeb, "192.0.2.10", uaFirefox}
	first := e.signInFrom(t, "repeat@example.com", laptop)
	if !e.alerted(t, first) {
		t.Fatal("a new device did not alert")
	}
	// The holder logs out: the revoked row still counts.
	if err := e.svc.DeleteCurrentSession(t.Context(), authz.Actor{Kind: authz.KindUser, UserID: u.ID, SessionID: first.Session.ID}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		e.clk.Advance(time.Hour)
		if e.alerted(t, e.signInFrom(t, "repeat@example.com", laptop)) {
			t.Fatal("a known device alerted again")
		}
	}
	// Months later the rows are gone: nothing recognizes the device.
	e.clk.Advance(120 * 24 * time.Hour)
	if _, err := e.store.DeleteEndedSessions(t.Context(), e.clk.Now(), 1000); err != nil {
		t.Fatal(err)
	}
	if !e.alerted(t, e.signInFrom(t, "repeat@example.com", laptop)) {
		t.Fatal("a sign-in to a dormant account did not alert")
	}
}

// TestNewSignIn_AlertsPerDayAreCapped: an account gets at most three
// alerts in a rolling day; the fourth new network that day is held back,
// the next day alerts again. A limiter that cannot answer alerts.
func TestNewSignIn_AlertsPerDayAreCapped(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.activeUser(t, "cap@example.com")
	e.signInFrom(t, "cap@example.com", homeChrome)
	var got []bool
	for i := range 4 { // four new networks in a day
		got = append(got, e.alerted(t, e.signInFrom(t, "cap@example.com", origin{identity.ClientWeb, "10." + strconv.Itoa(i) + ".0.1", uaChrome})))
	}
	if want := []bool{true, true, true, false}; !slices.Equal(got, want) {
		t.Fatalf("alerts %v, want %v", got, want)
	}
	e.clk.Advance(24 * time.Hour)
	if !e.alerted(t, e.signInFrom(t, "cap@example.com", origin{identity.ClientWeb, "198.51.100.200", uaFirefox})) {
		t.Fatal("no alert the next day")
	}

	down := newEnv(t)
	down.activeUser(t, "down@example.com")
	down.signInFrom(t, "down@example.com", homeChrome)
	down.lim.Fail(ratelimit.ErrUnavailable)
	if !down.alerted(t, down.signInFrom(t, "down@example.com", phoneApp)) {
		t.Fatal("an unavailable limiter held the alert back")
	}
}

// TestNewSignIn_SecondStepAlertsOnce: an account with two-step
// verification alerts when the session opens, once, never at the password
// step (nor when the second step fails).
func TestNewSignIn_SecondStepAlertsOnce(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	_, _, secret, _ := e.mfaUser(t, "twostep@example.com")
	stranger := origin{identity.ClientWeb, "192.0.2.99", uaFirefox}
	ctx := httpx.WithUserAgent(httpx.WithClientIP(t.Context(), stranger.ip), stranger.ua)
	step := func() identity.MFAChallenge {
		res, err := e.svc.CreateSession(ctx, authz.Anonymous(), identity.Login{Email: "twostep@example.com", Password: goodPassword, Client: stranger.client})
		if err != nil || !res.MFARequired {
			t.Fatalf("password step = %+v, %v", res, err)
		}
		return res.Challenge
	}
	ch := step()
	if _, err := e.svc.CompleteMFALogin(ctx, authz.Anonymous(), identity.MFALogin{ChallengeID: ch.ID, TOTPCode: "000000"}); err == nil {
		t.Fatal("a wrong code opened a session")
	}
	if n := len(e.newSignIns(t)); n != 0 {
		t.Fatalf("%d alerts before any session opened", n)
	}
	ch = step()
	res, err := e.svc.CompleteMFALogin(ctx, authz.Anonymous(), identity.MFALogin{ChallengeID: ch.ID, TOTPCode: e.nextCode(t, secret)})
	if err != nil {
		t.Fatal(err)
	}
	if !e.alerted(t, res) || len(e.newSignIns(t)) != 1 {
		t.Fatalf("alerts %+v, want one for %s", e.newSignIns(t), res.Session.ID)
	}
}

// TestNewSignIn_AlertGoesByEmailAppAndPush: the worker sends the in-app
// notification and the push (fixed texts, no data, the security settings
// link) and the e-mail, which names the device family, the client kind and
// the time, never the address or the network; the dedupe keys are the
// session's, so a redelivered event sends nothing twice.
func TestNewSignIn_AlertGoesByEmailAppAndPush(t *testing.T) {
	t.Parallel()
	tpl, err := notify.EmbeddedTemplates()
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t)
	u := e.activeUser(t, "alert-mail@example.com")
	e.signInFrom(t, "alert-mail@example.com", homeChrome)
	e.deliver(t)
	before := len(e.mail.sent())
	res := e.signInFrom(t, "alert-mail@example.com", origin{identity.ClientWeb, "192.0.2.55", uaFirefox})
	e.deliver(t)
	msgs := e.mail.sent()[before:]
	if len(msgs) != 3 {
		t.Fatalf("sent %d messages, want in_app, push and e-mail: %+v", len(msgs), msgs)
	}
	key := "identity.new_sign_in:" + res.Session.ID.String()
	for i, ch := range []notify.Channel{notify.ChannelInApp, notify.ChannelPush} {
		m := msgs[i]
		switch {
		case m.Channel != ch || m.Template != "identity.security_new_sign_in" || m.Category != notify.CategoryTransactional:
			t.Fatalf("message %d = %s %s %s", i, m.Channel, m.Template, m.Category)
		case m.UserID == nil || *m.UserID != u.ID || m.LinkPath.String() != "/me/security":
			t.Fatalf("%s: recipient %v link %q", ch, m.UserID, m.LinkPath)
		case m.ToEmail != "" || len(m.Data) != 0 || m.DedupeKey != key+":"+string(ch):
			t.Fatalf("%s: to %q data %v key %q", ch, m.ToEmail, m.Data, m.DedupeKey)
		}
	}
	m := msgs[2]
	at, _ := m.Data["at"].(time.Time)
	if m.Channel != notify.ChannelEmail || m.ToEmail != u.Email || m.Template != "identity.security_new_sign_in" || m.DedupeKey != key ||
		m.Data["device"] != "Firefox" || m.Data["client"] != "web" || !at.Equal(res.Session.CreatedAt) ||
		m.Data["link"] != "https://app.koro.test/me/security" || m.Data["reset_link"] != "https://app.koro.test/auth/reset-password" {
		t.Fatalf("e-mail = %+v", m)
	}
	for k, v := range m.Data {
		if s, ok := v.(string); ok && (strings.Contains(s, "192.0.2") || strings.Contains(s, "Mozilla")) {
			t.Fatalf("e-mail data %s carries the network or the User-Agent: %q", k, s)
		}
	}
	meta, ok := tpl.Meta("identity.security_new_sign_in")
	if !ok || meta.Preference != notify.PrefSecurity || meta.Priority != notify.PriorityHigh || !meta.Supports(notify.ChannelEmail) ||
		!meta.Supports(notify.ChannelInApp) || !meta.Supports(notify.ChannelPush) {
		t.Fatalf("template meta = %+v", meta)
	}
	r, err := tpl.Render(m.Template, "tr", notify.RenderInput{Data: m.Data})
	if err != nil {
		t.Fatal(err)
	}
	when := res.Session.CreatedAt.In(clock.Istanbul()).Format("02.01.2006 15:04")
	for _, want := range []string{"Cihaz: Firefox (web)", "Zaman: " + when, "https://app.koro.test/me/security", "https://app.koro.test/auth/reset-password"} {
		if !strings.Contains(r.Text, want) || !strings.Contains(r.HTML, want) {
			t.Fatalf("e-mail lacks %q:\n%s", want, r.Text)
		}
	}
	if strings.Contains(r.Text+r.HTML, "192.0.2") {
		t.Fatal("the e-mail shows the network")
	}

	// The app's sign-in reads as the app in Turkish.
	appMail := m
	appMail.Data = map[string]any{"name": "Ada", "device": "Koro app", "client": "mobile", "at": res.Session.CreatedAt}
	if r, err := tpl.Render(appMail.Template, "tr", notify.RenderInput{Data: appMail.Data}); err != nil || !strings.Contains(r.Text, "Cihaz: Koro uygulaması (mobil)") {
		t.Fatalf("app e-mail = %q, %v", r.Text, err)
	}
}

// failingAudit refuses the record of a sign-in once armed.
type failingAudit struct {
	next  identity.AuditLogger
	armed atomic.Bool
}

var errAuditDown = errors.New("audit down")

func (f *failingAudit) Log(ctx context.Context, e audit.Event) error {
	if f.armed.Load() && e.Action == string(authz.IdentitySessionCreate) && e.Outcome == audit.Success {
		return errAuditDown
	}
	return f.next.Log(ctx, e)
}

// TestNewSignIn_AttackerCannotSuppressTheAlert (takeover): the alert is
// written in the sign-in's transaction — it commits with the session or
// not at all — and nothing the new session does afterwards recalls it:
// logging out, closing the holder's other sessions, changing the password.
// The worker still sends all three messages.
func TestNewSignIn_AttackerCannotSuppressTheAlert(t *testing.T) {
	t.Parallel()
	fa := &failingAudit{}
	e := newEnv(t, func(d *identity.Deps) { fa.next, d.Audit = d.Audit, fa })
	u := e.activeUser(t, "victim@example.com")
	holder := e.signInFrom(t, "victim@example.com", homeChrome)
	e.deliver(t)
	attacker := origin{identity.ClientMobile, "192.0.2.66", uaApp}

	// The sign-in fails after the event was written: no token, no alert.
	// (The in-memory store keeps that attempt's session row, PostgreSQL rolls
	// it back with the event: sign_in_alert_integration_test.go. The next
	// attempt comes from elsewhere so the kept row does not recognize it.)
	fa.armed.Store(true)
	failed := origin{identity.ClientMobile, "100.64.0.77", uaApp}
	ctx := httpx.WithUserAgent(httpx.WithClientIP(t.Context(), failed.ip), failed.ua)
	_, err := e.svc.CreateSession(ctx, authz.Anonymous(), identity.Login{Email: "victim@example.com", Password: goodPassword, Client: failed.client})
	if !errors.Is(err, errAuditDown) {
		t.Fatalf("CreateSession = %v, want the audit failure", err)
	}
	if n := len(e.newSignIns(t)); n != 0 {
		t.Fatalf("%d alerts of a sign-in that did not commit", n)
	}
	fa.armed.Store(false)

	res := e.signInFrom(t, "victim@example.com", attacker)
	if !e.alerted(t, res) {
		t.Fatal("the takeover sign-in did not alert")
	}
	a := authz.Actor{Kind: authz.KindUser, UserID: u.ID, SessionID: res.Session.ID}
	if err := e.svc.RevokeMySession(t.Context(), a, holder.Session.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.ChangePassword(t.Context(), a, identity.PasswordChange{CurrentPassword: goodPassword, NewPassword: "Baska-Bir-Uzun-Parola-4417"}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.DeleteCurrentSession(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	before := len(e.mail.sent())
	e.deliver(t)
	var got []notify.Channel
	for _, m := range e.mail.sent()[before:] {
		if m.Template == "identity.security_new_sign_in" {
			got = append(got, m.Channel)
		}
	}
	if !slices.Equal(got, []notify.Channel{notify.ChannelInApp, notify.ChannelPush, notify.ChannelEmail}) {
		t.Fatalf("alert sent on %v, want in_app, push and e-mail", got)
	}
}
