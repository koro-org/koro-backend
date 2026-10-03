package config

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

const redacted = "[REDACTED]"

// Secret holds a sensitive configuration value. Every textual form (fmt
// verbs, slog, JSON/text marshalling) prints [REDACTED]; the value is only
// available through Reveal.
type Secret struct{ v string }

// NewSecret wraps v (tests and programmatic construction).
func NewSecret(v string) Secret { return Secret{v: strings.TrimSpace(v)} }

// Reveal returns the secret value. Call it only where the value is used.
func (s Secret) Reveal() string { return s.v }

// IsZero reports whether the secret is empty.
func (s Secret) IsZero() bool { return s.v == "" }

// String implements fmt.Stringer.
func (Secret) String() string { return redacted }

// GoString implements fmt.GoStringer.
func (Secret) GoString() string { return redacted }

// Format implements fmt.Formatter so that no verb can print the value.
func (Secret) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redacted) }

// LogValue implements slog.LogValuer.
func (Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalText implements encoding.TextMarshaler (JSON included).
func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// UnmarshalText implements encoding.TextUnmarshaler; surrounding whitespace
// (including the trailing newline of secret files) is trimmed.
func (s *Secret) UnmarshalText(b []byte) error {
	s.v = strings.TrimSpace(string(b))
	return nil
}
