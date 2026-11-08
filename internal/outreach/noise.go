package outreach

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// noisePurpose is the crypto.Deriver purpose of the estimate offsets
// (BE-203): a subkey of its own under CRYPTO_TOKEN_KEYS, independent of
// every other token derived from those keys.
const noisePurpose = "outreach.audience_noise"

// KeyedNoise is the production Noise: the offset is read from
// HMAC-SHA256 of the seed under the active token key, so nobody without
// the key can predict or remove it. A rotation of the token keys draws new
// offsets for every estimate (05 §12.2: once a year or after a leak).
type KeyedNoise struct{ keys NoiseKeys }

var _ Noise = (*KeyedNoise)(nil)

// NewKeyedNoise returns the Noise keyed by keys; an active key is
// required (a missing key stops the process at start, 05 §12.2).
func NewKeyedNoise(keys NoiseKeys) (*KeyedNoise, error) {
	if keys == nil || keys.ActiveKeyID() == "" {
		return nil, errors.New("outreach: the estimate noise needs a token key")
	}
	return &KeyedNoise{keys: keys}, nil
}

// Offset implements Noise. The first 8 bytes of the MAC, reduced modulo
// 2·half+1, pick the offset; for the ranges outreach uses (half ≤ 500)
// the modulo bias is below 2⁻⁵³.
func (k *KeyedNoise) Offset(seed []byte, half int) (int, error) {
	switch {
	case half < 0:
		return 0, fmt.Errorf("outreach: estimate noise: negative range %d", half)
	case half == 0:
		return 0, nil
	}
	mac, err := k.keys.DerivedToken(noisePurpose, k.keys.ActiveKeyID(), seed)
	if err != nil {
		return 0, fmt.Errorf("outreach: estimate noise: %w", err)
	}
	if len(mac) < 8 {
		return 0, errors.New("outreach: estimate noise: short MAC")
	}
	span := uint64(2*half + 1)
	return int(binary.BigEndian.Uint64(mac[:8])%span) - half, nil //nolint:gosec // G115: the remainder is below 2·half+1
}
