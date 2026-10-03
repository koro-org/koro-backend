package token_test

import (
	"crypto/sha256"
	"encoding/base64"
	"testing"

	"koro/internal/platform/token"
)

func TestToken_HashAndConstantTimeCompare(t *testing.T) {
	t.Parallel()
	raw, hash, err := token.New()
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != token.EncodedLen || !token.WellFormed(raw) {
		t.Fatalf("raw token %q is not 43 base64url characters", raw)
	}
	if b, err := base64.RawURLEncoding.DecodeString(raw); err != nil || len(b) != token.Size {
		t.Fatalf("raw token decodes to %d bytes, %v", len(b), err)
	}
	want := sha256.Sum256([]byte(raw))
	if len(hash) != token.HashSize || !token.Equal(hash, want[:]) || !token.Equal(token.Hash(raw), hash) {
		t.Fatal("hash is not SHA-256 of the raw token")
	}
	if !token.Verify(raw, hash) {
		t.Fatal("Verify(raw, hash) = false")
	}

	other, otherHash, err := token.New()
	if err != nil {
		t.Fatal(err)
	}
	if other == raw || token.Equal(otherHash, hash) {
		t.Fatal("two tokens are equal")
	}
	flipped := raw[:42] + "A"
	if raw[42] == 'A' {
		flipped = raw[:42] + "E"
	}
	if token.Verify(other, hash) || token.Verify(flipped, hash) || token.Verify("", hash) {
		t.Fatal("Verify accepted a wrong token")
	}
	if token.Equal(hash, hash[:31]) || token.Equal(nil, hash) || !token.Equal(nil, nil) {
		t.Fatal("Equal must compare lengths too")
	}

	for _, bad := range []string{"", raw[:42], raw + "A", raw[:42] + "=", raw[:42] + "+", raw[:42] + "/"} {
		if token.WellFormed(bad) {
			t.Errorf("WellFormed(%q) = true", bad)
		}
	}

	seen := map[string]bool{}
	for range 1000 {
		r, _, err := token.New()
		if err != nil || seen[r] {
			t.Fatalf("New() repeated a token or failed: %v", err)
		}
		seen[r] = true
	}
}
