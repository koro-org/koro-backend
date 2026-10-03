package obs_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"koro/internal/platform/obs"
)

// Synthetic test vectors only (05 §13.7).
func TestObs_Scrub_MasksEmailPhoneIBANToken(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"email", "invite sent to ayse.yilmaz@ogrenci.edu.tr today", "invite sent to a***@o***.tr today"},
		{"phone international", "call +90 532 123 45 67", "call ***67"},
		{"phone national", "call 05321234567 now", "call ***67 now"},
		{"phone bare", "5321234567", "***67"},
		{"iban spaced", "pay TR33 0006 1005 1978 6457 8413 26 please", "pay TR**…1326 please"},
		{"iban compact", "TR330006100519786457841326", "TR**…1326"},
		{"tckn", "id 12345678901", "id [TCKN]"},
		{"bearer token", "Authorization: Bearer abc.DEF-123_xyz=", "Authorization: Bearer [REDACTED]"},
		{"session cookie", "Cookie: __Host-koro_session=s3cr3tValue; other=1", "Cookie: __Host-koro_session=[REDACTED]; other=1"},
		{"query token", "GET /confirm?token=abcdef123&x=1", "GET /confirm?token=[REDACTED]&x=1"},
		{"json password", `{"email_hint":"x","password":"hunter2!"}`, `{"email_hint":"x","password":"[REDACTED]"}`},
		{"uuid untouched", "event 0192f5c4-1d2e-7a3b-9c4d-5e6f708192a3", "event 0192f5c4-1d2e-7a3b-9c4d-5e6f708192a3"},
		{"timestamp untouched", "at 2026-10-03T09:15:00.123Z took 1759480000123ns", "at 2026-10-03T09:15:00.123Z took 1759480000123ns"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := obs.Scrub(tt.in); got != tt.want {
				t.Fatalf("Scrub(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestObs_Masks_AreIdempotent(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"ayse@ogrenci.edu.tr", "+90 532 123 45 67", "TR33 0006 1005 1978 6457 8413 26"} {
		once := obs.Scrub(in)
		if twice := obs.Scrub(once); twice != once {
			t.Fatalf("Scrub not idempotent: %q → %q → %q", in, once, twice)
		}
	}
	if got := obs.MaskPhone(obs.MaskPhone("5321234567")); got != "***67" {
		t.Fatalf("MaskPhone twice = %q", got)
	}
	if got := obs.MaskIBAN(obs.MaskIBAN("TR330006100519786457841326")); got != "TR**…1326" {
		t.Fatalf("MaskIBAN twice = %q", got)
	}
}

type secretish struct{}

func (secretish) LogValue() slog.Value { return slog.StringValue("[REDACTED]") }

func TestObs_Logger_RedactsByTypeKeyAndScrub(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := obs.NewLogger(&buf, obs.LoggerOptions{Level: slog.LevelDebug, Service: "koro-test", Env: "dev"})
	ctx := obs.WithActorID(obs.WithRequestID(t.Context(), "req-12345678"), "0192f5c4-1d2e-7a3b-9c4d-5e6f708192a3")

	log.InfoContext(ctx, "user invited",
		slog.Any("invitee", obs.Email("mehmet.kaya@example.test")),
		slog.String("email", "zeynep@example.test"),
		slog.String("phone", "05321234567"),
		slog.String("password", "hunter2"),
		slog.String("access_token", "tok_abc"),
		slog.Any("pepper", secretish{}),
		slog.String("note", "reach me at ali@example.test or 05329876543"),
		slog.Any("err", errors.New("pq: duplicate key value (iban)=(TR330006100519786457841326)")),
	)

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("log line is not JSON: %v\n%s", err, buf.String())
	}
	want := map[string]string{
		"invitee":      "m***@e***.test",
		"email":        "z***@e***.test",
		"phone":        "***67",
		"password":     "[REDACTED]",
		"access_token": "[REDACTED]",
		"pepper":       "[REDACTED]",
		"note":         "reach me at a***@e***.test or ***43",
		"err":          "pq: duplicate key value (iban)=(TR**…1326)",
		"request_id":   "req-12345678",
		"actor_id":     "0192f5c4-1d2e-7a3b-9c4d-5e6f708192a3",
		"service":      "koro-test",
		"env":          "dev",
	}
	for k, v := range want {
		if got, _ := rec[k].(string); got != v {
			t.Errorf("field %q = %q, want %q", k, got, v)
		}
	}
	for _, leak := range []string{"hunter2", "tok_abc", "mehmet", "zeynep", "ali@", "TR3300", "05321234567"} {
		if strings.Contains(buf.String(), leak) {
			t.Errorf("log line leaks %q: %s", leak, buf.String())
		}
	}
}

func TestObs_SetupTelemetry_EmptyIsNoop(t *testing.T) {
	t.Parallel()
	shutdown, err := obs.SetupTelemetry(t.Context(), obs.TelemetryOptions{Service: "koro-test"})
	if err != nil {
		t.Fatalf("SetupTelemetry() error = %v", err)
	}
	if err := shutdown(t.Context()); err != nil {
		t.Fatalf("shutdown() error = %v", err)
	}
	if _, err := obs.SetupTelemetry(t.Context(), obs.TelemetryOptions{OTLPEndpoint: "http://otel:4318"}); !errors.Is(err, obs.ErrTelemetryNotImplemented) {
		t.Fatalf("configured exporter must fail fast, got %v", err)
	}
}
