package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/httpx"
	"koro/internal/platform/obs"
)

// signInHTTP serves the container's routes; do sends one request (token
// optional) and decodes the JSON answer.
type signInHTTP struct {
	t   *testing.T
	app *httpx.App
}

func (h signInHTTP) do(method, path, tok, body string) (int, map[string]any) {
	h.t.Helper()
	req := httptest.NewRequestWithContext(h.t.Context(), method, path, strings.NewReader(body))
	req.Header.Set("X-Koro-Client", "mobile")
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.app.Handler().ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func (h signInHTTP) login(email string) string {
	h.t.Helper()
	code, body := h.do(http.MethodPost, "/api/v1/auth/sessions", "", `{"email":"`+email+`","password":"`+matrixReauthPassword+`"}`)
	tok, _ := body["token"].(string)
	if code != http.StatusOK || tok == "" {
		h.t.Fatalf("login = %d %v", code, body)
	}
	return tok
}

// TestWire_SignInAttempts_OwnSecurityEventsOverHTTP runs BE-078 (T-316)
// through the real ActorResolver, identity and audit: wrong passwords, a
// wrong second factor and an attempt on the locked account are 401s
// exactly like an unknown address or challenge, and the owner's GET
// /me/security-events lists each of them — consecutive same-reason
// attempts as one item with attempts and first_occurred_at, the reason
// code, no ip_prefix or user_agent_family although the records hold the
// attempts' network (the owner's own requests keep theirs). Another
// user's list shows none of them. Before BE-078 the owner saw no attempt.
func TestWire_SignInAttempts_OwnSecurityEventsOverHTTP(t *testing.T) {
	t.Parallel()
	e := newActorEnv(t)
	u, v := e.user(t, nil), e.user(t, nil)
	app, err := httpx.New(httpx.Deps{Log: obs.Discard(), Actors: e.c.actors})
	if err != nil {
		t.Fatal(err)
	}
	e.c.registerRoutes(app.V1)
	h := signInHTTP{t: t, app: app}
	tokU, tokV := h.login(u.Email), h.login(v.Email)

	// u turns on MFA (BE-076's route).
	pw := `"reauth":{"password":"` + matrixReauthPassword + `"}`
	code, body := h.do(http.MethodPost, "/api/v1/me/mfa/totp", tokU, "{"+pw+"}")
	secret, _ := body["secret_base32"].(string)
	if code != http.StatusOK || secret == "" {
		t.Fatalf("enroll = %d %v", code, body)
	}
	gen := func(at time.Time) string {
		c, err := totp.GenerateCodeCustom(secret, at, totp.ValidateOpts{Period: 30, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if code, body = h.do(http.MethodPost, "/api/v1/me/mfa/totp/confirm", tokU, `{"code":"`+gen(e.clk.Now())+`",`+pw+`}`); code != http.StatusOK {
		t.Fatalf("confirm = %d %v", code, body)
	}
	wrongCode := func() string {
		var window []string
		for d := -2; d <= 2; d++ {
			window = append(window, gen(e.clk.Now().Add(time.Duration(d)*30*time.Second)))
		}
		for i := range 10 {
			if c := fmt.Sprintf("%06d", i*111111); !slices.Contains(window, c) {
				return c
			}
		}
		t.Fatal("no wrong code")
		return ""
	}

	// The answers say nothing about the account.
	answer := func(code int, body map[string]any) string {
		return fmt.Sprint(code, body["type"], body["title"], body["code"], body["detail"])
	}
	wrongPassword := func(email string) string {
		t.Helper()
		return answer(h.do(http.MethodPost, "/api/v1/auth/sessions", "", `{"email":"`+email+`","password":"Yanlis-Parola-Deneme-1"}`))
	}
	unknown := wrongPassword("nobody-" + u.ID.String() + "@actor.koro.test")
	if !strings.HasPrefix(unknown, "401") || !strings.Contains(unknown, "identity.invalid_credentials") {
		t.Fatalf("unknown address = %s, want 401 identity.invalid_credentials", unknown)
	}
	wrongPasswords := func(n int) {
		t.Helper()
		for range n {
			if got := wrongPassword(u.Email); got != unknown {
				t.Fatalf("wrong password = %s, unknown address = %s; want the same answer", got, unknown)
			}
		}
	}
	wrongPasswords(2)
	code, body = h.do(http.MethodPost, "/api/v1/auth/sessions", "", `{"email":"`+u.Email+`","password":"`+matrixReauthPassword+`"}`)
	challenge, _ := body["challenge_id"].(string)
	if code != http.StatusOK || body["mfa_required"] != true || challenge == "" {
		t.Fatalf("password step = %d %v", code, body)
	}
	code, body = h.do(http.MethodPost, "/api/v1/auth/sessions/mfa", "", `{"challenge_id":"`+challenge+`","totp_code":"`+wrongCode()+`"}`)
	if code != http.StatusUnauthorized || body["code"] != "identity.mfa_login_failed" {
		t.Fatalf("wrong second factor = %d %v", code, body)
	}
	wrongPasswords(2) // the fifth failure locks the account
	wrongPasswords(1) // locked: still the same answer

	rows, err := e.c.audit.store.List(t.Context(), audit.Filter{Action: string(authz.IdentitySessionCreate), Outcome: audit.Denied}, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	var reasons []string
	for _, r := range rows {
		if r.ActorKind != "anonymous" || r.ResourceID == nil || *r.ResourceID != u.ID || r.IP == "" {
			t.Fatalf("attempt record %+v: want anonymous, on u, with its network recorded", r)
		}
		reasons = append(reasons, r.Reason)
	}
	if want := []string{"locked", "invalid_credentials", "invalid_credentials", "invalid_mfa_code", "invalid_credentials", "invalid_credentials"}; !slices.Equal(reasons, want) {
		t.Fatalf("attempt records %q, want %q", reasons, want)
	}

	list := func(tok string) []map[string]any {
		t.Helper()
		code, body := h.do(http.MethodGet, "/api/v1/me/security-events?limit=100", tok, "")
		if code != http.StatusOK || body["next_cursor"] != nil {
			t.Fatalf("security events = %d %v", code, body)
		}
		var out []map[string]any
		for _, x := range body["items"].([]any) {
			out = append(out, x.(map[string]any))
		}
		return out
	}
	type item struct {
		Reason   string
		Attempts float64
	}
	var attempts []item
	ownNetwork := false
	for _, ev := range list(tokU) {
		if ev["attempts"] == nil {
			ownNetwork = ownNetwork || (ev["action"] == string(authz.IdentitySessionCreate) && ev["outcome"] == "success" && ev["ip_prefix"] != nil)
			continue
		}
		if ev["action"] != string(authz.IdentitySessionCreate) || ev["outcome"] != "denied" || ev["ip_prefix"] != nil ||
			ev["user_agent_family"] != nil || ev["first_occurred_at"] == nil {
			t.Fatalf("attempt item %v: want a denied sign-in with first_occurred_at and no network facts", ev)
		}
		attempts = append(attempts, item{ev["reason"].(string), ev["attempts"].(float64)})
	}
	want := []item{{"locked", 1}, {"invalid_credentials", 2}, {"invalid_mfa_code", 1}, {"invalid_credentials", 2}}
	if !reflect.DeepEqual(attempts, want) {
		t.Fatalf("owner's attempt items %+v, want %+v", attempts, want)
	}
	if !ownNetwork {
		t.Fatal("the owner's own sign-in lost its ip_prefix")
	}
	for _, ev := range list(tokV) {
		if ev["attempts"] != nil || ev["outcome"] == "denied" {
			t.Fatalf("another user's security events show %v", ev)
		}
	}
}

// TestWire_ReauthRateLimited_NotAuditedOnlyLoggedAndCounted (T-322,
// BE-078): an R operation over its reauth class (5 per 15 minutes per
// actor) is refused by the operation middleware with 429 before the
// handler — no proof is looked at, so no module writes an audit record —
// and the refusal is a WARN "rate limited" log line (rule, action, the
// actor from the context) and koro_ratelimit_rejected_total{rule,action},
// whose labels never carry the actor.
func TestWire_ReauthRateLimited_NotAuditedOnlyLoggedAndCounted(t *testing.T) {
	t.Parallel()
	e := newActorEnv(t)
	u := e.user(t, nil)
	logs := &lockedBuffer{}
	log := obs.NewLogger(logs, obs.LoggerOptions{Level: slog.LevelInfo})
	m := obs.NewMetrics()
	app, err := httpx.New(httpx.Deps{Log: log, Metrics: m, Actors: e.c.actors})
	if err != nil {
		t.Fatal(err)
	}
	e.c.infra.log = log
	e.c.apiMiddleware(app.API, true)
	e.c.registerRoutes(app.V1)
	h := signInHTTP{t: t, app: app}
	tok := h.login(u.Email)

	enroll := `{"reauth":{"password":"` + matrixReauthPassword + `"}}`
	for i := 1; i <= 5; i++ {
		if code, body := h.do(http.MethodPost, "/api/v1/me/mfa/totp", tok, enroll); code != http.StatusOK {
			t.Fatalf("enroll %d = %d %v", i, code, body)
		}
	}
	before, err := e.c.audit.store.List(t.Context(), audit.Filter{}, nil, 1000)
	if err != nil {
		t.Fatal(err)
	}
	code, body := h.do(http.MethodPost, "/api/v1/me/mfa/totp", tok, enroll)
	if code != http.StatusTooManyRequests || body["code"] != "rate_limited" {
		t.Fatalf("sixth enroll = %d %v, want 429 rate_limited", code, body)
	}
	after, err := e.c.audit.store.List(t.Context(), audit.Filter{}, nil, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("the 429 wrote %d audit records: %+v", len(after)-len(before), after[:len(after)-len(before)])
	}

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, obs.MetricsPath, nil))
	b, _ := io.ReadAll(rec.Body)
	if out := string(b); !strings.Contains(out, `koro_ratelimit_rejected_total{action="identity.mfa.enroll",rule="reauth"} 1`) ||
		strings.Contains(out, u.ID.String()) {
		t.Fatalf("metrics: want koro_ratelimit_rejected_total{reauth, identity.mfa.enroll} 1 and no actor ID:\n%s", out)
	}
	var lines []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
		var r map[string]any
		if json.Unmarshal([]byte(line), &r) == nil && r["msg"] == "rate limited" {
			lines = append(lines, r)
		}
	}
	if len(lines) != 1 || lines[0]["level"] != "WARN" || lines[0]["rule"] != "reauth" ||
		lines[0]["action"] != string(authz.IdentityMfaEnroll) || lines[0]["actor_id"] != u.ID.String() {
		t.Fatalf("rate limited logs %v, want one WARN line with rule reauth, action and actor_id", lines)
	}
}
