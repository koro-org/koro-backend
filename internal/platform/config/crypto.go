package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"koro/internal/platform/clock"
	"koro/internal/platform/crypto"
	"koro/internal/platform/crypto/openbao"
)

// Crypto configures envelope encryption, blind indexes and derived tokens
// (internal/platform/crypto). Owners: BE-003, BE-055.
//
// Key-encryption keys (KVKK 2018/10: keys apart from the data):
//
//   - CRYPTO_OPENBAO_ADDR set: new values are wrapped by OpenBao Transit
//     (BE-055). CRYPTO_KEKS is then optional and only opens values sealed
//     before the switch; crypto.rewrap moves them to OpenBao, after which
//     the local KEK can be removed from the server.
//   - otherwise the local KEKs (CRYPTO_KEKS, a Docker secret file in
//     staging/prod: CRYPTO_KEKS_FILE=/run/secrets/crypto_keks; T-82) wrap.
//
// In prod OpenBao is required unless CRYPTO_OPENBAO_REQUIRED=false says
// otherwise explicitly. A missing key stops the process (05 §12.2).
//
// Keyring format: "id:base64[,id:base64...]" (commas or newlines); the first
// entry is active, the others only decrypt/verify (rotation). Generate a
// key with `openssl rand -base64 32`. Local KEK ids must not start with
// "bao." (the OpenBao key ids).
type Crypto struct {
	// KEKs wrap the per-value data keys (32-byte keys). Required without
	// OpenBao.
	KEKs Secret `env:"CRYPTO_KEKS"`
	// BlindIndexKey keys the *_bidx columns (base64, ≥ 32 bytes). It cannot
	// rotate without recomputing every index.
	BlindIndexKey Secret `env:"CRYPTO_BLIND_INDEX_KEY,required,notEmpty"`
	// TokenKeys key derived tokens such as QR codes (≥ 32-byte keys).
	TokenKeys Secret `env:"CRYPTO_TOKEN_KEYS,required,notEmpty"`

	// OpenBaoAddr is the OpenBao address (https in staging/prod); empty →
	// local KEKs only.
	OpenBaoAddr string `env:"CRYPTO_OPENBAO_ADDR"`
	// OpenBaoToken is a periodic token with the Transit policy of package
	// openbao (CRYPTO_OPENBAO_TOKEN_FILE in staging/prod).
	OpenBaoToken Secret `env:"CRYPTO_OPENBAO_TOKEN"`
	// OpenBaoMount and OpenBaoKey name the Transit mount and key.
	OpenBaoMount string `env:"CRYPTO_OPENBAO_TRANSIT_MOUNT" envDefault:"transit"`
	OpenBaoKey   string `env:"CRYPTO_OPENBAO_KEY" envDefault:"koro-kek"`
	// OpenBaoCACert is the PEM CA of a private OpenBao certificate
	// (CRYPTO_OPENBAO_CA_CERT_FILE); empty → system roots.
	OpenBaoCACert string `env:"CRYPTO_OPENBAO_CA_CERT"`
	// OpenBaoTimeout bounds one OpenBao request.
	OpenBaoTimeout time.Duration `env:"CRYPTO_OPENBAO_TIMEOUT" envDefault:"5s"`
	// OpenBaoRequired: "true" refuses to start without OpenBao, "false"
	// allows the local KEK; empty means true in prod, false elsewhere.
	OpenBaoRequired string `env:"CRYPTO_OPENBAO_REQUIRED"`
}

// OpenBaoEnabled reports whether new values are wrapped by OpenBao.
func (s Crypto) OpenBaoEnabled() bool { return strings.TrimSpace(s.OpenBaoAddr) != "" }

// OpenBaoConfig returns the Transit settings (OpenBaoEnabled only).
func (s Crypto) OpenBaoConfig(clk clock.Clock) openbao.Config {
	return openbao.Config{
		Addr: strings.TrimSpace(s.OpenBaoAddr), Token: s.OpenBaoToken.Reveal(), Mount: s.OpenBaoMount, Key: s.OpenBaoKey,
		CACertPEM: []byte(strings.TrimSpace(s.OpenBaoCACert)), Timeout: s.OpenBaoTimeout, Clock: clk,
	}
}

// openBaoRequired resolves CRYPTO_OPENBAO_REQUIRED.
func (s Crypto) openBaoRequired(c Common) (bool, error) {
	switch v := strings.TrimSpace(s.OpenBaoRequired); v {
	case "":
		return c.Env == EnvProd, nil
	default:
		b, err := strconv.ParseBool(v)
		if err != nil {
			return false, errors.New("CRYPTO_OPENBAO_REQUIRED must be true or false")
		}
		return b, nil
	}
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

	switch {
	case !s.KEKs.IsZero():
		keks, err := crypto.ParseKeyring(s.KEKs.Reveal())
		if err == nil {
			_, err = crypto.NewLocalKEK(keks)
		}
		for _, k := range keks {
			if strings.HasPrefix(k.ID, openbao.KeyIDPrefix) {
				err = errors.Join(err, fmt.Errorf("key id %q uses the reserved prefix %q", k.ID, openbao.KeyIDPrefix))
			}
		}
		check("CRYPTO_KEKS", keks, err)
	case !s.OpenBaoEnabled():
		errs = append(errs, errors.New("CRYPTO_KEKS is required unless CRYPTO_OPENBAO_ADDR is set"))
	}

	required, err := s.openBaoRequired(c)
	if err != nil {
		errs = append(errs, err)
	}
	if s.OpenBaoEnabled() {
		errs = append(errs, s.validateOpenBao(c)...)
	} else if required {
		errs = append(errs, fmt.Errorf("CRYPTO_OPENBAO_ADDR is required in %s (KVKK: keys are kept apart from the data); "+
			"set CRYPTO_OPENBAO_REQUIRED=false to run on the local KEK", c.Env))
	}

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

func (s Crypto) validateOpenBao(c Common) []error {
	var errs []error
	if _, err := openbao.New(s.OpenBaoConfig(clock.Real())); err != nil {
		errs = append(errs, fmt.Errorf("CRYPTO_OPENBAO_*: %w", err))
	}
	if c.ProdLike() {
		if !strings.HasPrefix(strings.TrimSpace(s.OpenBaoAddr), "https://") {
			errs = append(errs, fmt.Errorf("CRYPTO_OPENBAO_ADDR must use https in %s", c.Env))
		}
		if weakSecret([]byte(s.OpenBaoToken.Reveal())) {
			errs = append(errs, fmt.Errorf("CRYPTO_OPENBAO_TOKEN looks like a placeholder in %s", c.Env))
		}
	}
	if s.OpenBaoTimeout <= 0 || s.OpenBaoTimeout > 30*time.Second {
		errs = append(errs, errors.New("CRYPTO_OPENBAO_TIMEOUT must be positive and at most 30s"))
	}
	return errs
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
