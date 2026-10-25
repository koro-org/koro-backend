package config_test

import (
	"encoding/base64"
	"encoding/hex"
	"math/rand/v2"
	"strings"
	"testing"

	"koro/internal/platform/config"
)

// randomSamples is the number of generated values per format. The seed is
// fixed so a failure is reproducible; the bounds of weakSecretText keep the
// false-positive rate of every format far below 1/randomSamples.
const randomSamples = 20000

// newRand returns the deterministic generator of one test.
func newRand(seed byte) *rand.ChaCha8 {
	var s [32]byte
	s[0] = seed
	return rand.NewChaCha8(s)
}

func randomBytes(r *rand.ChaCha8, n int) []byte {
	b := make([]byte, n)
	_, _ = r.Read(b)
	return b
}

const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

func randomBase62(r *rand.ChaCha8, n int) string {
	b := randomBytes(r, n)
	for i := range b {
		b[i] = base62[int(b[i])%len(base62)]
	}
	return string(b)
}

// TestWeakSecretTextRandom: generated text secrets are never refused —
// `openssl rand -hex 32` (koro-infra password-pepper, web-revalidate-secret),
// -hex 16 (the shortest accepted length), -base64 32/48 and OpenBao tokens
// (legacy "s." + 24 and "hvs." + 91 base62 characters).
func TestWeakSecretTextRandom(t *testing.T) {
	t.Parallel()
	r := newRand(121)
	formats := []struct {
		name string
		gen  func() string
	}{
		{"hex 32 bytes", func() string { return hex.EncodeToString(randomBytes(r, 32)) }},
		{"hex 16 bytes", func() string { return hex.EncodeToString(randomBytes(r, 16)) }},
		{"base64 32 bytes", func() string { return base64.StdEncoding.EncodeToString(randomBytes(r, 32)) }},
		{"base64 48 bytes", func() string { return base64.StdEncoding.EncodeToString(randomBytes(r, 48)) }},
		{"OpenBao legacy token", func() string { return "s." + randomBase62(r, 24) }},
		{"OpenBao service token", func() string { return "hvs." + randomBase62(r, 91) }},
	}
	for _, f := range formats {
		refused, keyRuleRefused := 0, 0
		for range randomSamples {
			v := f.gen()
			if config.WeakSecretText(v) {
				refused++
			}
			if config.WeakKey([]byte(v)) { // the pre-BE-121 rule, for the log
				keyRuleRefused++
			}
		}
		if refused != 0 {
			t.Errorf("%s: %d of %d random values refused as placeholders", f.name, refused, randomSamples)
		}
		t.Logf("%s: text rule refused %d/%d, byte-key rule would refuse %d/%d",
			f.name, refused, randomSamples, keyRuleRefused, randomSamples)
	}
}

// TestWeakKeyRandom: random decoded keys (32-byte keyring entries) pass.
func TestWeakKeyRandom(t *testing.T) {
	t.Parallel()
	r := newRand(122)
	for i := range randomSamples {
		if config.WeakKey(randomBytes(r, 32)) {
			t.Fatalf("random 32-byte key %d refused", i)
		}
	}
}

// placeholderTexts are values a person types instead of generating one:
// the .env.example development values, "change me" variants, repeated
// characters and patterns, single-class strings and short values.
var placeholderTexts = []string{
	"devpepperdevpepperdevpepperdevpepper", // .env.example PASSWORD_PEPPER
	"dev-only-openbao-root",                // .env.example CRYPTO_OPENBAO_TOKEN
	"dev-only-root-token",
	"dev-only-change-me",
	"devdevdevdev",
	"dev-root",
	"CHANGE-ME-CHANGE-ME-CHANGE-ME-CHANGE-ME",
	"changeme0123456789abcdef0123456789",
	"Q8yJ2mZ5bW1tK4xR-CHANGEME-7nV0cL3hQ8yJ",
	"replace-with-openssl-rand-hex-32-output",
	"your-Secret-here-0123456789-abcdefgh",
	"example-revalidate-Secret-value-0123",
	"placeholder-0123456789-ABCDEFGHIJKLMN",
	"Password1234567890Password1234567890",
	"Dummy0123456789ABCDEFdummy0123456789",
	strings.Repeat("ab", 20),
	strings.Repeat("a", 64),
	strings.Repeat("0", 64),
	strings.Repeat("0123456789abcdef", 4),
	strings.Repeat("Abc123", 8),
	strings.Repeat("x1Y2z3", 7),
	"abcdefghijklmnopqrstuvwxyzabcdefghij",
	"ABCDEFGHIJKLMNOPQRSTUVWXYZABCDEFGHIJ",
	"12345678901234567890123456789012345",
	"aB3$",
	"Q8yJ2mZ5bW1tK4x",
	"",
}

// TestWeakSecretTextPlaceholders: every known placeholder is refused.
func TestWeakSecretTextPlaceholders(t *testing.T) {
	t.Parallel()
	for i, v := range placeholderTexts {
		if !config.WeakSecretText(v) {
			t.Errorf("placeholder %d (%d characters) accepted", i, len(v))
		}
	}
}

// TestWeakKeyPlaceholders: the decoded .env.example keys are refused.
func TestWeakKeyPlaceholders(t *testing.T) {
	t.Parallel()
	for _, k := range []string{
		"ZGV2ZGV2ZGV2ZGV2ZGV2ZGV2ZGV2ZGV2ZGV2ZGV2ZGU=", // CRYPTO_KEKS
		"aWR4aWR4aWR4aWR4aWR4aWR4aWR4aWR4aWR4aWR4aWQ=", // CRYPTO_BLIND_INDEX_KEY
		"dG9rdG9rdG9rdG9rdG9rdG9rdG9rdG9rdG9rdG9rdG8=", // CRYPTO_TOKEN_KEYS
		"YXVkaXRhdWRpdGF1ZGl0YXVkaXRhdWRpdGF1ZGl0YXU=", // AUDIT_ROOT_SIGNING_KEYS
		"YW5vbmFub25hbm9uYW5vbmFub25hbm9uYW5vbmFub24=", // DISCOVERY_ANON_KEYS
	} {
		b, err := base64.StdEncoding.DecodeString(k)
		if err != nil {
			t.Fatal(err)
		}
		if !config.WeakKey(b) {
			t.Errorf("placeholder key %s accepted", k[:4])
		}
	}
}

type textSecretsOnly struct {
	config.Common
	Passwd    config.Passwd
	Webcache  config.Webcache
	RateLimit config.RateLimit
}

// TestTextSecretsInProd: PASSWORD_PEPPER, KORO_REVALIDATE_SECRET and
// RATE_LIMIT_WEB_KEYS accept random `openssl rand -hex 32` values in prod
// (BE-121: about one in four was refused before) and refuse placeholders
// without printing them.
func TestTextSecretsInProd(t *testing.T) {
	t.Parallel()
	env := func(pepper, revalidate, webKey string) map[string]string {
		return map[string]string{
			"APP_ENV": "prod", "PASSWORD_PEPPER": pepper, "RATE_LIMIT_WEB_KEYS": webKey,
			"KORO_WEB_INTERNAL_URL": "http://koro-web:3000", "KORO_REVALIDATE_SECRET": revalidate,
		}
	}
	r := newRand(123)
	for i := range 2000 {
		pepper, revalidate := hex.EncodeToString(randomBytes(r, 32)), hex.EncodeToString(randomBytes(r, 32))
		webKey := base64.StdEncoding.EncodeToString(randomBytes(r, 32))
		if _, err := config.Load[textSecretsOnly](env(pepper, revalidate, webKey)); err != nil {
			t.Fatalf("random hex secrets %d refused: %v", i, err)
		}
	}
	for _, v := range placeholderTexts {
		if len(v) < 32 { // both length minimums (32) are checked first
			continue
		}
		_, err := config.Load[textSecretsOnly](env(v, v, v))
		if err == nil {
			t.Errorf("placeholder of %d characters accepted", len(v))
			continue
		}
		msg := err.Error()
		if !strings.Contains(msg, "PASSWORD_PEPPER looks like a placeholder") ||
			!strings.Contains(msg, "KORO_REVALIDATE_SECRET looks like a placeholder") ||
			!strings.Contains(msg, "RATE_LIMIT_WEB_KEYS: key 1 looks like a placeholder") {
			t.Errorf("placeholder of %d characters: %v", len(v), err)
		}
		if strings.Contains(msg, v) {
			t.Error("the error prints the secret value")
		}
	}
}
