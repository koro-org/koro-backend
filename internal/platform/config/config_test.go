package config_test

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"koro/internal/platform/config"
)

// secretConfig exercises the loader with a required secret; BE-001 owns no
// real secret yet (DATABASE_URL, PASSWORD_PEPPER... arrive with their tickets).
type secretConfig struct {
	config.Common
	Token config.Secret `env:"TEST_TOKEN,required,notEmpty"`
	Other config.Secret `env:"TEST_OTHER"`
}

func devEnv(kv ...string) map[string]string {
	m := map[string]string{"APP_ENV": "dev"}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

func TestConfig_MissingSecret_FailsFast(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		environ map[string]string
		wantMsg string
	}{
		{"missing", devEnv("TEST_OTHER", "other-value-123"), `required environment variable "TEST_TOKEN" is not set`},
		{"empty", devEnv("TEST_TOKEN", "", "TEST_OTHER", "other-value-123"), `environment variable "TEST_TOKEN" should not be empty`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.Load[secretConfig](tt.environ)
			if err == nil {
				t.Fatal("Load() succeeded without the required secret")
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Fatalf("error %q does not name the variable (want %q)", err, tt.wantMsg)
			}
			if strings.Contains(err.Error(), "other-value-123") {
				t.Fatalf("error leaks a secret value: %q", err)
			}
		})
	}

	t.Run("api without required settings", func(t *testing.T) {
		t.Parallel()
		_, err := config.LoadAPI(map[string]string{})
		if err == nil {
			t.Fatal("LoadAPI() succeeded with an empty environment")
		}
		for _, v := range []string{"APP_ENV", "PUBLIC_BASE_URL", "TRUSTED_ORIGINS"} {
			if !strings.Contains(err.Error(), v) {
				t.Errorf("error %q does not mention %s", err, v)
			}
		}
	})
}

func TestConfig_FileSecret_ReadsFromFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "test_token")
	if err := os.WriteFile(path, []byte("file-s3cr3t\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("reads and trims", func(t *testing.T) {
		t.Parallel()
		environ := devEnv("TEST_TOKEN_FILE", path, "SSL_CERT_FILE", "/nonexistent/undeclared")
		cfg, err := config.Load[secretConfig](environ)
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if got := cfg.Token.Reveal(); got != "file-s3cr3t" {
			t.Fatalf("Token = %q, want file content", got)
		}
		if _, set := environ["TEST_TOKEN"]; set {
			t.Fatal("Load() modified the caller's environment map")
		}
	})
	t.Run("both set", func(t *testing.T) {
		t.Parallel()
		_, err := config.Load[secretConfig](devEnv("TEST_TOKEN", "inline", "TEST_TOKEN_FILE", path))
		if err == nil || !strings.Contains(err.Error(), "TEST_TOKEN and TEST_TOKEN_FILE are both set") {
			t.Fatalf("Load() error = %v, want both-set error", err)
		}
	})
	t.Run("missing file", func(t *testing.T) {
		t.Parallel()
		_, err := config.Load[secretConfig](devEnv("TEST_TOKEN_FILE", filepath.Join(dir, "absent")))
		if err == nil || !strings.Contains(err.Error(), "TEST_TOKEN_FILE") {
			t.Fatalf("Load() error = %v, want file error naming TEST_TOKEN_FILE", err)
		}
	})
}

func TestSecret_NeverPrintsValue(t *testing.T) {
	t.Parallel()
	s := config.NewSecret("  hunter2  ")
	if s.Reveal() != "hunter2" {
		t.Fatalf("Reveal() = %q", s.Reveal())
	}
	out := fmt.Sprintf("%v %s %+v %#v %q %x %d", s, s, s, s, s, s, s)
	b, err := json.Marshal(struct{ S config.Secret }{s})
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	slog.New(slog.NewTextHandler(&sb, nil)).Info("x", "s", s)
	for _, text := range []string{out, string(b), sb.String()} {
		if strings.Contains(text, "hunter2") || !strings.Contains(text, "[REDACTED]") {
			t.Fatalf("secret leaked or not redacted: %q", text)
		}
	}
}

func validAPIEnv(kv ...string) map[string]string {
	m := map[string]string{
		"APP_ENV":         "dev",
		"PUBLIC_BASE_URL": "http://localhost:3000",
		"TRUSTED_ORIGINS": "http://localhost:3000",
	}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

func TestConfig_LoadAPI_DefaultsAndValidation(t *testing.T) {
	t.Parallel()
	cfg, err := config.LoadAPI(validAPIEnv("TRUSTED_PROXIES", "10.0.0.0/8, 192.168.1.10,", "LOG_LEVEL", "DEBUG"))
	if err != nil {
		t.Fatalf("LoadAPI() error = %v", err)
	}
	if cfg.HTTP.Addr != ":8080" || cfg.HTTP.ShutdownDelay != 5*time.Second || cfg.HTTP.ShutdownTimeout != 25*time.Second ||
		cfg.HTTP.RequestTimeout != 15*time.Second || cfg.HTTP.ProblemTypeBase != "urn:koro:problem:" || cfg.LogFormat != "json" {
		t.Fatalf("unexpected defaults: %+v", cfg.HTTP)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Fatalf("LogLevel = %v", cfg.LogLevel)
	}
	if got := strings.Join(cfg.HTTP.TrustedProxies, "|"); got != "10.0.0.0/8|192.168.1.10" {
		t.Fatalf("TrustedProxies = %q", got)
	}

	bad := []struct {
		name    string
		environ map[string]string
		want    string
	}{
		{"unknown env", validAPIEnv("APP_ENV", "qa"), "APP_ENV must be one of"},
		{"text log outside dev", validAPIEnv("APP_ENV", "staging", "LOG_FORMAT", "text",
			"PUBLIC_BASE_URL", "https://koro.example", "TRUSTED_ORIGINS", "https://koro.example"), "LOG_FORMAT=text"},
		{"http origin in prod", validAPIEnv("APP_ENV", "prod", "PUBLIC_BASE_URL", "https://koro.example"), "must use https"},
		{"origin with path", validAPIEnv("TRUSTED_ORIGINS", "http://localhost:3000/app"), "scheme://host[:port] only"},
		{"openapi exposed in prod", validAPIEnv("APP_ENV", "prod", "EXPOSE_OPENAPI", "true",
			"PUBLIC_BASE_URL", "https://koro.example", "TRUSTED_ORIGINS", "https://koro.example"), "EXPOSE_OPENAPI"},
		{"bad proxy", validAPIEnv("TRUSTED_PROXIES", "proxy.local"), "TRUSTED_PROXIES"},
		{"zero timeout", validAPIEnv("SHUTDOWN_TIMEOUT", "0s"), "SHUTDOWN_TIMEOUT must be positive"},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.LoadAPI(tt.environ)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("LoadAPI() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestConfig_LoadHealthcheck_NeedsNothing(t *testing.T) {
	t.Parallel()
	cfg, err := config.LoadHealthcheck(nil)
	if err != nil || cfg.HTTPAddr != ":8080" {
		t.Fatalf("LoadHealthcheck(nil) = %+v, %v", cfg, err)
	}
}
