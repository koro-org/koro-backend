package httpx_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/obs"
	"koro/internal/platform/problem"
)

// fakeResolver accepts one token per transport.
type fakeResolver struct {
	mu    sync.Mutex
	calls []httpx.Credential
	fail  error
}

func (f *fakeResolver) Resolve(ctx context.Context, cred httpx.Credential) (context.Context, error) {
	f.mu.Lock()
	f.calls = append(f.calls, cred)
	f.mu.Unlock()
	if f.fail != nil {
		return nil, f.fail
	}
	switch {
	case cred.Transport == httpx.TransportBearer && cred.TokenHash == sha256.Sum256([]byte("mobile-token")):
		return obs.WithActorID(ctx, "user-mobile"), nil
	case cred.Transport == httpx.TransportCookie && cred.TokenHash == sha256.Sum256([]byte("web-token")):
		return obs.WithActorID(ctx, "user-web"), nil
	}
	return nil, problem.ErrUnauthenticated
}

type whoOutput struct {
	Body struct {
		Actor string `json:"actor"`
	}
}

func newAuthApp(t *testing.T, r httpx.ActorResolver) *testApp {
	t.Helper()
	app := newTestApp(t, func(d *httpx.Deps) { d.Actors = r })
	who := func(ctx context.Context, _ *emptyInput) (*whoOutput, error) {
		o := &whoOutput{}
		o.Body.Actor = obs.ActorID(ctx)
		return o, nil
	}
	huma.Register(app.V1, op.Koro(huma.Operation{OperationID: "getTestMe", Method: http.MethodGet, Path: "/me", Tags: []string{"testonly"}},
		op.Meta{Action: "testonly.me.read", Surface: op.SurfaceSelf}), who)
	huma.Register(app.V1, op.Koro(huma.Operation{OperationID: "getTestPublic", Method: http.MethodGet, Path: "/public", Tags: []string{"testonly"}},
		op.Meta{Action: "testonly.page.read", Surface: op.SurfacePublic}), who)
	return app
}

func TestAuth_ProtectedOperation(t *testing.T) {
	t.Parallel()
	app := newAuthApp(t, &fakeResolver{})
	tests := []struct {
		name      string
		hdr       map[string]string
		wantCode  int
		wantActor string
	}{
		{"no credential", nil, http.StatusUnauthorized, ""},
		{"bearer", map[string]string{"Authorization": "Bearer mobile-token"}, http.StatusOK, "user-mobile"},
		{"cookie", map[string]string{"Cookie": httpx.SessionCookieName + "=web-token"}, http.StatusOK, "user-web"},
		{"cookie token used as bearer", map[string]string{"Authorization": "Bearer web-token"}, http.StatusUnauthorized, ""},
		{"bearer wins over cookie", map[string]string{"Authorization": "Bearer nope", "Cookie": httpx.SessionCookieName + "=web-token"}, http.StatusUnauthorized, ""},
		{"malformed authorization", map[string]string{"Authorization": "Basic dXNlcjpwYXNz"}, http.StatusUnauthorized, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := app.do(t, http.MethodGet, "/api/v1/me", nil, tt.hdr)
			if tt.wantCode == http.StatusUnauthorized {
				m := problemBody(t, rec, http.StatusUnauthorized)
				if m["code"] != "unauthenticated" {
					t.Fatalf("code = %v", m["code"])
				}
				return
			}
			if rec.Code != tt.wantCode || !strings.Contains(rec.Body.String(), tt.wantActor) {
				t.Fatalf("status = %d body = %s, want %d actor %s", rec.Code, rec.Body.String(), tt.wantCode, tt.wantActor)
			}
		})
	}
}

func TestAuth_PublicOperation_StaleCredentialIsAnonymous(t *testing.T) {
	t.Parallel()
	app := newAuthApp(t, &fakeResolver{})
	rec := app.do(t, http.MethodGet, "/api/v1/public", nil, map[string]string{"Cookie": httpx.SessionCookieName + "=expired"})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"actor":""`) {
		t.Fatalf("stale cookie on public op: %d %s", rec.Code, rec.Body.String())
	}
	rec = app.do(t, http.MethodGet, "/api/v1/public", nil, map[string]string{"Authorization": "Bearer mobile-token"})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "user-mobile") {
		t.Fatalf("valid bearer on public op: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAuth_ResolverFailure_Returns500(t *testing.T) {
	t.Parallel()
	app := newAuthApp(t, &fakeResolver{fail: errors.New("sessions: connection refused 10.0.0.7")})
	rec := app.do(t, http.MethodGet, "/api/v1/me", nil, map[string]string{"Authorization": "Bearer mobile-token"})
	m := problemBody(t, rec, http.StatusInternalServerError)
	if m["code"] != "internal_error" || strings.Contains(rec.Body.String(), "10.0.0.7") {
		t.Fatalf("resolver failure response: %s", rec.Body.String())
	}
}

func TestAuth_NoResolver_ProtectedIs401(t *testing.T) {
	t.Parallel()
	app := newAuthApp(t, nil)
	problemBody(t, app.do(t, http.MethodGet, "/api/v1/me", nil, map[string]string{"Authorization": "Bearer mobile-token"}), http.StatusUnauthorized)
	if rec := app.do(t, http.MethodGet, "/api/v1/public", nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("public op without resolver = %d", rec.Code)
	}
}
