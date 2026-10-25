package config_test

import (
	"slices"
	"strings"
	"testing"

	"koro/internal/platform/config"
)

type rateLimitOnly struct {
	config.Common
	RateLimit config.RateLimit
}

// Synthetic test values, not keys. webKeyNoF is 64 hex characters without
// the digit f (15 distinct characters): about one random
// `openssl rand -hex 32` in four lacks some digit and must still pass.
const (
	webKeyA   = "test-web-key-a-0123456789abcdefghijklmnopqrstuvwxyz"
	webKeyB   = "test-web-key-b-0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	webKeyNoF = "139d6ae29c82542da60abdcd96c2076dec57932526c15e3a6872a51602e5755b"
)

// TestRateLimit_WebKeys (BE-119): RATE_LIMIT_WEB_KEYS is optional in every
// environment (unset: koro-web counts against its IP), takes at most two
// keys (comma or newline separated, also from RATE_LIMIT_WEB_KEYS_FILE),
// rejects malformed, duplicate and placeholder keys, and never prints a key.
func TestRateLimit_WebKeys(t *testing.T) {
	t.Parallel()
	env := func(appEnv string, kv ...string) map[string]string {
		m := map[string]string{"APP_ENV": appEnv}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}

	for _, e := range []string{"dev", "staging", "prod"} {
		cfg, err := config.Load[rateLimitOnly](env(e))
		if err != nil || len(cfg.RateLimit.WebKeyList()) != 0 {
			t.Fatalf("%s without keys = %q, %v; want none, no error", e, cfg.RateLimit.WebKeyList(), err)
		}
	}
	cfg, err := config.Load[rateLimitOnly](env("prod", "RATE_LIMIT_WEB_KEYS", " "+webKeyA+" , "+webKeyNoF+"\n"))
	if err != nil || !slices.Equal(cfg.RateLimit.WebKeyList(), []string{webKeyA, webKeyNoF}) {
		t.Fatalf("prod with two keys = %q, %v", cfg.RateLimit.WebKeyList(), err)
	}
	if s := fmtV(cfg); strings.Contains(s, webKeyA) || strings.Contains(s, webKeyNoF) {
		t.Fatal("a key is printed")
	}
	path := writeSecret(t, webKeyA+"\n"+webKeyB+"\n")
	cfg, err = config.Load[rateLimitOnly](env("prod", "RATE_LIMIT_WEB_KEYS_FILE", path))
	if err != nil || !slices.Equal(cfg.RateLimit.WebKeyList(), []string{webKeyA, webKeyB}) {
		t.Fatalf("keys from file = %q, %v", cfg.RateLimit.WebKeyList(), err)
	}
	api, err := config.LoadAPI(validAPIEnv("RATE_LIMIT_WEB_KEYS", webKeyA))
	if err != nil || !slices.Equal(api.RateLimit.WebKeyList(), []string{webKeyA}) {
		t.Fatalf("LoadAPI() = %q, %v", api.RateLimit.WebKeyList(), err)
	}
	if _, err := config.Load[rateLimitOnly](env("dev", "RATE_LIMIT_WEB_KEYS", strings.Repeat("ab", 20))); err != nil {
		t.Fatalf("dev placeholder rejected: %v", err)
	}

	bad := []struct {
		name, keys, want string
		appEnv           string
	}{
		{"three keys", webKeyA + "," + webKeyB + "," + webKeyNoF, "at most 2 keys", "dev"},
		{"short", "kisa-anahtar", "key 1 must be 32–256 characters", "dev"},
		{"too long", strings.Repeat(webKeyA, 6), "key 1 must be 32–256 characters", "dev"},
		{"header-unsafe character", webKeyA[:40] + `"x;y`, "key 1 must be", "dev"},
		{"duplicate", webKeyA + "," + webKeyA, "key 2 is listed twice", "dev"},
		{"low diversity in prod", strings.Repeat("ab", 20), "key 1 looks like a placeholder", "prod"},
		{"change-me in staging", webKeyB + ",CHANGE-ME-0123456789abcdefghijklmnopqrstuv", "key 2 looks like a placeholder", "staging"},
	}
	for _, b := range bad {
		_, err := config.Load[rateLimitOnly](env(b.appEnv, "RATE_LIMIT_WEB_KEYS", b.keys))
		if err == nil || !strings.Contains(err.Error(), b.want) {
			t.Errorf("%s: Load() = %v, want %q", b.name, err, b.want)
			continue
		}
		for k := range strings.SplitSeq(b.keys, ",") {
			if strings.Contains(err.Error(), k) {
				t.Errorf("%s: the error prints a key: %v", b.name, err)
			}
		}
	}
}
