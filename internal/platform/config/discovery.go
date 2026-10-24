package config

import (
	"errors"
	"fmt"

	"koro/internal/platform/crypto"
)

// Discovery configures the discovery module (internal/discovery). Only
// tickets of the discovery module edit this file and the
// "# --- discovery ---" block of .env.example (BE-001 R7).
//
// The pseudonyms (anon_id) and the session seeds of the ranking are
// derived from discovery's own keyring (BE-102, T-329), under the
// "discovery.anon_id" and "discovery.session_seed" purposes: a rotation of
// CRYPTO_TOKEN_KEYS no longer renames anyone. The limits of a ranking are
// code constants (students.MaxDiscoveryMatches).
//
// Rotation policy (the renewal of the pseudonyms, 01 §4.16): the new key
// goes first, the previous one stays second for the renewal window (30
// days) so the pseudonyms a company kept still resolve, then it is
// removed. Hence at most DiscoveryAnonMaxKeys keys: the active one and the
// previous generation (Resolve derives the pseudonym under every key for
// every candidate, so the ring also bounds that work).
//
// Required in staging and prod (a missing key stops the process, 05
// §12.2). In dev it may be empty: discovery then derives from
// CRYPTO_TOKEN_KEYS as before BE-102 and logs a warning.
type Discovery struct {
	// AnonKeys keys the pseudonyms and session seeds: keyring
	// "id:base64[,id:base64]" of ≥ 32-byte keys, the first active (new
	// answers), the second only resolves. Generate: openssl rand -base64 32.
	// Staging/prod: DISCOVERY_ANON_KEYS_FILE=/run/secrets/discovery_anon_keys.
	AnonKeys Secret `env:"DISCOVERY_ANON_KEYS"`
}

// DiscoveryAnonMaxKeys bounds DISCOVERY_ANON_KEYS: the active key and the
// previous one during its renewal window.
const DiscoveryAnonMaxKeys = 2

// AnonKeyring parses AnonKeys (nil when unset).
func (d Discovery) AnonKeyring() ([]crypto.Key, error) {
	if d.AnonKeys.IsZero() {
		return nil, nil
	}
	return crypto.ParseKeyring(d.AnonKeys.Reveal())
}

func (d *Discovery) validate(c Common) error {
	keys, err := d.AnonKeyring()
	if err == nil && len(keys) > 0 {
		_, err = crypto.NewDeriver(keys) // sizes and duplicate ids
	}
	var errs []error
	switch {
	case err != nil:
		errs = append(errs, fmt.Errorf("DISCOVERY_ANON_KEYS: %w", err))
	case len(keys) == 0 && c.ProdLike():
		errs = append(errs, fmt.Errorf("DISCOVERY_ANON_KEYS is required in %s (T-329: pseudonyms have their own key)", c.Env))
	case len(keys) > DiscoveryAnonMaxKeys:
		errs = append(errs, fmt.Errorf("DISCOVERY_ANON_KEYS: at most %d keys (the active one and the previous one "+
			"during its renewal window)", DiscoveryAnonMaxKeys))
	}
	if err == nil {
		for _, k := range keys {
			if c.ProdLike() && weakSecret(k.Secret) {
				errs = append(errs, fmt.Errorf("DISCOVERY_ANON_KEYS: key %q looks like a placeholder in staging/prod", k.ID))
			}
		}
	}
	return errors.Join(errs...)
}
