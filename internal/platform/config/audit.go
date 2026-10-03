package config

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"path/filepath"

	"koro/internal/platform/crypto"
)

// Audit configures the audit module (internal/audit): the daily root
// signing keys and the separate store of the signed roots (02 §2.2).
// Owner: BE-007. Only tickets of the audit module edit this file and the
// "# --- audit ---" block of .env.example (BE-001 R7).
//
// Both are required in staging and prod (a missing key stops the process,
// 05 §12.2). In dev they may be empty: audit.daily_root then fails loudly
// (audit.signer_not_configured) instead of signing with a made-up key.
// OpenBao Transit replaces the local keys with BE-055 (T-82).
type Audit struct {
	// RootSigningKeys are Ed25519 seeds in keyring format
	// "id:base64(32 bytes)[,id:base64...]"; the first signs, the others
	// only verify (rotation). Generate: openssl rand -base64 32.
	RootSigningKeys Secret `env:"AUDIT_ROOT_SIGNING_KEYS"`
	// RootArchiveDir is the write-once directory (a volume separate from
	// the database) that receives a copy of every daily root. Object-locked
	// storage is an OPS task (H-02).
	RootArchiveDir string `env:"AUDIT_ROOT_ARCHIVE_DIR"`
}

// RootKeys parses RootSigningKeys (nil when unset).
func (a Audit) RootKeys() ([]crypto.Key, error) {
	if a.RootSigningKeys.IsZero() {
		return nil, nil
	}
	return crypto.ParseKeyring(a.RootSigningKeys.Reveal())
}

func (a *Audit) validate(c Common) error {
	var errs []error
	keys, err := a.RootKeys()
	switch {
	case err != nil:
		errs = append(errs, fmt.Errorf("AUDIT_ROOT_SIGNING_KEYS: %w", err))
	case len(keys) == 0 && c.ProdLike():
		errs = append(errs, fmt.Errorf("AUDIT_ROOT_SIGNING_KEYS is required in %s", c.Env))
	}
	for _, k := range keys {
		switch {
		case len(k.Secret) != ed25519.SeedSize:
			errs = append(errs, fmt.Errorf("AUDIT_ROOT_SIGNING_KEYS: key %q must be a %d-byte Ed25519 seed", k.ID, ed25519.SeedSize))
		case c.ProdLike() && weakSecret(k.Secret):
			errs = append(errs, fmt.Errorf("AUDIT_ROOT_SIGNING_KEYS: key %q looks like a placeholder in staging/prod", k.ID))
		}
	}
	switch {
	case a.RootArchiveDir == "" && c.ProdLike():
		errs = append(errs, fmt.Errorf("AUDIT_ROOT_ARCHIVE_DIR is required in %s", c.Env))
	case a.RootArchiveDir != "" && !filepath.IsAbs(a.RootArchiveDir):
		errs = append(errs, errors.New("AUDIT_ROOT_ARCHIVE_DIR must be an absolute path"))
	}
	return errors.Join(errs...)
}
