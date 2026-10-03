package config

import (
	"errors"
	"fmt"

	"koro/internal/platform/crypto"
)

// Crypto configures envelope encryption, blind indexes and derived tokens
// (internal/platform/crypto). Until OpenBao Transit arrives (BE-055, T-82)
// the KEKs are local and come from a Docker secret file in staging/prod
// (CRYPTO_KEKS_FILE=/run/secrets/crypto_keks). A missing key stops the
// process (05 §12.2). Owner: BE-003.
//
// Keyring format: "id:base64[,id:base64...]" (commas or newlines); the first
// entry is active, the others only decrypt/verify (rotation). Generate a
// key with `openssl rand -base64 32`.
type Crypto struct {
	// KEKs wrap the per-value data keys (32-byte keys).
	KEKs Secret `env:"CRYPTO_KEKS,required,notEmpty"`
	// BlindIndexKey keys the *_bidx columns (base64, ≥ 32 bytes). It cannot
	// rotate without recomputing every index.
	BlindIndexKey Secret `env:"CRYPTO_BLIND_INDEX_KEY,required,notEmpty"`
	// TokenKeys key derived tokens such as QR codes (≥ 32-byte keys).
	TokenKeys Secret `env:"CRYPTO_TOKEN_KEYS,required,notEmpty"`
}

func (s *Crypto) validate(c Common) error {
	var errs []error
	check := func(name string, keys []crypto.Key, err error) {
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			return
		}
		for _, k := range keys {
			if c.ProdLike() && weakSecret(k.Secret) {
				errs = append(errs, fmt.Errorf("%s: key %q looks like a placeholder in staging/prod", name, k.ID))
			}
		}
	}

	keks, err := crypto.ParseKeyring(s.KEKs.Reveal())
	if err == nil {
		_, err = crypto.NewLocalKEK(keks)
	}
	check("CRYPTO_KEKS", keks, err)

	idx, err := crypto.ParseKey(s.BlindIndexKey.Reveal())
	if err == nil {
		_, err = crypto.NewIndexer(idx)
	}
	check("CRYPTO_BLIND_INDEX_KEY", []crypto.Key{{ID: "blind-index", Secret: idx}}, err)

	tok, err := crypto.ParseKeyring(s.TokenKeys.Reveal())
	if err == nil {
		_, err = crypto.NewDeriver(tok)
	}
	check("CRYPTO_TOKEN_KEYS", tok, err)
	return errors.Join(errs...)
}

// weakSecret flags low-diversity values such as the .env.example
// placeholders ("devdevdev…"): a random 32-byte key has about 30 distinct
// bytes, so fewer than 16 means it was not generated.
func weakSecret(b []byte) bool {
	var seen [256]bool
	n := 0
	for _, c := range b {
		if !seen[c] {
			seen[c] = true
			n++
		}
	}
	return n < 16
}
