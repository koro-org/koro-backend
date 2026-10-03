package config_test

import (
	"strings"
	"testing"

	"koro/internal/platform/config"
)

type consentOnly struct {
	config.Common
	Consent config.Consent
}

// İYS settings (BE-055): all or none; CONSENT_IYS_REQUIRED needs them;
// https outside dev; the password never appears in an error.
func TestConsent_IYSValidation(t *testing.T) {
	t.Parallel()
	full := func(kv ...string) map[string]string {
		m := map[string]string{"APP_ENV": "prod", "CONSENT_IYS_BASE_URL": "https://api.iys.org.tr", "CONSENT_IYS_USERNAME": "koro",
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
