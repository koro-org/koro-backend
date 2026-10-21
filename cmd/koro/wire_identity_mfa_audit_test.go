package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/httpx"
	"koro/internal/platform/obs"
)

// TestWire_MFACodeDenied_OwnSecurityEventsOverHTTP runs BE-076 through the
// real ActorResolver, identity and audit: a wrong confirmTotp or verifyMfa
// code is 422 identity.invalid_mfa_code and one denied record by the
// signed-in user (reason invalid_mfa_code), so GET /me/security-events
// lists it with the network facts of the user's own request; a wrong
// completeMfaLogin code is the same 401 as an unknown challenge and one
// denied sign-in record without actor (nobody is signed in), the account
// as its resource. Before BE-076 every one of them was written without
// actor and reason, and the owner never saw the signed-in ones.
func TestWire_MFACodeDenied_OwnSecurityEventsOverHTTP(t *testing.T) {
	t.Parallel()
	e := newActorEnv(t)
	u := e.user(t, nil)
	app, err := httpx.New(httpx.Deps{Log: obs.Discard(), Actors: e.c.actors})
	if err != nil {
		t.Fatal(err)
	}
	e.c.registerRoutes(app.V1)
	do := func(method, path, tok, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
		req.Header.Set("X-Koro-Client", "mobile")
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	login := `{"email":"` + u.Email + `","password":"` + matrixReauthPassword + `"}`
	code, body := do(http.MethodPost, "/api/v1/auth/sessions", "", login)
	if code != http.StatusOK || body["token"] == nil {
		t.Fatalf("login = %d %v", code, body)
	}
	tok := body["token"].(string)
	pw := `"reauth":{"password":"` + matrixReauthPassword + `"}`
	code, body = do(http.MethodPost, "/api/v1/me/mfa/totp", tok, "{"+pw+"}")
	if code != http.StatusOK || body["secret_base32"] == nil {
		t.Fatalf("enroll = %d %v", code, body)
	}
	secret := body["secret_base32"].(string)
	gen := func(at time.Time) string {
		c, err := totp.GenerateCodeCustom(secret, at, totp.ValidateOpts{Period: 30, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	wrong := func() string { // a code no step of the accepted window shows
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
	refused := func(what string, code int, body map[string]any) {
		t.Helper()
		if code != http.StatusUnprocessableEntity || body["code"] != "identity.invalid_mfa_code" {
			t.Fatalf("%s with a wrong code = %d %v, want 422 identity.invalid_mfa_code", what, code, body)
		}
	}

	code, body = do(http.MethodPost, "/api/v1/me/mfa/totp/confirm", tok, `{"code":"`+wrong()+`",`+pw+`}`)
	refused("confirmTotp", code, body)
	if code, body = do(http.MethodPost, "/api/v1/me/mfa/totp/confirm", tok, `{"code":"`+gen(e.clk.Now())+`",`+pw+`}`); code != http.StatusOK {
		t.Fatalf("confirm = %d %v", code, body)
	}
	code, body = do(http.MethodPost, "/api/v1/me/mfa/verify", tok, `{"code":"`+wrong()+`"}`)
	refused("verifyMfa", code, body)

	// The owner's security events list both refusals, with the network facts
	// of the owner's own requests.
	code, body = do(http.MethodGet, "/api/v1/me/security-events", tok, "")
	if code != http.StatusOK {
		t.Fatalf("security events = %d %v", code, body)
	}
	items, _ := body["items"].([]any)
	for _, act := range []authz.Action{authz.IdentityMfaConfirm, authz.IdentityMfaVerify} {
		if !slices.ContainsFunc(items, func(x any) bool {
			ev, _ := x.(map[string]any)
			return ev["action"] == string(act) && ev["outcome"] == "denied" && ev["ip_prefix"] != nil && ev["ip_prefix"] != ""
		}) {
			t.Fatalf("no denied %s (with its ip_prefix) among the owner's security events: %v", act, items)
		}
	}

	// Sign-in: a wrong second factor answers exactly like an unknown challenge.
	code, body = do(http.MethodPost, "/api/v1/auth/sessions", "", login)
	if code != http.StatusOK || body["mfa_required"] != true || body["challenge_id"] == nil {
		t.Fatalf("password step = %d %v", code, body)
	}
	challenge := body["challenge_id"].(string)
	answer := func(challengeID string) (int, map[string]any) {
		code, body := do(http.MethodPost, "/api/v1/auth/sessions/mfa", "", `{"challenge_id":"`+challengeID+`","totp_code":"`+wrong()+`"}`)
		return code, map[string]any{"status": code, "type": body["type"], "title": body["title"], "code": body["code"], "detail": body["detail"]}
	}
	gotCode, got := answer(challenge)
	unknownCode, unknown := answer(uuid.NewV4().String())
	if gotCode != http.StatusUnauthorized || got["code"] != "identity.mfa_login_failed" || !reflect.DeepEqual(got, unknown) {
		t.Fatalf("wrong code = %d %v, unknown challenge = %d %v; want the same 401 identity.mfa_login_failed", gotCode, got, unknownCode, unknown)
	}
	rows, err := e.c.audit.store.List(t.Context(), audit.Filter{Action: string(authz.IdentitySessionCreate), Outcome: audit.Denied}, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("%d denied sign-in records, want one (the wrong code; the unknown challenge names no account): %+v", len(rows), rows)
	}
	r := rows[0]
	var meta map[string]any
	if err := json.Unmarshal(r.Metadata, &meta); err != nil {
		t.Fatal(err)
	}
	if r.ActorKind != "anonymous" || r.ActorUserID != nil || r.Reason != "invalid_mfa_code" || r.ResourceType != "user" ||
		r.ResourceID == nil || *r.ResourceID != u.ID || r.RequestID == "" || meta["mfa"] != "totp" || meta["failed_attempts"] != float64(2) {
		t.Fatalf("denied sign-in record %+v (metadata %v), want anonymous, reason invalid_mfa_code, resource user %s, mfa totp, the second failure",
			r, meta, u.ID)
	}
}
