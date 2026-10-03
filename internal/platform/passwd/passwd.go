// Package passwd hashes and verifies passwords (05 §14.1).
//
//   - argon2id, m = 19 MiB, t = 2, p = 1, 16-byte salt, 32-byte key (OWASP
//     minimum), stored as a PHC string: $argon2id$v=19$m=19456,t=2,p=1$salt$key.
//     Verification reads the parameters from the string (bounded, so a
//     tampered hash cannot exhaust memory) and reports needsRehash when they
//     differ from the current ones.
//   - Pepper: the password is NFKC-normalized and mixed with HMAC-SHA256
//     under a ≥ 32-byte pepper that lives outside the database (Docker
//     secret PASSWORD_PEPPER). This also bounds the argon2 input length.
//   - A semaphore limits concurrent computations (each needs 19 MiB).
//   - VerifyDummy burns the same time for unknown users, so response timing
//     does not reveal which e-mail addresses exist.
//
// Password policy (length, breached list, lockout) belongs to identity.
// The package is a leaf (standard library, golang.org/x/crypto, x/text).
package passwd

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
	"golang.org/x/text/unicode/norm"
)

// Current parameters (05 §14.1).
const (
	MemoryKiB = 19 * 1024
	Time      = 2
	Threads   = 1
	KeyLen    = 32
	SaltLen   = 16

	// MinPepperLen is the minimum pepper size in bytes.
	MinPepperLen = 32

	// Bounds for parameters read from a stored hash.
	maxMemoryKiB = 256 * 1024
	maxTime      = 10
	maxThreads   = 8
	minSaltLen   = 8
	maxSaltLen   = 64
	minKeyLen    = 16
	maxKeyLen    = 64
)

// Errors.
var (
	ErrInvalidHash = errors.New("passwd: invalid hash encoding")
	ErrConfig      = errors.New("passwd: invalid configuration")
)

var b64 = base64.RawStdEncoding.Strict()

// Hasher hashes and verifies passwords. It is safe for concurrent use.
type Hasher struct {
	pepper []byte
	sem    chan struct{}
	dummy  string
}

// New returns a Hasher. pepper must be at least MinPepperLen bytes and
// maxConcurrent at least 1 (memory bound: maxConcurrent × 19 MiB).
func New(pepper []byte, maxConcurrent int) (*Hasher, error) {
	if len(pepper) < MinPepperLen {
		return nil, fmt.Errorf("%w: pepper must be at least %d bytes", ErrConfig, MinPepperLen)
	}
	if maxConcurrent < 1 {
		return nil, fmt.Errorf("%w: maxConcurrent must be at least 1", ErrConfig)
	}
	h := &Hasher{pepper: append([]byte(nil), pepper...), sem: make(chan struct{}, maxConcurrent)}
	dummy := make([]byte, SaltLen)
	if _, err := rand.Read(dummy); err != nil {
		return nil, fmt.Errorf("passwd: dummy: %w", err)
	}
	enc, err := h.hashWith(b64.EncodeToString(dummy), MemoryKiB, Time, Threads)
	if err != nil {
		return nil, err
	}
	h.dummy = enc
	return h, nil
}

// Hash returns the PHC-encoded argon2id hash of pw with the current
// parameters.
func (h *Hasher) Hash(ctx context.Context, pw string) (string, error) {
	if err := h.acquire(ctx); err != nil {
		return "", err
	}
	defer h.release()
	return h.hashWith(pw, MemoryKiB, Time, Threads)
}

// Verify reports whether pw matches encoded. needsRehash is true when the
// password matched but encoded uses other parameters than the current ones:
// the caller then stores a fresh Hash. A malformed or out-of-bounds encoded
// value returns ErrInvalidHash.
func (h *Hasher) Verify(ctx context.Context, pw, encoded string) (ok, needsRehash bool, err error) {
	p, err := parse(encoded)
	if err != nil {
		return false, false, err
	}
	if err := h.acquire(ctx); err != nil {
		return false, false, err
	}
	defer h.release()
	got := argon2.IDKey(h.prehash(pw), p.salt, p.time, p.memory, p.threads, uint32(len(p.key))) //nolint:gosec // parse bounds the key to 64 bytes
	ok = subtle.ConstantTimeCompare(got, p.key) == 1
	return ok, ok && !p.current(), nil
}

// VerifyDummy performs a verification against an internal hash with the
// current parameters and discards the result. Call it when the user does
// not exist so that the response takes as long as a real verification.
func (h *Hasher) VerifyDummy(ctx context.Context, pw string) error {
	_, _, err := h.Verify(ctx, pw, h.dummy)
	return err
}

// NeedsRehash reports whether encoded was produced with parameters other
// than the current ones (or cannot be parsed).
func NeedsRehash(encoded string) bool {
	p, err := parse(encoded)
	return err != nil || !p.current()
}

func (h *Hasher) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case h.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Hasher) release() { <-h.sem }

// prehash = HMAC-SHA256(pepper, NFKC(pw)).
func (h *Hasher) prehash(pw string) []byte {
	m := hmac.New(sha256.New, h.pepper)
	m.Write([]byte(norm.NFKC.String(pw)))
	return m.Sum(nil)
}

func (h *Hasher) hashWith(pw string, memory, time uint32, threads uint8) (string, error) {
	salt := make([]byte, SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("passwd: salt: %w", err)
	}
	key := argon2.IDKey(h.prehash(pw), salt, time, memory, threads, KeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, memory, time, threads, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

type params struct {
	memory, time uint32
	threads      uint8
	salt, key    []byte
}

func (p params) current() bool {
	return p.memory == MemoryKiB && p.time == Time && p.threads == Threads &&
		len(p.salt) == SaltLen && len(p.key) == KeyLen
}

// parse reads a PHC argon2id string strictly: canonical numbers only, no
// padding, parameters within bounds.
func parse(encoded string) (params, error) {
	var p params
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" ||
		parts[2] != "v="+strconv.Itoa(argon2.Version) {
		return p, ErrInvalidHash
	}
	fields := strings.Split(parts[3], ",")
	if len(fields) != 3 {
		return p, ErrInvalidHash
	}
	var vals [3]uint64
	for i, f := range fields {
		name, val, ok := strings.Cut(f, "=")
		n, err := strconv.ParseUint(val, 10, 32)
		if !ok || err != nil || name != "mtp"[i:i+1] || strconv.FormatUint(n, 10) != val {
			return p, ErrInvalidHash
		}
		vals[i] = n
	}
	m, t, th := vals[0], vals[1], vals[2]
	if t < 1 || t > maxTime || th < 1 || th > maxThreads || m < 8*th || m > maxMemoryKiB {
		return p, ErrInvalidHash
	}
	salt, err := b64.DecodeString(parts[4])
	if err != nil || len(salt) < minSaltLen || len(salt) > maxSaltLen {
		return p, ErrInvalidHash
	}
	key, err := b64.DecodeString(parts[5])
	if err != nil || len(key) < minKeyLen || len(key) > maxKeyLen {
		return p, ErrInvalidHash
	}
	return params{memory: uint32(m), time: uint32(t), threads: uint8(th), salt: salt, key: key}, nil
}
