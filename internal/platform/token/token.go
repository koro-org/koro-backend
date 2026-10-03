// Package token creates opaque bearer tokens (05 §14.2): sessions,
// invitations, e-mail verification and password reset all use the same
// pattern. A token is 32 bytes from crypto/rand, sent once to the client as
// base64url (43 characters); the database stores only SHA-256(token) in a
// unique bytea column and looks it up with `WHERE token_hash = $1`. 256 bits
// of entropy need no slow hash. Raw tokens and their hashes are never logged.
//
// In-memory comparisons of secrets go through Equal / Verify, which use
// crypto/subtle (05 §14.3). The package is a leaf (standard library only).
package token

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
)

// Size is the number of random bytes in a token; EncodedLen its length in
// base64url without padding.
const (
	Size       = 32
	EncodedLen = 43
	HashSize   = sha256.Size
)

// New returns a fresh random token and its SHA-256 hash.
func New() (raw string, hash []byte, err error) {
	b := make([]byte, Size)
	if _, err := rand.Read(b); err != nil {
		return "", nil, fmt.Errorf("token: %w", err)
	}
	raw = base64.RawURLEncoding.EncodeToString(b)
	return raw, Hash(raw), nil
}

// Hash returns SHA-256 of the raw token as stored in the database.
func Hash(raw string) []byte {
	s := sha256.Sum256([]byte(raw))
	return s[:]
}

// WellFormed reports whether raw has the shape of a token produced by New
// (43 base64url characters). Use it to reject garbage before a lookup; it
// says nothing about validity.
func WellFormed(raw string) bool {
	if len(raw) != EncodedLen {
		return false
	}
	_, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	return err == nil
}

// Equal compares two secret values (hashes, MACs, codes) in constant time.
// Never use ==, bytes.Equal or strings.EqualFold for secrets.
func Equal(a, b []byte) bool { return subtle.ConstantTimeCompare(a, b) == 1 }

// Verify reports whether raw hashes to want, in constant time.
func Verify(raw string, want []byte) bool { return Equal(Hash(raw), want) }
