package config_test

import (
	"strings"
	"testing"

	"koro/internal/platform/config"
)

type webcacheOnly struct {
	config.Common
	Webcache config.Webcache
}

// strongRevalidateSecret is a test value: 64 hex characters, not a real key.
const strongRevalidateSecret = "0f1e2d3c4b5a69788796a5b4c3d2e1f00112233445566778899aabbccddeeff"

// TestWebcache_Validation: both values are required in staging/prod, may
// be absent in dev (the webhook is skipped), and the secret follows
// koro-web's 32-character minimum (BE-106).
func TestWebcache_Validation(t *testing.T) {
	t.Parallel()
	env := func(appEnv string, kv ...string) map[string]string {
		m := map[string]string{"APP_ENV": appEnv}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	full := []string{"KORO_WEB_INTERNAL_URL", "http://koro-web:3000", "KORO_REVALIDATE_SECRET", strongRevalidateSecret}

	dev, err := config.Load[webcacheOnly](env("dev"))
	if err != nil || dev.Webcache.Enabled() {
		t.Fatalf("dev without values = %+v, %v; want disabled, no error", dev.Webcache, err)
	}
	prod, err := config.Load[webcacheOnly](env("prod", full...))
	if err != nil || !prod.Webcache.Enabled() || prod.Webcache.RevalidateSecret.Reveal() != strongRevalidateSecret {
		t.Fatalf("prod with values = %v", err)
	}
	if strings.Contains(fmtV(prod.Webcache), strongRevalidateSecret) {
		t.Fatal("the secret is printed")
	}

	bad := []struct {
		name    string
		environ map[string]string
		want    string
	}{
		{"prod without url", env("prod", "KORO_REVALIDATE_SECRET", strongRevalidateSecret), "KORO_WEB_INTERNAL_URL is required in prod"},
		{"staging without secret", env("staging", "KORO_WEB_INTERNAL_URL", "http://koro-web:3000"), "KORO_REVALIDATE_SECRET is required in staging"},
		{"short secret", env("dev", "KORO_REVALIDATE_SECRET", "kisa-sir"), "at least 32 characters"},
		{"placeholder in prod", env("prod", "KORO_WEB_INTERNAL_URL", "http://koro-web:3000",
			"KORO_REVALIDATE_SECRET", strings.Repeat("ab", 20)), "placeholder"},
		{"url with path", env("dev", "KORO_WEB_INTERNAL_URL", "http://koro-web:3000/api"), "no path"},
		{"url scheme", env("dev", "KORO_WEB_INTERNAL_URL", "ftp://koro-web"), "http or https"},
		{"url without host", env("dev", "KORO_WEB_INTERNAL_URL", "http://"), "scheme://host"},
	}
	for _, c := range bad {
		if _, err := config.Load[webcacheOnly](c.environ); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: Load() = %v, want %q", c.name, err, c.want)
		}
	}
}

// TestWebcache_SecretFromFile_WorkerOnly: the secret comes from
// KORO_REVALIDATE_SECRET_FILE (Docker secret); only `koro worker` reads the
// section, the API never asks for the secret.
func TestWebcache_SecretFromFile_WorkerOnly(t *testing.T) {
	t.Parallel()
	path := writeSecret(t, strongRevalidateSecret+"\n")
	cfg, err := config.Load[webcacheOnly](map[string]string{
		"APP_ENV": "dev", "KORO_WEB_INTERNAL_URL": "http://localhost:3000", "KORO_REVALIDATE_SECRET_FILE": path,
	})
	if err != nil || cfg.Webcache.RevalidateSecret.Reveal() != strongRevalidateSecret {
		t.Fatalf("Load() from file = %v", err)
	}
	w, err := config.LoadWorker(validAPIEnv("KORO_WEB_INTERNAL_URL", "http://localhost:3000", "KORO_REVALIDATE_SECRET_FILE", path))
	if err != nil || !w.Webcache.Enabled() {
		t.Fatalf("LoadWorker() = %+v, %v", w.Webcache, err)
	}
	if _, err := config.LoadAPI(validAPIEnv()); err != nil {
		t.Fatalf("LoadAPI() without the webhook values = %v", err)
	}
}
