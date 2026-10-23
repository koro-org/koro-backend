package config_test

import (
	"strings"
	"testing"

	"koro/internal/platform/config"
)

type identityOnly struct {
	config.Common
	Identity config.Identity
}

// TestIdentity_AppLinkBaseURL (BE-096): APP_LINK_BASE_URL is the single
// e-mail link origin; the old IDENTITY_LINK_BASE_URL is still read, and both
// set must agree. Dev defaults to the local web client.
func TestIdentity_AppLinkBaseURL(t *testing.T) {
	t.Parallel()
	env := func(appEnv string, kv ...string) map[string]string {
		m := map[string]string{"APP_ENV": appEnv}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	ok := []struct {
		name    string
		environ map[string]string
		want    string
	}{
		{"dev default", env("dev"), "http://localhost:3000"},
		{"new name", env("prod", "APP_LINK_BASE_URL", " https://app.example.invalid/ "), "https://app.example.invalid"},
		{"old name only", env("prod", "IDENTITY_LINK_BASE_URL", "https://app.example.invalid"), "https://app.example.invalid"},
		{"both equal", env("prod", "APP_LINK_BASE_URL", "https://app.example.invalid/",
			"IDENTITY_LINK_BASE_URL", "https://app.example.invalid"), "https://app.example.invalid"},
		{"path prefix", env("staging", "APP_LINK_BASE_URL", "https://example.invalid/koro"), "https://example.invalid/koro"},
		{"localhost in dev", env("dev", "APP_LINK_BASE_URL", "http://127.0.0.1:8081"), "http://127.0.0.1:8081"},
	}
	for _, tt := range ok {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.Load[identityOnly](tt.environ)
			if err != nil || cfg.Identity.LinkBaseURL != tt.want || cfg.Identity.LegacyLinkBaseURL != "" {
				t.Fatalf("LinkBaseURL = %q (legacy %q), %v; want %q", cfg.Identity.LinkBaseURL, cfg.Identity.LegacyLinkBaseURL, err, tt.want)
			}
		})
	}
	bad := []struct {
		name    string
		environ map[string]string
		wantErr string
	}{
		{"both differ", env("prod", "APP_LINK_BASE_URL", "https://a.example.invalid", "IDENTITY_LINK_BASE_URL", "https://b.example.invalid"), "differ"},
		{"prod default is localhost", env("prod"), "must use https"},
		{"http in staging", env("staging", "APP_LINK_BASE_URL", "http://app.example.invalid"), "must use https"},
		{"localhost in prod", env("prod", "APP_LINK_BASE_URL", "https://localhost"), "localhost"},
		{"old name http in prod", env("prod", "IDENTITY_LINK_BASE_URL", "http://app.example.invalid"), "APP_LINK_BASE_URL must use https"},
		{"query", env("dev", "APP_LINK_BASE_URL", "https://app.example.invalid/?a=1"), "without user info"},
		{"empty query", env("dev", "APP_LINK_BASE_URL", "https://app.example.invalid?"), "without user info"},
		{"fragment", env("dev", "APP_LINK_BASE_URL", "https://app.example.invalid/#x"), "without user info"},
		{"user info", env("dev", "APP_LINK_BASE_URL", "https://u:p@app.example.invalid"), "without user info"},
		{"relative", env("dev", "APP_LINK_BASE_URL", "/app"), "absolute"},
		{"custom scheme", env("dev", "APP_LINK_BASE_URL", "koro://open"), "absolute"},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.Load[identityOnly](tt.environ)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), "u:p@") {
				t.Fatal("error message leaks the value")
			}
		})
	}
}
