package consent

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"math"
	"time"
	"uuid"

	"koro/internal/platform/crypto"
)

// Signed unsubscribe links of the BE-062 format, still in the mailboxes.
// BE-153 mints opaque links only (unsubscribe.go); these are verified until
// their own expiry, at most CONSENT_UNSUBSCRIBE_TTL (≤ 400 days) after
// BE-153 shipped. Then this file goes (00 T-395), with the Deriver of
// UnsubscribeConfig and the literal tokens of the authz matrix.
//
// The token is an HMAC (crypto.Deriver over CRYPTO_TOKEN_KEYS, subkey of
// the purpose consent.unsubscribe, so it never verifies as another kind of
// derived token) over
//
//	version (1) | purpose tag (1) | user ID (16) | expiry, Unix seconds (8) | key ID length (1) | key ID
//
// followed by the 32-byte MAC, base64url without padding: 80 to 164
// characters, never the 43 of an opaque token. It carries the user ID in
// clear (Q-36: the reason for BE-153). Rotating CRYPTO_TOKEN_KEYS keeps the
// links verifiable while their key stays in the keyring.

const (
	// signedDerive is the crypto.Deriver purpose of the MAC subkey.
	signedDerive = "consent.unsubscribe"
	// signedVersion is the token layout.
	signedVersion byte = 1
	// signedFixed is the fixed head: version, tag, user ID, expiry.
	signedFixed = 1 + 1 + 16 + 8
	signedMAC   = 32
	maxKeyID    = 64 // crypto.ParseKeyring IDs
)

// signedTags are the purposes a signed link withdraws.
var signedTags = map[byte]Purpose{1: PurposeCommercialEmail}

// verifySigned checks a signed token and returns its claim, or the reason
// it is refused ("" on success). raw is bounded and free of line breaks
// (resolveUnsubscribe). The MAC is checked before anything in the token is
// trusted, in constant time.
func verifySigned(d *crypto.Deriver, raw string, now time.Time) (unsubscribeClaim, string) {
	if d == nil {
		return unsubscribeClaim{}, "not_configured"
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	if err != nil || len(b) < signedFixed+2+signedMAC {
		return unsubscribeClaim{}, "malformed"
	}
	n := int(b[signedFixed])
	if n == 0 || n > maxKeyID || len(b) != signedFixed+1+n+signedMAC {
		return unsubscribeClaim{}, "malformed"
	}
	head, mac := b[:signedFixed+1+n], b[signedFixed+1+n:]
	want, err := d.DerivedToken(signedDerive, string(head[signedFixed+1:]), head)
	if err != nil {
		return unsubscribeClaim{}, "unknown_key"
	}
	if subtle.ConstantTimeCompare(mac, want) != 1 {
		return unsubscribeClaim{}, "bad_mac"
	}
	// Signed by us from here on.
	if head[0] != signedVersion {
		return unsubscribeClaim{}, "version"
	}
	purpose, ok := signedTags[head[1]]
	if !ok {
		return unsubscribeClaim{}, "purpose"
	}
	var uid uuid.UUID
	copy(uid[:], head[2:18])
	exp := binary.BigEndian.Uint64(head[18:signedFixed])
	switch {
	case uid == uuid.Nil():
		return unsubscribeClaim{}, "malformed"
	case exp > math.MaxInt64 || !now.Before(time.Unix(int64(exp), 0)):
		return unsubscribeClaim{}, "expired"
	}
	return unsubscribeClaim{UserID: uid, Purpose: purpose, signed: true}, ""
}
