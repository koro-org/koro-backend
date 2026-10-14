package billing_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/billing"
	"koro/internal/identity"
	"koro/internal/platform/httpx"
	"koro/internal/platform/obs"
)

// Obviously fake token values (never a provider's format).
const (
	fakeAccessToken  = "test-access-token-not-real"
	fakeRefreshToken = "test-refresh-token-not-real"
)

// newHTTP serves the billing operations of e with the persona resolver.
func newHTTP(t *testing.T, e *env) *httpx.App {
	t.Helper()
	app, err := httpx.New(httpx.Deps{Log: obs.Discard(),
		Actors: authztest.NewResolver(append(authztest.Personas(), authztest.Helpers()...)...)})
	if err != nil {
		t.Fatal(err)
	}
	billing.Register(app.V1, e.svc)
	return app
}

func call(t *testing.T, app *httpx.App, persona, method, path, body string) (int, string) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequestWithContext(t.Context(), method, "/api/v1"+path, r)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if p, _ := authztest.Get(persona); p.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.Token)
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func putBody(proof string) string {
	reauth := ""
	if proof != "" {
		reauth = fmt.Sprintf(`, "reauth": {"password": %q}`, proof)
	}
	return fmt.Sprintf(`{"external_account": "test-company-1", "credentials": {"access_token": %q, "refresh_token": %q,
		"expires_at": "2026-10-14T12:00:00Z"}%s}`, fakeAccessToken, fakeRefreshToken, reauth)
}

// TestPutIntegration_SecretEncrypted_NeverReturned: the tokens are sealed
// with the envelope (bound to their column and provider), stored only
// sealed, and no answer — the PUT, the list, the audit record — carries
// them.
func TestPutIntegration_SecretEncrypted_NeverReturned(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	app := newHTTP(t, e)
	code, body := call(t, app, "finance_admin", http.MethodPut, "/admin/integrations/parasut", putBody(reauthPassword))
	if code != http.StatusOK {
		t.Fatalf("PUT = %d %s", code, body)
	}
	var dto map[string]any
	if err := json.Unmarshal([]byte(body), &dto); err != nil {
		t.Fatal(err)
	}
	if dto["provider"] != "parasut" || dto["external_account"] != "test-company-1" || dto["has_access_token"] != true ||
		dto["has_refresh_token"] != true || dto["rotated_at"] == nil || dto["expires_at"] == nil {
		t.Fatalf("PUT body %v", dto)
	}
	_, list := call(t, app, "platform_admin", http.MethodGet, "/admin/integrations", "")
	for _, s := range []string{body, list} {
		if strings.Contains(s, fakeAccessToken) || strings.Contains(s, fakeRefreshToken) || strings.Contains(s, "_enc") {
			t.Fatalf("a secret leaked: %s", s)
		}
	}
	if !strings.Contains(list, `"parasut"`) {
		t.Fatalf("list = %s", list)
	}

	rows, err := e.store.Credentials(t.Context())
	if err != nil || len(rows) != 1 {
		t.Fatalf("stored rows = %d, %v", len(rows), err)
	}
	c := rows[0]
	for name, sealed := range map[string][]byte{fakeAccessToken: c.AccessTokenEnc, fakeRefreshToken: c.RefreshTokenEnc} {
		if len(sealed) == 0 || bytes.Contains(sealed, []byte(name)) {
			t.Fatalf("token stored in clear or not at all: %q", sealed)
		}
	}
	plain, err := e.sealer.Open(t.Context(), billing.AccessTokenPurposePrefix+"parasut", c.AccessTokenEnc)
	if err != nil || string(plain) != fakeAccessToken {
		t.Fatalf("Open(access) = %q, %v", plain, err)
	}
	if _, err := e.sealer.Open(t.Context(), billing.AccessTokenPurposePrefix+"iyzico", c.AccessTokenEnc); err == nil {
		t.Fatal("a sealed token opened under another provider's purpose")
	}
	plain, err = e.sealer.Open(t.Context(), billing.RefreshTokenPurposePrefix+"parasut", c.RefreshTokenEnc)
	if err != nil || string(plain) != fakeRefreshToken {
		t.Fatalf("Open(refresh) = %q, %v", plain, err)
	}
	ev := e.audit.of(authz.BillingIntegrationManage)
	if len(ev) != 1 || ev[0].Metadata["reauth"] != "password" || ev[0].Metadata["provider"] != "parasut" {
		t.Fatalf("audit = %+v", ev)
	}
	if raw, _ := json.Marshal(ev[0]); bytes.Contains(raw, []byte(fakeAccessToken)) || bytes.Contains(raw, []byte(fakeRefreshToken)) {
		t.Fatalf("a secret in the audit record: %s", raw)
	}

	// Without an envelope nothing is stored in clear: 503.
	noKey := newEnv(t)
	mod, err := billing.New(billingDepsWithoutSealer(t, noKey))
	if err != nil {
		t.Fatal(err)
	}
	fin := actor(t, "finance_admin")
	_, err = mod.Service.AdminPutIntegration(ctxOf(t, fin), fin, "parasut", billing.IntegrationInput{AccessToken: fakeAccessToken},
		identity.ReauthProof{Password: reauthPassword})
	wantStatus(t, err, http.StatusServiceUnavailable)
	if rows, _ := noKey.store.Credentials(t.Context()); len(rows) != 0 {
		t.Fatalf("stored without an envelope: %+v", rows)
	}
}

// TestPutIntegration_RequiresReauth: no proof → 403 identity.reauth_required,
// a wrong one → 403 identity.reauth_failed; nothing is stored; the roles
// (pad·R, fin·R; everyone else 403) hold.
func TestPutIntegration_RequiresReauth(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	app := newHTTP(t, e)
	code, body := call(t, app, "finance_admin", http.MethodPut, "/admin/integrations/parasut", putBody(""))
	if code != http.StatusForbidden || !strings.Contains(body, "identity.reauth_required") {
		t.Fatalf("no proof: %d %s", code, body)
	}
	code, body = call(t, app, "platform_admin", http.MethodPut, "/admin/integrations/parasut", putBody("wrong-password-not-real"))
	if code != http.StatusForbidden || !strings.Contains(body, "identity.reauth_failed") {
		t.Fatalf("wrong proof: %d %s", code, body)
	}
	if rows, _ := e.store.Credentials(t.Context()); len(rows) != 0 {
		t.Fatalf("stored without re-authentication: %+v", rows)
	}
	for _, p := range []string{"club_admin", "support", "dpo", "company_admin"} {
		if code, _ := call(t, app, p, http.MethodPut, "/admin/integrations/parasut", putBody(reauthPassword)); code != http.StatusForbidden {
			t.Fatalf("%s: %d, want 403", p, code)
		}
	}
	if code, _ := call(t, app, "anonymous", http.MethodPut, "/admin/integrations/parasut", putBody(reauthPassword)); code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", code)
	}
	if code, body := call(t, app, "finance_admin", http.MethodPut, "/admin/integrations/stripe", putBody(reauthPassword)); code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown provider: %d %s", code, body)
	}
	code, body = call(t, app, "finance_admin", http.MethodPut, "/admin/integrations/parasut",
		fmt.Sprintf(`{"credentials": {}, "reauth": {"password": %q}}`, reauthPassword))
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("no token: %d %s", code, body)
	}
	if code, body := call(t, app, "platform_admin", http.MethodPut, "/admin/integrations/parasut", putBody(reauthPassword)); code != http.StatusOK {
		t.Fatalf("platform_admin with the proof: %d %s", code, body)
	}
}

// billingDepsWithoutSealer rebuilds e's dependencies without an envelope.
func billingDepsWithoutSealer(t *testing.T, e *env) billing.Deps {
	t.Helper()
	d := e.deps(t)
	d.Sealer = nil
	return d
}
