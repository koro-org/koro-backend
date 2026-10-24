package config

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"path/filepath"

	"koro/internal/platform/crypto"
)

// Audit configures the audit module (internal/audit) in every process that
// wires it (api, worker, seed): the public keys that verify the signed daily
// roots (02 §2.2; POST /admin/audit/verify runs in the API, audit.verify_chain
// in the worker). Owner: BE-007. Only tickets of the audit module edit this
// file and the "# --- audit ---" block of .env.example (BE-001 R7).
//
// The private signing keys and the root archive are in AuditRoot, which only
// `koro worker` loads (BE-107, least privilege): the API never signs a root,
// so it never receives the seeds.
//
// AUDIT_ROOT_VERIFY_KEYS is required in staging and prod (a missing key
// stops the process, 05 §12.2). In dev it may be empty: a verification of a
// range that has roots then fails loudly (audit.signer_not_configured).
// OpenBao Transit replaces the local keys with BE-055 (T-82).
type Audit struct {
	// RootVerifyKeys are Ed25519 public keys in keyring format
	// "id:base64(32 bytes)[,id:base64...]", one per signing key (current and
	// rotated). Not secret; .env.example shows how to derive one from a
	// seed with openssl. The worker refuses to start when a signing key has
	// no matching entry here.
	RootVerifyKeys string `env:"AUDIT_ROOT_VERIFY_KEYS"`
}

// VerifyKeys parses RootVerifyKeys (nil when unset).
func (a Audit) VerifyKeys() ([]crypto.Key, error) {
	if a.RootVerifyKeys == "" {
		return nil, nil
	}
	return crypto.ParseKeyring(a.RootVerifyKeys)
}

func (a *Audit) validate(c Common) error {
	var errs []error
	keys, err := a.VerifyKeys()
	switch {
	case err != nil:
		errs = append(errs, fmt.Errorf("AUDIT_ROOT_VERIFY_KEYS: %w", err))
	case len(keys) == 0 && c.ProdLike():
		errs = append(errs, fmt.Errorf("AUDIT_ROOT_VERIFY_KEYS is required in %s", c.Env))
	}
	for _, k := range keys {
		if len(k.Secret) != ed25519.PublicKeySize {
			errs = append(errs, fmt.Errorf("AUDIT_ROOT_VERIFY_KEYS: key %q must be a %d-byte Ed25519 public key", k.ID, ed25519.PublicKeySize))
		}
	}
	return errors.Join(errs...)
}

// AuditRoot configures the signing of the daily audit roots and their
// separate store. Only `koro worker` loads it (audit.daily_root runs there;
// BE-107): the API container gets neither the seeds nor the archive volume.
// Owner: BE-007/BE-107; same edit rule as Audit.
//
// Both are required in staging and prod (05 §12.2). In dev they may be
// empty: audit.daily_root then fails loudly (audit.signer_not_configured)
// instead of signing with a made-up key.
type AuditRoot struct {
	// RootSigningKeys are Ed25519 seeds in keyring format
	// "id:base64(32 bytes)[,id:base64...]"; the first signs, the others
	// only verify (rotation). Generate: openssl rand -base64 32.
	RootSigningKeys Secret `env:"AUDIT_ROOT_SIGNING_KEYS"`
	// RootArchiveDir is the write-once directory (a volume separate from
	// the database) that receives a copy of every daily root. Object-locked
	// storage is an OPS task (H-02).
	RootArchiveDir string `env:"AUDIT_ROOT_ARCHIVE_DIR"`
}

// AuditRootVars are the variables of AuditRoot, which `koro api` ignores
// (cmd_api.go warns when they reach an API container in staging/prod).
var AuditRootVars = []string{"AUDIT_ROOT_SIGNING_KEYS", "AUDIT_ROOT_SIGNING_KEYS_FILE", "AUDIT_ROOT_ARCHIVE_DIR"}

// RootKeys parses RootSigningKeys (nil when unset).
func (a AuditRoot) RootKeys() ([]crypto.Key, error) {
	if a.RootSigningKeys.IsZero() {
		return nil, nil
	}
	return crypto.ParseKeyring(a.RootSigningKeys.Reveal())
}

func (a *AuditRoot) validate(c Common) error {
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
