package audit

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sync"

	"koro/internal/platform/crypto"
)

// RootVerifier verifies the signatures of stored daily roots. It needs no
// private key: the API verifies with Ed25519Verifier (public keys from
// AUDIT_ROOT_VERIFY_KEYS) and never holds the seeds (BE-107).
type RootVerifier interface {
	// Verify checks sig of msg under keyID (an old key after rotation).
	Verify(ctx context.Context, keyID string, msg, sig []byte) (bool, error)
}

// RootSigner signs daily roots and verifies the signatures of stored ones.
// Implementations: Ed25519Signer (local keys from AUDIT_ROOT_SIGNING_KEYS,
// worker only); OpenBao Transit with BE-055 (02 §2.2).
type RootSigner interface {
	RootVerifier
	// Sign signs msg with the active key and returns the key ID with it.
	Sign(ctx context.Context, msg []byte) (sig []byte, keyID string, err error)
}

// Ed25519Signer signs with local Ed25519 keys. keys[0] is active; the
// others only verify (rotation). Each secret is a 32-byte seed.
type Ed25519Signer struct {
	active string
	keys   map[string]ed25519.PrivateKey
}

// NewEd25519Signer returns a signer over keys (crypto.ParseKeyring format).
func NewEd25519Signer(keys []crypto.Key) (*Ed25519Signer, error) {
	if len(keys) == 0 {
		return nil, errors.New("audit: no root signing key")
	}
	s := &Ed25519Signer{active: keys[0].ID, keys: make(map[string]ed25519.PrivateKey, len(keys))}
	for i, k := range keys {
		if len(k.Secret) != ed25519.SeedSize {
			return nil, fmt.Errorf("audit: root signing key %d (%s) must be a %d-byte Ed25519 seed", i+1, k.ID, ed25519.SeedSize)
		}
		if _, dup := s.keys[k.ID]; dup {
			return nil, fmt.Errorf("audit: root signing key id %s is repeated", k.ID)
		}
		s.keys[k.ID] = ed25519.NewKeyFromSeed(k.Secret)
	}
	return s, nil
}

// Sign implements RootSigner.
func (s *Ed25519Signer) Sign(_ context.Context, msg []byte) ([]byte, string, error) {
	return ed25519.Sign(s.keys[s.active], msg), s.active, nil
}

// Verify implements RootSigner. An unknown key ID is an invalid signature.
func (s *Ed25519Signer) Verify(_ context.Context, keyID string, msg, sig []byte) (bool, error) {
	k, ok := s.keys[keyID]
	if !ok {
		return false, nil
	}
	pub, _ := k.Public().(ed25519.PublicKey)
	return ed25519.Verify(pub, msg, sig), nil
}

// Ed25519Verifier verifies root signatures with Ed25519 public keys only.
type Ed25519Verifier struct {
	keys map[string]ed25519.PublicKey
}

// NewEd25519Verifier returns a verifier over keys (crypto.ParseKeyring
// format); each secret is a 32-byte Ed25519 public key.
func NewEd25519Verifier(keys []crypto.Key) (*Ed25519Verifier, error) {
	if len(keys) == 0 {
		return nil, errors.New("audit: no root verify key")
	}
	v := &Ed25519Verifier{keys: make(map[string]ed25519.PublicKey, len(keys))}
	for i, k := range keys {
		if len(k.Secret) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("audit: root verify key %d (%s) must be a %d-byte Ed25519 public key", i+1, k.ID, ed25519.PublicKeySize)
		}
		if _, dup := v.keys[k.ID]; dup {
			return nil, fmt.Errorf("audit: root verify key id %s is repeated", k.ID)
		}
		v.keys[k.ID] = ed25519.PublicKey(bytes.Clone(k.Secret))
	}
	return v, nil
}

// Verify implements RootVerifier. An unknown key ID is an invalid signature.
func (v *Ed25519Verifier) Verify(_ context.Context, keyID string, msg, sig []byte) (bool, error) {
	pub, ok := v.keys[keyID]
	if !ok {
		return false, nil
	}
	return ed25519.Verify(pub, msg, sig), nil
}

// CheckCovers reports an error unless v verifies with exactly the public
// key of every signing key of s. A worker whose seeds do not match the
// API's public keys would sign roots that the API reports as tampered, so
// the mismatch stops the worker at start (BE-107). Errors name key IDs only.
func (v *Ed25519Verifier) CheckCovers(s *Ed25519Signer) error {
	var errs []error
	for id, k := range s.keys {
		pub, _ := k.Public().(ed25519.PublicKey)
		switch got, ok := v.keys[id]; {
		case !ok:
			errs = append(errs, fmt.Errorf("audit: signing key %s has no AUDIT_ROOT_VERIFY_KEYS entry", id))
		case !got.Equal(pub):
			errs = append(errs, fmt.Errorf("audit: AUDIT_ROOT_VERIFY_KEYS entry %s is not the public key of signing key %s", id, id))
		}
	}
	return errors.Join(errs...)
}

// RootArchive is the separate store that keeps a copy of every daily root
// (02 §2.2 external_ref). It is write-once: Put of an existing key
// succeeds only with identical content (a retried job), otherwise
// ErrArchiveConflict. Production storage with object lock (S3) is OPS work
// (H-02); DirArchive serves dev/staging and the pilot volume.
type RootArchive interface {
	Put(ctx context.Context, key string, data []byte) (ref string, err error)
}

// archiveKeyRe guards the object keys the module writes.
var archiveKeyRe = regexp.MustCompile(`^audit-roots/[0-9]{4}-[0-9]{2}-[0-9]{2}\.json$`)

// DirArchive writes each root to a read-only file under a directory that
// is not the database's (a mounted volume). Existing files are never
// overwritten.
type DirArchive struct{ dir string }

// NewDirArchive returns an archive rooted at the absolute path dir. The
// directory is created on the first Put (only the worker writes roots).
func NewDirArchive(dir string) (*DirArchive, error) {
	if dir == "" || !filepath.IsAbs(dir) {
		return nil, errors.New("audit: archive directory must be an absolute path")
	}
	return &DirArchive{dir: filepath.Clean(dir)}, nil
}

// Put implements RootArchive.
func (a *DirArchive) Put(_ context.Context, key string, data []byte) (string, error) {
	if !archiveKeyRe.MatchString(key) {
		return "", fmt.Errorf("audit: archive key %q", key)
	}
	path := filepath.Join(a.dir, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return "", fmt.Errorf("audit: archive directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o440) //nolint:gosec // G304: key is validated by archiveKeyRe
	if errors.Is(err, fs.ErrExist) {
		old, rerr := os.ReadFile(path)
		if rerr != nil {
			return "", fmt.Errorf("audit: archive read %s: %w", key, rerr)
		}
		if !bytes.Equal(old, data) {
			return "", ErrArchiveConflict
		}
		return key, nil
	}
	if err != nil {
		return "", fmt.Errorf("audit: archive create %s: %w", key, err)
	}
	_, werr := f.Write(data)
	serr := f.Sync()
	if err := errors.Join(werr, serr, f.Close()); err != nil {
		_ = os.Remove(path) // a partial file would block the retry
		return "", fmt.Errorf("audit: archive write %s: %w", key, err)
	}
	return key, nil
}

// MemArchive is an in-memory RootArchive for tests and wiring.
type MemArchive struct {
	mu   sync.Mutex
	objs map[string][]byte
}

// NewMemArchive returns an empty archive.
func NewMemArchive() *MemArchive { return &MemArchive{objs: map[string][]byte{}} }

// Put implements RootArchive.
func (a *MemArchive) Put(_ context.Context, key string, data []byte) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if old, ok := a.objs[key]; ok {
		if !bytes.Equal(old, data) {
			return "", ErrArchiveConflict
		}
		return key, nil
	}
	a.objs[key] = bytes.Clone(data)
	return key, nil
}

// Get returns the object stored under key.
func (a *MemArchive) Get(key string) ([]byte, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	b, ok := a.objs[key]
	return bytes.Clone(b), ok
}
