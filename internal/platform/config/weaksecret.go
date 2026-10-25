package config

import "strings"

// Placeholder detection for staging/prod secrets (BE-121). There are two
// kinds of secret and one check for each:
//
//   - weakKey: decoded key bytes (CRYPTO_*, AUDIT_ROOT_SIGNING_KEYS,
//     DISCOVERY_ANON_KEYS: base64 in the keyring, ≥ 32 bytes once decoded).
//   - weakSecretText: secrets used as text (PASSWORD_PEPPER,
//     KORO_REVALIDATE_SECRET, CRYPTO_OPENBAO_TOKEN; BE-119's
//     RATE_LIMIT_WEB_KEYS too), typically `openssl rand -hex 32` or
//     `-base64 32` (koro-infra swarm-secrets.sh) or an OpenBao token.
//
// Neither prints the value; callers name only the variable in the error.

// weakKey flags low-diversity key bytes such as the .env.example
// placeholders ("devdevdev…"): a random 32-byte key has about 30 distinct
// bytes, so fewer than 16 means it was not generated. It must not be used
// on text: a hex string has only 16 possible characters.
func weakKey(b []byte) bool { return distinctBytes(b) < 16 }

// Bounds of weakSecretText. Each is far below what a random value of the
// accepted alphabets reaches, so a generated secret is never refused
// (TestWeakSecretTextRandom); the false-positive estimates assume the
// shortest accepted value.
const (
	// minSecretText refuses values too short to be generated. The callers
	// have their own, larger minimums (32); this one covers
	// CRYPTO_OPENBAO_TOKEN ("s." + 24 characters at least).
	minSecretText = 16
	// minSecretTextChars: a 32-character random hex string misses 9 of the
	// 16 digits with probability below 1e-7.
	minSecretTextChars = 8
	// Distinct trigrams must be at least 3/4 of all trigrams: a repeated
	// pattern ("0123456789abcdef" × 4, "abab…") repeats its trigrams, a
	// random string of the hex alphabet (4096 trigrams) almost never does.
	trigramNum, trigramDen = 3, 4
)

// placeholderMarkers are words a generated value does not contain. Each
// has at least six letters and matches only in lower, upper or title case
// (as people type it): a random base64 string contains one with
// probability below 1e-7.
var placeholderMarkers = []string{
	"change", "dev-only", "example", "placeholder", "replace", "secret", "password", "pepper", "sample",
}

// hasPlaceholderMarker reports whether s contains a marker word.
func hasPlaceholderMarker(s string) bool {
	for _, m := range placeholderMarkers {
		if strings.Contains(s, m) || strings.Contains(s, strings.ToUpper(m)) ||
			strings.Contains(s, strings.ToUpper(m[:1])+m[1:]) {
			return true
		}
	}
	return false
}

// weakSecretText reports whether a text secret looks like a placeholder:
// too short, too few distinct characters, a marker word, letters of one
// case without digits, or a repeated pattern.
// Random hex, base64 and base62 (OpenBao token) values pass.
func weakSecretText(s string) bool {
	if len(s) < minSecretText || distinctBytes([]byte(s)) < minSecretTextChars {
		return true
	}
	return hasPlaceholderMarker(s) || wordLike(s) || repeatsTrigrams(s)
}

// distinctBytes counts the distinct byte values of b.
func distinctBytes(b []byte) int {
	var seen [256]bool
	n := 0
	for _, c := range b {
		if !seen[c] {
			seen[c] = true
			n++
		}
	}
	return n
}

// wordLike reports whether s has no digit and only one letter case, like
// typed words ("devpepper…", "my-long-passphrase"). Random hex has digits
// (all letters: below 1e-13, and only six of them); base64 and base62 mix
// both cases.
func wordLike(s string) bool {
	var lower, upper bool
	for _, c := range []byte(s) {
		switch {
		case c >= '0' && c <= '9':
			return false
		case c >= 'a' && c <= 'z':
			lower = true
		case c >= 'A' && c <= 'Z':
			upper = true
		}
	}
	return !lower || !upper
}

// repeatsTrigrams reports whether fewer than 3/4 of the trigrams of s are
// distinct.
func repeatsTrigrams(s string) bool {
	total := max(len(s)-2, 0)
	seen := make(map[string]struct{}, total)
	for i := range total {
		seen[s[i:i+3]] = struct{}{}
	}
	return len(seen)*trigramDen < total*trigramNum
}
