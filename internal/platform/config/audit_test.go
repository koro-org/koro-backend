package config_test

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"koro/internal/platform/config"
)

type auditOnly struct {
	config.Common
	Audit config.Audit
}

type auditRootOnly struct {
	config.Common
	AuditRoot config.AuditRoot
}

// devAuditKey is the .env.example placeholder (32 low-diversity bytes).
const devAuditKey = "dev1:YXVkaXRhdWRpdGF1ZGl0YXVkaXRhdWRpdGF1ZGl0YXU="

// realAuditKey is a random 32-byte seed (test only).
const realAuditKey = "k1:N8K/UY+pn3A/AYjHBG910h167H/WctWV5KJpjwqWrKU="

func TestAuditRoot_Validation(t *testing.T) {
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
			cfg, err := config.Load[auditRootOnly](tt.environ)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Load() error = %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("Load() error = %v, want %q", err, tt.wantErr)
			case err != nil && strings.Contains(err.Error(), "N8K/UY"):
				t.Fatalf("error leaks the key: %v", err)
			}
			if err == nil && tt.environ["AUDIT_ROOT_SIGNING_KEYS"] != "" {
				keys, kerr := cfg.AuditRoot.RootKeys()
				if kerr != nil || len(keys) == 0 {
					t.Fatalf("RootKeys() = %v, %v", keys, kerr)
				}
			}
		})
	}
}

// devAuditVerifyKey is the public key of devAuditKey (.env.example).
const devAuditVerifyKey = "dev1:eARyIQn7PbU7ZruoCTHgf7ojtVhotKNa+pBr0/m6gEI="

func TestAudit_VerifyKeysValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		environ map[string]string
		wantErr string // "" = valid
	}{
		{"prod with public keys", map[string]string{"APP_ENV": "prod", "AUDIT_ROOT_VERIFY_KEYS": "k1:EVPb5bmkfsWR+pUFeIehhFLGBA/ISQHuDQnmW2oC5Zs=," + devAuditVerifyKey}, ""},
		{"prod requires public keys", map[string]string{"APP_ENV": "prod"}, "AUDIT_ROOT_VERIFY_KEYS is required in prod"},
		{"staging requires public keys", map[string]string{"APP_ENV": "staging"}, "AUDIT_ROOT_VERIFY_KEYS is required in staging"},
		{"dev may leave it empty", map[string]string{"APP_ENV": "dev"}, ""},
		{"key must be 32 bytes", map[string]string{"APP_ENV": "dev", "AUDIT_ROOT_VERIFY_KEYS": "k1:c2hvcnQ="}, "32-byte Ed25519 public key"},
		{"keyring format", map[string]string{"APP_ENV": "dev", "AUDIT_ROOT_VERIFY_KEYS": "eARyIQn7PbU7ZruoCTHgf7ojtVhotKNa+pBr0/m6gEI="}, "AUDIT_ROOT_VERIFY_KEYS"},
		// The seeds are not this variable: the API needs none (BE-107).
		{"prod does not ask for the seeds", map[string]string{"APP_ENV": "prod", "AUDIT_ROOT_VERIFY_KEYS": devAuditVerifyKey}, ""},
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
			}
			if err == nil && tt.environ["AUDIT_ROOT_VERIFY_KEYS"] != "" {
				if keys, kerr := cfg.Audit.VerifyKeys(); kerr != nil || len(keys) == 0 {
					t.Fatalf("VerifyKeys() = %v, %v", keys, kerr)
				}
			}
		})
	}
}

// Least privilege (BE-107): only `koro worker` declares the daily root seeds
// and archive. No other subcommand has an AuditRoot section or a variable
// of it anywhere in its tree, so their *_FILE secret is never even read.
func TestAuditRoot_OnlyWorkerDeclaresIt(t *testing.T) {
	t.Parallel()
	declares := func(root reflect.Type) bool {
		var walk func(t reflect.Type) bool
		walk = func(t reflect.Type) bool {
			for i := range t.NumField() {
				f := t.Field(i)
				name, _, _ := strings.Cut(f.Tag.Get("env"), ",")
				if f.Type == reflect.TypeFor[config.AuditRoot]() || strings.HasPrefix(name, "AUDIT_ROOT_SIGNING") || name == "AUDIT_ROOT_ARCHIVE_DIR" {
					return true
				}
				if f.Type.Kind() == reflect.Struct && walk(f.Type) {
					return true
				}
			}
			return false
		}
		return walk(root)
	}
	for _, tc := range []struct {
		name string
		typ  reflect.Type
		want bool
	}{
		{"api", reflect.TypeFor[config.API](), false},
		{"seed", reflect.TypeFor[config.Seed](), false},
		{"migrate", reflect.TypeFor[config.Migrate](), false},
		{"healthcheck", reflect.TypeFor[config.Healthcheck](), false},
		{"worker", reflect.TypeFor[config.Worker](), true},
	} {
		if got := declares(tc.typ); got != tc.want {
			t.Errorf("%s declares AuditRoot = %v, want %v", tc.name, got, tc.want)
		}
	}
	for _, v := range config.AuditRootVars {
		if !strings.HasPrefix(v, "AUDIT_ROOT_SIGNING_KEYS") && v != "AUDIT_ROOT_ARCHIVE_DIR" {
			t.Errorf("AuditRootVars has %q", v)
		}
	}
}

// The API starts without the seeds and ignores a seed file it is handed
// (the file is not read); the worker reads it.
func TestAuditRoot_APIIgnoresSeedFile(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "no-such-secret")
	api := validAPIEnv("AUDIT_ROOT_SIGNING_KEYS_FILE", missing, "AUDIT_ROOT_ARCHIVE_DIR", "relative/is/not/checked")
	if _, err := config.LoadAPI(api); err != nil {
		t.Fatalf("LoadAPI() with a worker-only seed file = %v", err)
	}
	if _, err := config.Load[auditRootOnly](map[string]string{"APP_ENV": "dev", "AUDIT_ROOT_SIGNING_KEYS_FILE": missing}); err == nil ||
		!strings.Contains(err.Error(), "AUDIT_ROOT_SIGNING_KEYS_FILE") {
		t.Fatalf("worker section with a missing seed file error = %v", err)
	}
}
