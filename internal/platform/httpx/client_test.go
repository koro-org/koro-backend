package httpx_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"koro/internal/platform/httpx"
)

// The audit log reads the client IP and the User-Agent from the request
// context (01 §4.1; BE-007).
func TestHTTP_ClientIPAndUserAgentInContext(t *testing.T) {
	t.Parallel()
	app := newTestApp(t, nil)
	var ip, ua string
	registerGET(app.V1, "getTestClient", "/client", func(ctx context.Context, _ *emptyInput) (*okOutput, error) {
		ip, ua = httpx.ClientIPFrom(ctx), httpx.UserAgentFrom(ctx)
		return ok(), nil
	})

	if rec := app.do(t, http.MethodGet, "/api/v1/client", nil, map[string]string{"User-Agent": "Koro/1.0 (iOS 19)"}); rec.Code != http.StatusOK {
		t.Fatalf("status = %d; %s", rec.Code, rec.Body.String())
	}
	if ip != "192.0.2.1" || ua != "Koro/1.0 (iOS 19)" { // httptest's RemoteAddr
		t.Fatalf("ctx client = (%q, %q)", ip, ua)
	}

	long := strings.Repeat("ğ", httpx.MaxUserAgent+50)
	app.do(t, http.MethodGet, "/api/v1/client", nil, map[string]string{"User-Agent": long})
	if n := utf8.RuneCountInString(ua); n != httpx.MaxUserAgent || !utf8.ValidString(ua) {
		t.Fatalf("long User-Agent kept %d runes (valid UTF-8: %v), want %d", n, utf8.ValidString(ua), httpx.MaxUserAgent)
	}
}
