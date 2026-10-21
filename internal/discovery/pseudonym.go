package discovery

import (
	"crypto/subtle"
	"encoding/base32"
	"errors"
	"fmt"
	"slices"
	"uuid"

	"koro/internal/platform/crypto"
)

// Purposes of the derived tokens of discovery (crypto.Deriver: independent
// HKDF subkeys of the token keyring).
const (
	anonPurpose = "discovery.anon_id"
	seedPurpose = "discovery.session_seed"
)

// AnonIDLen is the length of a pseudonym: 16 base32 characters (80 bits;
// 01 §4.16).
const AnonIDLen = 16

// anonEncoding is lower-case RFC 4648 base32 without padding (URL safe).
var anonEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// Pseudonyms derives the company-scoped pseudonyms of level 2 and the
// session seeds of the ranking from the token keyring (CRYPTO_TOKEN_KEYS):
// anon_id = base32(HMAC(k_discovery, company_org_id ‖ user_id))[:16] with
// k_discovery the "discovery.anon_id" subkey of the active key. A pseudonym
// derived under an older key of the ring still resolves (rotation), new
// answers use the active key.
type Pseudonyms struct {
	d    *crypto.Deriver
	keys []string // active first
}

// NewPseudonyms returns the pseudonyms of d. keyIDs are the ids of d's
// keyring; the first must be d's active key.
func NewPseudonyms(d *crypto.Deriver, keyIDs []string) (*Pseudonyms, error) {
	switch {
	case d == nil:
		return nil, errors.New("discovery: no token deriver")
	case len(keyIDs) == 0 || keyIDs[0] != d.ActiveKeyID():
		return nil, errors.New("discovery: the first key id must be the deriver's active key")
	}
	p := &Pseudonyms{d: d, keys: slices.Clone(keyIDs)}
	for _, k := range p.keys { // every id must belong to the ring
		if _, err := p.derive(k, uuid.Nil(), uuid.Nil()); err != nil {
			return nil, fmt.Errorf("discovery: key %q: %w", k, err)
		}
	}
	return p, nil
}

// AnonID returns userID's pseudonym for companyOrgID under the active key.
func (p *Pseudonyms) AnonID(companyOrgID, userID uuid.UUID) (string, error) {
	return p.derive(p.keys[0], companyOrgID, userID)
}

func (p *Pseudonyms) derive(keyID string, companyOrgID, userID uuid.UUID) (string, error) {
	b, err := p.d.DerivedToken(anonPurpose, keyID, companyOrgID[:], userID[:])
	if err != nil {
		return "", err
	}
	return anonEncoding.EncodeToString(b)[:AnonIDLen], nil
}

// Matches reports whether anonID is userID's pseudonym for companyOrgID
// under any key of the ring (constant-time comparison).
func (p *Pseudonyms) Matches(anonID string, companyOrgID, userID uuid.UUID) (bool, error) {
	for _, k := range p.keys {
		want, err := p.derive(k, companyOrgID, userID)
		if err != nil {
			return false, err
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(anonID)) == 1 {
			return true, nil
		}
	}
	return false, nil
}

// Seed returns the ranking seed of a session for a company: stable for the
// session (the same order on every page and every repeated query), another
// one after a new sign-in. It never leaves the server; cursors carry only
// a digest of it (page.Fingerprint).
func (p *Pseudonyms) Seed(sessionID, companyOrgID uuid.UUID) ([]byte, error) {
	return p.d.DerivedToken(seedPurpose, p.keys[0], sessionID[:], companyOrgID[:])
}

// ValidAnonID reports whether s has the form of a pseudonym (16
// characters of the base32 alphabet); anything else is never found.
func ValidAnonID(s string) bool {
	if len(s) != AnonIDLen {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '2' || c > '7') {
			return false
		}
	}
	return true
}
