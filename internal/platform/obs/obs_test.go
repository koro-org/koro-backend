package obs_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

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
	tel, err := obs.SetupTelemetry(t.Context(), obs.TelemetryOptions{Service: "koro-test"})
	if err != nil {
		t.Fatalf("SetupTelemetry() error = %v", err)
	}
	if tel.Metrics != nil || tel.Tracing() || tel.MetricsAddr() != "" {
		t.Fatalf("empty options must install nothing: metrics=%v tracing=%v addr=%q", tel.Metrics, tel.Tracing(), tel.MetricsAddr())
	}
	_, span := tel.TracerProvider().Tracer("t").Start(t.Context(), "x")
	if span.IsRecording() || span.SpanContext().IsValid() {
		t.Fatal("tracer provider must be a no-op when tracing is off")
	}
	span.End()
	// The nil registry is safe to call: callers never branch.
	tel.Metrics.ObserveHTTP("GET", "/x", 200, 0)
	tel.Metrics.AuthzDenied("m", "a", "r")
	if err := tel.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
}

// TestObs_Scrub_PhoneLeftBoundary (BE-108): phone masking must keep catching
// every real phone shape in running text, but never the digits at the tail
// of an identifier. Synthetic 555 numbers only.
func TestObs_Scrub_PhoneLeftBoundary(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		// Still masked: formats.
		{"+90 555 123 45 67", "***67"},
		{"+905551234567", "***67"},
		{"0090 555 123 45 67", "***67"},
		{"00905551234567", "***67"},
		{"905551234567", "***67"},
		{"05551234567", "***67"},
		{"0555 123 45 67", "***67"},
		{"0 (555) 123 45 67", "***67"},
		{"(555) 123-45-67", "***67"},
		{"555.123.45.67", "***67"},
		{"0555-123-45-67", "***67"},
		{"555 123 45 67", "***67"},
		{"5551234567", "***67"},
		// Still masked: delimiters before the number.
		{"tel:05551234567", "tel:***67"},
		{`{"phone_alt":"5551234567"}`, `{"phone_alt":"***67"}`},
		{"id=5551234567", "id=***67"},
		{"phone='0555 123 45 67'", "phone='***67'"},
		{"Arayın: 0555 123 45 67.", "Arayın: ***67."},
		{"(+90 555 123 45 67)", "(***67)"},
		{"x+905551234567", "x***67"},
		{"no_5551234567", "no_***67"},
		{"ara,05551234567;", "ara,***67;"},
		{"çağrı\t5551234567\n", "çağrı\t***67\n"},
		{"GET /u?tel=%2B905551234567", "GET /u?tel=%2B***67"},
		{"GET /u?q=tel%3A05551234567", "GET /u?q=tel%3A***67"},
		{"two 5551234567 and 05559876543", "two ***67 and ***43"},
		{"ord-5551234567", "ord-***67"},
		// Not masked: digits at the tail of an identifier.
		{"request_id=01a16f74-50e8-7e6f-9999-0b5553195418", "request_id=01a16f74-50e8-7e6f-9999-0b5553195418"},
		{"01a16f74-50e8-7e6f-9999-905551234567", "01a16f74-50e8-7e6f-9999-905551234567"},
		{"01A16F74-50E8-7E6F-9999-905551234567", "01A16F74-50E8-7E6F-9999-905551234567"},
		{"55512345-5551-7555-9555-555123456789", "55512345-5551-7555-9555-555123456789"},
		{"90551234-5678-7abc-8def-0123456789ab", "90551234-5678-7abc-8def-0123456789ab"},
		{"31590187-5804-3493-6847-409968539732", "31590187-5804-3493-6847-409968539732"},
		{"12345678-1234-1234-1234-123456789012", "12345678-1234-1234-1234-123456789012"},
		{"trace=4bf92f3577b34da6a3ce90b5553195418", "trace=4bf92f3577b34da6a3ce90b5553195418"},
		{"sha=ab05551234567", "sha=ab05551234567"},
		{"a905551234567", "a905551234567"},
		// TCKN: Go's \b is an ASCII word boundary; letter→digit is not one.
		{"0b12345678901", "0b12345678901"},
		{"-12345678901-", "-[TCKN]-"},
	}
	for _, tt := range tests {
		if got := obs.Scrub(tt.in); got != tt.want {
			t.Errorf("Scrub(%q) = %q, want %q", tt.in, got, tt.want)
		}
		if got := obs.Scrub(obs.Scrub(tt.in)); got != tt.want {
			t.Errorf("Scrub not idempotent on %q: %q", tt.in, got)
		}
	}
}

// TestObs_Scrub_LeavesUUIDsAlone (BE-108): Scrub is the identity on UUIDs,
// bare or in the shapes they are logged in. Random UUIDv7s plus UUID-shaped
// strings made of digits only (the worst case for the digit patterns).
func TestObs_Scrub_LeavesUUIDsAlone(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 108))
	const hexDigits, decDigits = "0123456789abcdef", "0123456789"
	shaped := func(alphabet string) string {
		var b [36]byte
		for i := range b {
			switch i {
			case 8, 13, 18, 23:
				b[i] = '-'
			default:
				b[i] = alphabet[rng.IntN(len(alphabet))]
			}
		}
		return string(b[:])
	}
	v7 := func() string {
		var u [16]byte
		ms := uint64(time.Now().UnixMilli()) + rng.Uint64N(1<<40)
		for i := range 6 {
			u[i] = byte(ms >> (40 - 8*i))
		}
		for i := 6; i < 16; i++ {
			u[i] = byte(rng.Uint32())
		}
		u[6] = u[6]&0x0f | 0x70
		u[8] = u[8]&0x3f | 0x80
		h := hex.EncodeToString(u[:])
		return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
	}
	gens := []func() string{v7, func() string { return shaped(hexDigits) }, func() string { return shaped(decDigits) }}
	for i := range 6000 {
		id := gens[i%len(gens)]()
		for _, in := range []string{id, "request_id=" + id, `{"id":"` + id + `"}`, "user " + id + " left", strings.ToUpper(id)} {
			if got := obs.Scrub(in); got != in {
				t.Fatalf("Scrub(%q) = %q, want it unchanged", in, got)
			}
		}
	}
}
