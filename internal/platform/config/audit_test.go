package config_test

import (
	"strings"
	"testing"

	"koro/internal/platform/config"
)

type auditOnly struct {
	config.Common
	Audit config.Audit
}

// devAuditKey is the .env.example placeholder (32 low-diversity bytes).
const devAuditKey = "dev1:YXVkaXRhdWRpdGF1ZGl0YXVkaXRhdWRpdGF1ZGl0YXU="

// realAuditKey is a random 32-byte seed (test only).
const realAuditKey = "k1:N8K/UY+pn3A/AYjHBG910h167H/WctWV5KJpjwqWrKU="

func TestAudit_Validation(t *testing.T) {
	t.Parallel()
	env := func(kv ...string) map[string]string {
		m := map[string]string{"APP_ENV": "prod", "AUDIT_ROOT_SIGNING_KEYS": realAuditKey, "AUDIT_ROOT_ARCHIVE_DIR": "/var/lib/koro/audit"}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		for k, v := range m {
			if v == "<unset>" {
				delete(m, k)
			}
		}
		return m
	}
	tests := []struct {
		name    string
		environ map[string]string
		wantErr string // "" = valid
	}{
		{"prod with key and archive", env(), ""},
		{"prod rejects a placeholder among the rotation keys", env("AUDIT_ROOT_SIGNING_KEYS", realAuditKey+","+devAuditKey[:4]+"x"+devAuditKey[4:]), "looks like a placeholder"},
		{"dev may leave both empty", env("APP_ENV", "dev", "AUDIT_ROOT_SIGNING_KEYS", "<unset>", "AUDIT_ROOT_ARCHIVE_DIR", "<unset>"), ""},
		{"dev placeholder key", env("APP_ENV", "dev", "AUDIT_ROOT_SIGNING_KEYS", devAuditKey), ""},
		{"prod requires the key", env("AUDIT_ROOT_SIGNING_KEYS", "<unset>"), "AUDIT_ROOT_SIGNING_KEYS is required in prod"},
		{"staging requires the archive", env("APP_ENV", "staging", "AUDIT_ROOT_ARCHIVE_DIR", "<unset>"), "AUDIT_ROOT_ARCHIVE_DIR is required in staging"},
		{"prod rejects the placeholder", env("AUDIT_ROOT_SIGNING_KEYS", devAuditKey), "looks like a placeholder"},
		{"seed must be 32 bytes", env("AUDIT_ROOT_SIGNING_KEYS", "k1:c2hvcnQ="), "32-byte Ed25519 seed"},
		{"keyring format", env("AUDIT_ROOT_SIGNING_KEYS", "N8K/UY+pn3A/AYjHBG910h167H/WctWV5KJpjwqWrKU="), "AUDIT_ROOT_SIGNING_KEYS"},
		{"relative archive dir", env("AUDIT_ROOT_ARCHIVE_DIR", "audit-roots"), "must be an absolute path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.Load[auditOnly](tt.environ)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Load() error = %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("Load() error = %v, want %q", err, tt.wantErr)
			case err != nil && strings.Contains(err.Error(), "N8K/UY"):
				t.Fatalf("error leaks the key: %v", err)
			}
			if err == nil && tt.environ["AUDIT_ROOT_SIGNING_KEYS"] != "" {
				keys, kerr := cfg.Audit.RootKeys()
				if kerr != nil || len(keys) == 0 {
					t.Fatalf("RootKeys() = %v, %v", keys, kerr)
				}
			}
		})
	}
}
