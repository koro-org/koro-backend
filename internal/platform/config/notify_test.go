package config_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"koro/internal/platform/config"
)

type notifyOnly struct {
	config.Common
	Notify config.Notify
}

func TestNotify_Defaults_TargetLocalMailpit(t *testing.T) {
	t.Parallel()
	cfg, err := config.Load[notifyOnly](notifyDevEnv())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	n := cfg.Notify
	if n.SMTPHost != "localhost" || n.SMTPPort != 1025 || n.SMTPTLS != config.SMTPTLSNone ||
		n.SMTPFrom != "noreply@koro.test" || n.SMTPFromName != "Koro" || n.SMTPTimeout != 15*time.Second {
		t.Fatalf("defaults = %+v, want the Mailpit setup of 05 §12.3", n)
	}
}

func TestNotify_Validation(t *testing.T) {
	t.Parallel()
	prod := func(kv ...string) map[string]string {
		m := map[string]string{
			"APP_ENV": "prod", "SMTP_HOST": "smtp.koro.example.org", "SMTP_PORT": "587", "SMTP_TLS": "starttls",
			"SMTP_FROM": "noreply@bilet.koro.app", "SMTP_USERNAME": "koro", "SMTP_PASSWORD": "s3cr3t-pass",
		}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	tests := []struct {
		name    string
		environ map[string]string
		wantErr string // "" = valid
	}{
		{"dev defaults", notifyDevEnv(), ""},
		{"dev with explicit relay", notifyDevEnv("SMTP_HOST", "mailpit", "SMTP_PORT", "1025"), ""},
		{"prod with a real relay", prod(), ""},
		{"prod implicit tls", prod("SMTP_PORT", "465", "SMTP_TLS", "tls"), ""},
		{"prod without credentials", prod("SMTP_USERNAME", "", "SMTP_PASSWORD", ""), ""},
		{"staging also rejects the dev defaults", map[string]string{"APP_ENV": "staging"}, "SMTP_HOST must be a real relay"},
		{"prod rejects loopback relay", prod("SMTP_HOST", "127.0.0.1"), "SMTP_HOST must be a real relay"},
		{"prod rejects localhost", prod("SMTP_HOST", "localhost"), "SMTP_HOST must be a real relay"},
		{"prod rejects plain text", prod("SMTP_TLS", "none", "SMTP_USERNAME", "", "SMTP_PASSWORD", ""), "SMTP_TLS=none is allowed only with APP_ENV=dev"},
		{"prod rejects placeholder sender domain", prod("SMTP_FROM", "noreply@koro.test"), "placeholder domain"},
		{"prod rejects .example sender", prod("SMTP_FROM", "noreply@koro.example"), "placeholder domain"},
		{"unknown tls mode", notifyDevEnv("SMTP_TLS", "opportunistic"), "SMTP_TLS must be none, starttls or tls"},
		{"port zero", notifyDevEnv("SMTP_PORT", "0"), "SMTP_PORT must be between"},
		{"port too large", notifyDevEnv("SMTP_PORT", "70000"), "SMTP_PORT must be between"},
		{"host with a path", notifyDevEnv("SMTP_HOST", "smtp.koro.app/x"), "SMTP_HOST must be a host name"},
		{"sender with a display name", notifyDevEnv("SMTP_FROM", "Koro <noreply@koro.test>"), "SMTP_FROM must be a bare e-mail"},
		{"sender without a domain", notifyDevEnv("SMTP_FROM", "noreply"), "SMTP_FROM must be a bare e-mail"},
		{"sender name with a newline", notifyDevEnv("SMTP_FROM_NAME", "Koro\r\nBcc: x@y.z"), "SMTP_FROM_NAME"},
		{"sender name too long", notifyDevEnv("SMTP_FROM_NAME", strings.Repeat("k", 101)), "SMTP_FROM_NAME"},
		{"username without password", notifyDevEnv("SMTP_USERNAME", "koro"), "must be set together"},
		{"password without username", notifyDevEnv("SMTP_PASSWORD", "x"), "must be set together"},
		{"credentials over plain text", notifyDevEnv("SMTP_USERNAME", "koro", "SMTP_PASSWORD", "x"), "SMTP_USERNAME needs SMTP_TLS"},
		{"zero timeout", notifyDevEnv("SMTP_TIMEOUT", "0s"), "SMTP_TIMEOUT must be positive"},
		{"timeout longer than the delivery lease allows", notifyDevEnv("SMTP_TIMEOUT", "61s"), "SMTP_TIMEOUT must be positive and at most 1m"},
		{"one minute is fine", notifyDevEnv("SMTP_TIMEOUT", "1m"), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.Load[notifyOnly](tt.environ)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Load() error = %v, want none", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("Load() error = %v, want it to contain %q", err, tt.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), "s3cr3t-pass") {
				t.Fatalf("error leaks the SMTP password: %v", err)
			}
		})
	}
}

func TestNotify_PasswordNeverPrinted(t *testing.T) {
	t.Parallel()
	cfg, err := config.Load[notifyOnly](notifyDevEnv("SMTP_PASSWORD_FILE", writeSecret(t, "file-pass\n"), "SMTP_USERNAME", "koro", "SMTP_TLS", "starttls"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := cfg.Notify.SMTPPassword.Reveal(); got != "file-pass" {
		t.Fatalf("password = %q, want the trimmed file content", got)
	}
	if s := strings.Join([]string{cfg.Notify.SMTPPassword.String(), fmtV(cfg.Notify)}, " "); strings.Contains(s, "file-pass") {
		t.Fatalf("password printed: %s", s)
	}
}

func fmtV(v any) string { return fmt.Sprintf("%+v %#v", v, v) }

func writeSecret(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "smtp_password")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func notifyDevEnv(kv ...string) map[string]string {
	m := map[string]string{"APP_ENV": "dev"}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}
