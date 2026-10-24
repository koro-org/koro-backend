package config_test

import (
	"strings"
	"testing"

	"koro/internal/platform/config"
)

type discoveryOnly struct {
	config.Common
	Discovery config.Discovery
}

// Fake discovery keys of the tests (BE-102). fakeAnonKey1/2 are counting
// byte sequences (0x00…0x1f, 0x20…0x3f): 32 distinct bytes, so they pass
// the placeholder check, and obviously not generated. devAnonKey is the
// .env.example placeholder ("anon" × 8).
const (
	fakeAnonKey1 = "fake1:AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
	fakeAnonKey2 = "fake2:ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8="
	fakeAnonKey3 = "fake3:QEFCQ0RFRkdISUpLTE1OT1BRUlNUVVZXWFlaW1xdXl8="
	devAnonKey   = "dev1:YW5vbmFub25hbm9uYW5vbmFub25hbm9uYW5vbmFub24="
)

// TestDiscovery_AnonKeysValidation (BE-102, T-329): DISCOVERY_ANON_KEYS is
// discovery's own keyring. Staging and prod refuse to start without it or
// with a placeholder; dev may leave it empty (the token keyring fallback).
// At most two keys (active + the previous one of the renewal window), each
// at least 32 bytes, distinct ids; no error message carries key material.
func TestDiscovery_AnonKeysValidation(t *testing.T) {
	t.Parallel()
	env := func(kv ...string) map[string]string {
		m := map[string]string{"APP_ENV": "prod", "DISCOVERY_ANON_KEYS": fakeAnonKey2 + "," + fakeAnonKey1}
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
		keys    int    // the parsed ring when valid
	}{
		{"prod with active and previous key", env(), "", 2},
		{"prod with one key", env("DISCOVERY_ANON_KEYS", fakeAnonKey1), "", 1},
		{"prod requires the keyring", env("DISCOVERY_ANON_KEYS", "<unset>"), "DISCOVERY_ANON_KEYS is required in prod", 0},
		{"staging requires the keyring", env("APP_ENV", "staging", "DISCOVERY_ANON_KEYS", "<unset>"), "DISCOVERY_ANON_KEYS is required in staging", 0},
		{"prod rejects an empty value", env("DISCOVERY_ANON_KEYS", "  "), "DISCOVERY_ANON_KEYS is required in prod", 0},
		{"dev may leave it empty", env("APP_ENV", "dev", "DISCOVERY_ANON_KEYS", "<unset>"), "", 0},
		{"dev placeholder key", env("APP_ENV", "dev", "DISCOVERY_ANON_KEYS", devAnonKey), "", 1},
		{"prod rejects the placeholder", env("DISCOVERY_ANON_KEYS", devAnonKey), "looks like a placeholder", 0},
		{"staging rejects a placeholder as the previous key", env("APP_ENV", "staging", "DISCOVERY_ANON_KEYS", fakeAnonKey1+","+devAnonKey), "looks like a placeholder", 0},
		{"at most two keys", env("DISCOVERY_ANON_KEYS", fakeAnonKey3+","+fakeAnonKey2+","+fakeAnonKey1), "at most 2 keys", 0},
		{"at most two keys in dev too", env("APP_ENV", "dev", "DISCOVERY_ANON_KEYS", fakeAnonKey3+","+fakeAnonKey2+","+fakeAnonKey1), "at most 2 keys", 0},
		{"key shorter than 32 bytes", env("DISCOVERY_ANON_KEYS", "fake1:c2hvcnQ="), "must be 32", 0},
		{"duplicate ids", env("DISCOVERY_ANON_KEYS", fakeAnonKey1+","+strings.Replace(fakeAnonKey2, "fake2", "fake1", 1)), "duplicate key id", 0},
		{"keyring format", env("DISCOVERY_ANON_KEYS", "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="), "DISCOVERY_ANON_KEYS", 0},
		{"not base64", env("DISCOVERY_ANON_KEYS", "fake1:AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"), "not standard base64", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.Load[discoveryOnly](tt.environ)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Load() error = %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("Load() error = %v, want %q", err, tt.wantErr)
			case err != nil && (strings.Contains(err.Error(), "AAECAwQF") || strings.Contains(err.Error(), "ICEiIyQl") ||
				strings.Contains(err.Error(), "YW5vbmFu") || strings.Contains(err.Error(), "c2hvcnQ")):
				t.Fatalf("error leaks key material: %v", err)
			}
			if err != nil {
				return
			}
			keys, kerr := cfg.Discovery.AnonKeyring()
			if kerr != nil || len(keys) != tt.keys {
				t.Fatalf("AnonKeyring() = %d keys, %v; want %d", len(keys), kerr, tt.keys)
			}
			if tt.keys == 2 && (keys[0].ID != "fake2" || keys[1].ID != "fake1") {
				t.Fatalf("ring order %q, %q; want the active key first", keys[0].ID, keys[1].ID)
			}
		})
	}
}

// TestDiscovery_AnonKeysFromFile (BE-102): the keyring comes from a Docker
// secret file in staging/prod (DISCOVERY_ANON_KEYS_FILE), never printed.
func TestDiscovery_AnonKeysFromFile(t *testing.T) {
	t.Parallel()
	path := writeSecret(t, fakeAnonKey1+"\n")
	cfg, err := config.Load[discoveryOnly](map[string]string{"APP_ENV": "prod", "DISCOVERY_ANON_KEYS_FILE": path})
	if err != nil {
		t.Fatal(err)
	}
	if keys, err := cfg.Discovery.AnonKeyring(); err != nil || len(keys) != 1 || keys[0].ID != "fake1" {
		t.Fatalf("AnonKeyring() = %v, %v", len(keys), err)
	}
	if s := strings.Join([]string{cfg.Discovery.AnonKeys.String(), fmtV(cfg.Discovery)}, " "); strings.Contains(s, "AAECAwQF") {
		t.Fatalf("the keyring prints: %s", s)
	}
}
