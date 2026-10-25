package config_test

import (
	"strings"
	"testing"
	"time"

	"koro/internal/platform/config"
)

type consentOnly struct {
	config.Common
	Consent config.Consent
}

// İYS settings (BE-055, BE-105): CONSENT_IYS_PROVIDER picks the adapter
// (none, fake, official, netgsm; the fake only in dev); the official
// adapter's settings go together and only with it; CONSENT_IYS_REQUIRED
// needs an adapter; https outside dev; the password never appears in an
// error.
func TestConsent_IYSValidation(t *testing.T) {
	t.Parallel()
	full := func(kv ...string) map[string]string {
		m := map[string]string{"APP_ENV": "prod", "CONSENT_IYS_PROVIDER": "official", "CONSENT_IYS_BASE_URL": "https://api.iys.org.tr", "CONSENT_IYS_USERNAME": "koro",
			"CONSENT_IYS_PASSWORD": "iys-s3cret-pass", "CONSENT_IYS_CODE": "700123", "CONSENT_IYS_BRAND_CODE": "700124"}
		for i := 0; i+1 < len(kv); i += 2 {
			if kv[i+1] == "<unset>" {
				delete(m, kv[i])
				continue
			}
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	tests := []struct {
		name    string
		environ map[string]string
		wantErr string
	}{
		{"none in dev", map[string]string{"APP_ENV": "dev"}, ""},
		{"full in prod", full("CONSENT_IYS_REQUIRED", "true"), ""},
		{"required without settings", map[string]string{"APP_ENV": "prod", "CONSENT_IYS_REQUIRED": "true"}, "CONSENT_IYS_REQUIRED=true needs"},
		{"settings without the provider", full("CONSENT_IYS_PROVIDER", "<unset>"), "need CONSENT_IYS_PROVIDER=official"},
		{"official without settings", map[string]string{"APP_ENV": "prod", "CONSENT_IYS_PROVIDER": "official"}, "official needs"},
		{"unknown provider", map[string]string{"APP_ENV": "prod", "CONSENT_IYS_PROVIDER": "verimor"}, "CONSENT_IYS_PROVIDER must be"},
		{"fake in dev", map[string]string{"APP_ENV": "dev", "CONSENT_IYS_PROVIDER": "fake", "CONSENT_IYS_REQUIRED": "true"}, ""},
		{"fake in staging", map[string]string{"APP_ENV": "staging", "CONSENT_IYS_PROVIDER": "fake"}, "fake is a development adapter"},
		{"fake in prod", map[string]string{"APP_ENV": "prod", "CONSENT_IYS_PROVIDER": "fake"}, "fake is a development adapter"},
		{"official settings with the fake", full("APP_ENV", "dev", "CONSENT_IYS_PROVIDER", "fake"), "need CONSENT_IYS_PROVIDER=official"},
		// netgsm needs its own settings (TestConsent_NetgsmValidation).
		{"netgsm without its settings", map[string]string{"APP_ENV": "prod", "CONSENT_IYS_PROVIDER": "netgsm", "CONSENT_IYS_REQUIRED": "true"},
			"CONSENT_IYS_PROVIDER=netgsm needs"},
		{"partial", full("CONSENT_IYS_PASSWORD", "<unset>"), "must be set together"},
		{"http in prod", full("CONSENT_IYS_BASE_URL", "http://api.iys.org.tr"), "must use https"},
		{"http in dev", full("APP_ENV", "dev", "CONSENT_IYS_BASE_URL", "http://127.0.0.1:9000"), ""},
		{"recipient type", full("CONSENT_IYS_RECIPIENT_TYPE", "KURUM"), "BIREYSEL or TACIR"},
		{"timeout", full("CONSENT_IYS_TIMEOUT", "2m"), "CONSENT_IYS_TIMEOUT"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.Load[consentOnly](tt.environ)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Load() error = %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("Load() error = %v, want %q", err, tt.wantErr)
			case err != nil && strings.Contains(err.Error(), "iys-s3cret-pass"):
				t.Fatalf("error leaks the password: %v", err)
			case err == nil && strings.Contains(fmtV(cfg), "iys-s3cret-pass"):
				t.Fatal("password printed")
			}
		})
	}
}

// TestConsent_UnsubscribeConfig: the unsubscribe link base is optional
// (empty: no commercial e-mail), https only, without query or fragment
// (the token is appended as a path segment); the link lifetime has a
// default of one year and bounds (BE-062).
func TestConsent_UnsubscribeConfig(t *testing.T) {
	t.Parallel()
	env := func(kv ...string) map[string]string {
		m := map[string]string{"APP_ENV": "prod"}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	cfg, err := config.Load[consentOnly](env())
	if err != nil || cfg.Consent.UnsubscribeBaseURL != "" || cfg.Consent.UnsubscribeTTL != 365*24*time.Hour {
		t.Fatalf("default = %q %v, %v", cfg.Consent.UnsubscribeBaseURL, cfg.Consent.UnsubscribeTTL, err)
	}
	cfg, err = config.Load[consentOnly](env("CONSENT_UNSUBSCRIBE_BASE_URL", " https://api.koro.example.test/api/v1/unsubscribe-links/ ",
		"CONSENT_UNSUBSCRIBE_TTL", "720h"))
	if err != nil || cfg.Consent.UnsubscribeBaseURL != "https://api.koro.example.test/api/v1/unsubscribe-links" ||
		cfg.Consent.UnsubscribeTTL != 720*time.Hour {
		t.Fatalf("set = %q %v, %v", cfg.Consent.UnsubscribeBaseURL, cfg.Consent.UnsubscribeTTL, err)
	}
	for _, v := range []string{
		"http://koro.example.test/u", "/unsubscribe", "https://", "https://koro.example.test/u#x", "https://koro.example.test/u?a=1",
		"https://koro.example.test/u?", "https://user@koro.example.test/u", "https://koro.example.test/" + strings.Repeat("u", 300),
	} {
		if _, err := config.Load[consentOnly](env("CONSENT_UNSUBSCRIBE_BASE_URL", v)); err == nil ||
			!strings.Contains(err.Error(), "CONSENT_UNSUBSCRIBE_BASE_URL") {
			t.Errorf("%q accepted: %v", v, err)
		}
	}
	for _, v := range []string{"23h", "0s", "-1h", "9601h"} {
		if _, err := config.Load[consentOnly](env("CONSENT_UNSUBSCRIBE_TTL", v)); err == nil ||
			!strings.Contains(err.Error(), "CONSENT_UNSUBSCRIBE_TTL") {
			t.Errorf("TTL %s accepted: %v", v, err)
		}
	}
}

// TestConsent_IYSSyncMaxAge (BE-103, BE-105): the İYS send-time hold is off
// by default; when set it lies between 3h (the reconciliation runs hourly
// and İYS's change list hides its last hour) and a week.
func TestConsent_IYSSyncMaxAge(t *testing.T) {
	t.Parallel()
	cfg, err := config.Load[consentOnly](map[string]string{"APP_ENV": "prod"})
	if err != nil || cfg.Consent.IYSSyncMaxAge != 0 {
		t.Fatalf("default = %v, %v", cfg.Consent.IYSSyncMaxAge, err)
	}
	for v, want := range map[string]time.Duration{"0s": 0, "3h": 3 * time.Hour, "24h": 24 * time.Hour, "168h": 168 * time.Hour} {
		cfg, err := config.Load[consentOnly](map[string]string{"APP_ENV": "prod", "CONSENT_IYS_SYNC_MAX_AGE": v})
		if err != nil || cfg.Consent.IYSSyncMaxAge != want {
			t.Errorf("%s = %v, %v", v, cfg.Consent.IYSSyncMaxAge, err)
		}
	}
	for _, v := range []string{"1h", "2h", "179m", "169h", "-2h"} {
		if _, err := config.Load[consentOnly](map[string]string{"APP_ENV": "prod", "CONSENT_IYS_SYNC_MAX_AGE": v}); err == nil ||
			!strings.Contains(err.Error(), "CONSENT_IYS_SYNC_MAX_AGE") {
			t.Errorf("%s accepted: %v", v, err)
		}
	}
}

// TestConsent_NetgsmValidation (BE-110, BE-114): CONSENT_IYS_PROVIDER=netgsm
// needs the account, the İYS codes and the webhook secret, and in prod the
// webhook's source addresses (Q-16; not every address); the Netgsm
// settings need the provider; https in prod; the budgets stay within
// Netgsm's 10 requests per minute; secrets never appear in an error or a
// print.
func TestConsent_NetgsmValidation(t *testing.T) {
	t.Parallel()
	const password, token = "netgsm-s3cret-pass", "netgsm-webhook-secret-token-0123456789abcd"
	full := func(kv ...string) map[string]string {
		m := map[string]string{"APP_ENV": "prod", "CONSENT_IYS_PROVIDER": "netgsm", "CONSENT_IYS_REQUIRED": "true",
			"CONSENT_IYS_NETGSM_USERNAME": "8500000000", "CONSENT_IYS_NETGSM_PASSWORD": password,
			"CONSENT_IYS_NETGSM_IYS_CODE": "700123", "CONSENT_IYS_NETGSM_BRAND_CODE": "700124",
			"CONSENT_IYS_NETGSM_WEBHOOK_TOKEN": token, "CONSENT_IYS_NETGSM_WEBHOOK_ALLOWED_IPS": "192.0.2.0/24"}
		for i := 0; i+1 < len(kv); i += 2 {
			if kv[i+1] == "<unset>" {
				delete(m, kv[i])
				continue
			}
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	cfg, err := config.Load[consentOnly](full("CONSENT_IYS_NETGSM_WEBHOOK_ALLOWED_IPS", "192.0.2.0/24, 198.51.100.7"))
	n := cfg.Consent.Netgsm
	if err != nil || n.BaseURL != "https://api.netgsm.com.tr" || n.RefIDIn != "header" || n.RequestsPerMinute != 8 ||
		n.StatusQueriesPerHour != 240 || n.RecheckPerHour != 120 || n.ResultWait != 15*time.Second || n.ResendAfter != time.Hour ||
		len(n.AllowedPrefixes()) != 2 || n.AllowedPrefixes()[1].Bits() != 32 {
		t.Fatalf("defaults = %+v (%v), %v", n, n.AllowedPrefixes(), err)
	}
	if strings.Contains(fmtV(cfg), password) || strings.Contains(fmtV(cfg), token) {
		t.Fatal("a Netgsm secret is printed")
	}
	for _, tt := range []struct {
		name    string
		environ map[string]string
		wantErr string
	}{
		{"partial", full("CONSENT_IYS_NETGSM_WEBHOOK_TOKEN", "<unset>"), "CONSENT_IYS_PROVIDER=netgsm needs"},
		{"settings without the provider", full("CONSENT_IYS_PROVIDER", "none", "CONSENT_IYS_REQUIRED", "false"), "need CONSENT_IYS_PROVIDER=netgsm"},
		{"official settings with netgsm", full("CONSENT_IYS_BASE_URL", "https://api.iys.org.tr"), "need CONSENT_IYS_PROVIDER=official"},
		{"http in prod", full("CONSENT_IYS_NETGSM_BASE_URL", "http://api.netgsm.com.tr"), "must use https"},
		{"http in dev", full("APP_ENV", "dev", "CONSENT_IYS_NETGSM_BASE_URL", "http://127.0.0.1:9001"), ""},
		{"brand code", full("CONSENT_IYS_NETGSM_BRAND_CODE", "68xxxx"), "must be İYS numbers"},
		{"short token", full("CONSENT_IYS_NETGSM_WEBHOOK_TOKEN", "short"), "CONSENT_IYS_NETGSM_WEBHOOK_TOKEN must be"},
		{"token characters", full("CONSENT_IYS_NETGSM_WEBHOOK_TOKEN", strings.Repeat("a", 40)+"/x"), "CONSENT_IYS_NETGSM_WEBHOOK_TOKEN must be"},
		{"allowed ips", full("CONSENT_IYS_NETGSM_WEBHOOK_ALLOWED_IPS", "192.0.2.0/24,netgsm"), "ALLOWED_IPS"},
		// Q-16 (BE-114): in prod the webhook's source addresses are required.
		{"no allow list in prod", full("CONSENT_IYS_NETGSM_WEBHOOK_ALLOWED_IPS", "<unset>"), "needs CONSENT_IYS_NETGSM_WEBHOOK_ALLOWED_IPS in prod"},
		{"empty allow list in prod", full("CONSENT_IYS_NETGSM_WEBHOOK_ALLOWED_IPS", ""), "needs CONSENT_IYS_NETGSM_WEBHOOK_ALLOWED_IPS in prod"},
		{"every address in prod", full("CONSENT_IYS_NETGSM_WEBHOOK_ALLOWED_IPS", "198.51.100.7,0.0.0.0/0"), "allows every address"},
		{"every v6 address in prod", full("CONSENT_IYS_NETGSM_WEBHOOK_ALLOWED_IPS", "::/0"), "allows every address"},
		{"no allow list in staging", full("APP_ENV", "staging", "CONSENT_IYS_NETGSM_WEBHOOK_ALLOWED_IPS", "<unset>"), ""},
		{"no allow list in dev", full("APP_ENV", "dev", "CONSENT_IYS_NETGSM_WEBHOOK_ALLOWED_IPS", "<unset>"), ""},
		{"provider in upper case", full("CONSENT_IYS_PROVIDER", " Netgsm "), ""},
		{"refid", full("CONSENT_IYS_NETGSM_REFID_IN", "query"), "REFID_IN"},
		{"rate", full("CONSENT_IYS_NETGSM_REQUESTS_PER_MINUTE", "11"), "REQUESTS_PER_MINUTE"},
		{"queries over the rate", full("CONSENT_IYS_NETGSM_REQUESTS_PER_MINUTE", "2", "CONSENT_IYS_NETGSM_STATUS_QUERIES_PER_HOUR", "121"),
			"STATUS_QUERIES_PER_HOUR"},
		{"recheck over the queries", full("CONSENT_IYS_NETGSM_RECHECK_PER_HOUR", "241"), "RECHECK_PER_HOUR"},
		{"recheck off", full("CONSENT_IYS_NETGSM_RECHECK_PER_HOUR", "0"), ""},
		{"result wait", full("CONSENT_IYS_NETGSM_RESULT_WAIT", "2m"), "RESULT_WAIT"},
		{"resend", full("CONSENT_IYS_NETGSM_RESEND_AFTER", "1m"), "RESEND_AFTER"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.Load[consentOnly](tt.environ)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Load() error = %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("Load() error = %v, want %q", err, tt.wantErr)
			case err != nil && (strings.Contains(err.Error(), password) || strings.Contains(err.Error(), token)):
				t.Fatalf("error leaks a secret: %v", err)
			}
		})
	}
}
