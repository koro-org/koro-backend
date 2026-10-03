// Package crypto protects personal data at rest (01 §1.3, 02 §0.4):
//
//   - Envelope.Seal/Open: envelope encryption of `*_enc bytea` columns. Each
//     value gets a fresh 256-bit data key (DEK) and AES-256-GCM; the DEK is
//     wrapped by a KeyWrapper (KEK). The output carries the KEK id, so keys
//     can rotate and the crypto.rewrap job (subpackage rewrap) can move
//     values to the active key (Envelope.Rewrap: opened and sealed again in
//     memory, since the data AAD binds the header).
//   - Indexer.BlindIndex: deterministic HMAC-SHA256 of a normalized value for
//     equality search on encrypted columns (`*_bidx bytea`).
//   - Deriver.DerivedToken: HMAC-SHA256 tokens bound to a key id (QR codes;
//     02 §0.2 "HMAC(k, registration_id ‖ version)").
//
// Every operation is bound to a purpose (e.g. "identity.mfa_secret"): a
// ciphertext sealed for one purpose does not open under another, and blind
// indexes / derived tokens of different purposes use independent keys
// (HKDF-SHA256). Only the standard library is used; no algorithm is home
// made.
//
// Key-encryption keys (KVKK 2018/10: keys apart from the data): the KEK
// lives in OpenBao Transit (subpackage openbao, BE-055) and never leaves it;
// LocalKEK (keys from a Docker secret file, T-82) is the fallback of
// environments without OpenBao and the reader of values sealed before the
// switch. Chain combines them: new values are wrapped by the active wrapper
// only (no fallback: if the key service is down, Seal fails and nothing is
// stored), older wrappers only unwrap. Envelope.Rewrap moves a value to the
// active key (the crypto.rewrap job, subpackage rewrap). A missing key
// stops the process at start (05 §12.2).
package crypto

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Errors. Messages never contain key material or plaintext.
var (
	ErrConfig     = errors.New("crypto: invalid configuration")
	ErrPurpose    = errors.New("crypto: empty purpose")
	ErrMalformed  = errors.New("crypto: malformed ciphertext")
	ErrUnknownKey = errors.New("crypto: unknown key id")
	ErrDecrypt    = errors.New("crypto: decryption failed")
	// ErrUnavailable marks a key service that cannot serve the request
	// (unreachable, sealed, overloaded, permission denied). Callers fail
	// closed: nothing is sealed, nothing is stored in clear text.
	ErrUnavailable = errors.New("crypto: key service unavailable")
)

// KeySize is the size of AES-256 keys (KEKs and DEKs); MinMACKeySize the
// minimum size of HMAC keys.
const (
	KeySize       = 32
	MinMACKeySize = 32
)

// Key is a named secret key. IDs are not secret and are stored in
// ciphertexts and tokens.
type Key struct {
	ID     string
	Secret []byte
}

var keyIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// ParseKeyring parses "id:base64[,id:base64...]" (commas or newlines
// between entries, standard base64). The first entry is the active key.
// Errors name the entry by position and id, never the key.
func ParseKeyring(s string) ([]Key, error) {
	entries := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' })
	var keys []Key
	for i, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		id, b64, ok := strings.Cut(e, ":")
		if !ok || !keyIDRe.MatchString(id) {
			return nil, fmt.Errorf("%w: keyring entry %d: want id:base64 with id [A-Za-z0-9._-]{1,64}", ErrConfig, i+1)
		}
		secret, err := ParseKey(b64)
		if err != nil {
			return nil, fmt.Errorf("%w: keyring entry %d (%s): key is not standard base64", ErrConfig, i+1, id)
		}
		keys = append(keys, Key{ID: id, Secret: secret})
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("%w: keyring is empty", ErrConfig)
	}
	return keys, nil
}

// ParseKey decodes one standard base64 key (surrounding whitespace ignored).
func ParseKey(s string) ([]byte, error) {
	b, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("%w: key is not standard base64", ErrConfig)
	}
	return b, nil
}

// keyring checks ids and sizes and copies the secrets.
func keyring(keys []Key, minSize, maxSize int) (map[string][]byte, error) {
	if len(keys) == 0 {
		return nil, fmt.Errorf("%w: no key configured", ErrConfig)
	}
	m := make(map[string][]byte, len(keys))
	for _, k := range keys {
		if !keyIDRe.MatchString(k.ID) {
			return nil, fmt.Errorf("%w: invalid key id %q", ErrConfig, k.ID)
		}
		if _, dup := m[k.ID]; dup {
			return nil, fmt.Errorf("%w: duplicate key id %q", ErrConfig, k.ID)
		}
		if len(k.Secret) < minSize || len(k.Secret) > maxSize {
			return nil, fmt.Errorf("%w: key %q must be %d–%d bytes", ErrConfig, k.ID, minSize, maxSize)
		}
		m[k.ID] = append([]byte(nil), k.Secret...)
	}
	return m, nil
}

// lp appends a length-prefixed field (unambiguous concatenation).
func lp(dst []byte, b []byte) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(b)))
	return append(dst, b...)
}

// subkey derives an independent key for (domain, purpose).
func subkey(secret []byte, domain, purpose string) []byte {
	info := string(lp(lp(nil, []byte(domain)), []byte(purpose)))
	k, err := hkdf.Key(sha256.New, secret, nil, info, KeySize)
	if err != nil { // only for lengths > 255×32
		panic("crypto: hkdf: " + err.Error())
	}
	return k
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: %w", err)
	}
	return cipher.NewGCM(block)
}

// ---------------------------------------------------------------------------
// Key wrapping

// KeyWrapper wraps data encryption keys under a key-encryption key. Wrap
// uses the active KEK and returns its id; Unwrap selects the KEK by id. aad
// binds the wrapped key to its use (purpose) and must match on Unwrap.
// Implementations: LocalKEK (T-82), openbao.Transit (BE-055), Chain.
type KeyWrapper interface {
	Wrap(ctx context.Context, dek, aad []byte) (keyID string, wrapped []byte, err error)
	Unwrap(ctx context.Context, keyID string, wrapped, aad []byte) ([]byte, error)
}

// KeyOwner is a KeyWrapper that can tell whether it holds a key id (Chain
// routes Unwrap with it).
type KeyOwner interface {
	Owns(keyID string) bool
}

// ActiveKeyer is a KeyWrapper that knows the id Wrap uses now (the re-wrap
// job selects the values sealed under any other id).
type ActiveKeyer interface {
	ActiveKeyID(ctx context.Context) (string, error)
}

// LocalKEK wraps DEKs with AES-256-GCM under KEKs held in memory
// (CRYPTO_KEKS, a Docker secret file in staging/prod).
type LocalKEK struct {
	active string
	keys   map[string][]byte
}

var _ KeyWrapper = (*LocalKEK)(nil)

// NewLocalKEK returns a wrapper over 32-byte keys; keys[0] is active, the
// others only unwrap (rotation). No key is an error: the process must not
// start without a KEK (05 §12.2).
func NewLocalKEK(keys []Key) (*LocalKEK, error) {
	m, err := keyring(keys, KeySize, KeySize)
	if err != nil {
		return nil, err
	}
	return &LocalKEK{active: keys[0].ID, keys: m}, nil
}

// Wrap implements KeyWrapper. wrapped = nonce ‖ AES-GCM(dek).
func (l *LocalKEK) Wrap(_ context.Context, dek, aad []byte) (string, []byte, error) {
	g, err := newGCM(l.keys[l.active])
	if err != nil {
		return "", nil, err
	}
	nonce := make([]byte, g.NonceSize(), g.NonceSize()+len(dek)+g.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return "", nil, fmt.Errorf("crypto: nonce: %w", err)
	}
	return l.active, g.Seal(nonce, nonce, dek, aad), nil
}

// Owns implements KeyOwner.
func (l *LocalKEK) Owns(keyID string) bool {
	_, ok := l.keys[keyID]
	return ok
}

// ActiveKeyID implements ActiveKeyer.
func (l *LocalKEK) ActiveKeyID(context.Context) (string, error) { return l.active, nil }

// Unwrap implements KeyWrapper.
func (l *LocalKEK) Unwrap(_ context.Context, keyID string, wrapped, aad []byte) ([]byte, error) {
	k, ok := l.keys[keyID]
	if !ok {
		return nil, ErrUnknownKey
	}
	g, err := newGCM(k)
	if err != nil {
		return nil, err
	}
	if len(wrapped) < g.NonceSize()+g.Overhead() {
		return nil, ErrMalformed
	}
	dek, err := g.Open(nil, wrapped[:g.NonceSize()], wrapped[g.NonceSize():], aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return dek, nil
}

// Chain is the KeyWrapper of a key migration: Wrap always uses the active
// wrapper (never a fallback: a failing key service fails the seal), Unwrap
// asks the wrappers in order and uses the first that owns the key id.
// Typical: NewChain(openBaoTransit, localKEK) while values sealed under the
// local KEK are re-wrapped (crypto.rewrap).
type Chain struct {
	active KeyWrapper
	keyer  ActiveKeyer
	all    []KeyWrapper
	owners []KeyOwner // owners[i] is all[i]
}

var (
	_ KeyWrapper  = (*Chain)(nil)
	_ KeyOwner    = (*Chain)(nil)
	_ ActiveKeyer = (*Chain)(nil)
)

// NewChain returns a Chain; every wrapper must implement KeyOwner, the
// active one also ActiveKeyer.
func NewChain(active KeyWrapper, older ...KeyWrapper) (*Chain, error) {
	if active == nil {
		return nil, fmt.Errorf("%w: no active key wrapper", ErrConfig)
	}
	keyer, ok := active.(ActiveKeyer)
	if !ok {
		return nil, fmt.Errorf("%w: the active key wrapper cannot name its key", ErrConfig)
	}
	all := append([]KeyWrapper{active}, older...)
	owners := make([]KeyOwner, len(all))
	for i, w := range all {
		o, ok := w.(KeyOwner)
		if !ok {
			return nil, fmt.Errorf("%w: every chained key wrapper must name the keys it owns", ErrConfig)
		}
		owners[i] = o
	}
	return &Chain{active: active, keyer: keyer, all: all, owners: owners}, nil
}

// Wrap implements KeyWrapper with the active wrapper.
func (c *Chain) Wrap(ctx context.Context, dek, aad []byte) (string, []byte, error) {
	return c.active.Wrap(ctx, dek, aad)
}

// Unwrap implements KeyWrapper with the wrapper that owns keyID.
func (c *Chain) Unwrap(ctx context.Context, keyID string, wrapped, aad []byte) ([]byte, error) {
	for i, o := range c.owners {
		if o.Owns(keyID) {
			return c.all[i].Unwrap(ctx, keyID, wrapped, aad)
		}
	}
	return nil, ErrUnknownKey
}

// Owns implements KeyOwner.
func (c *Chain) Owns(keyID string) bool {
	for _, o := range c.owners {
		if o.Owns(keyID) {
			return true
		}
	}
	return false
}

// ActiveKeyID implements ActiveKeyer.
func (c *Chain) ActiveKeyID(ctx context.Context) (string, error) {
	return c.keyer.ActiveKeyID(ctx)
}

// ---------------------------------------------------------------------------
// Envelope encryption

// Format v1 (02 §0.4 "sürüm ‖ key_id ‖ sarılı DEK ‖ nonce ‖ şifreli metin"):
//
//	0x01 ‖ len(kid) u8 ‖ kid ‖ len(wrapped) u16be ‖ wrapped ‖ nonce(12) ‖ ciphertext+tag(16)
const (
	formatV1      = 0x01
	maxWrappedLen = 1024
	nonceSize     = 12
	tagSize       = 16
	dekAADDomain  = "koro/envelope-dek/v1"
	dataAADDomain = "koro/envelope-data/v1"
)

// Envelope seals and opens values with per-value data keys.
type Envelope struct {
	w KeyWrapper
}

// NewEnvelope returns an Envelope over w.
func NewEnvelope(w KeyWrapper) (*Envelope, error) {
	if w == nil {
		return nil, fmt.Errorf("%w: no key wrapper", ErrConfig)
	}
	return &Envelope{w: w}, nil
}

// Seal encrypts plaintext for purpose.
func (e *Envelope) Seal(ctx context.Context, purpose string, plaintext []byte) ([]byte, error) {
	if purpose == "" {
		return nil, ErrPurpose
	}
	dek := make([]byte, KeySize)
	defer clear(dek)
	if _, err := rand.Read(dek); err != nil {
		return nil, fmt.Errorf("crypto: dek: %w", err)
	}
	kid, wrapped, err := e.w.Wrap(ctx, dek, lp([]byte(dekAADDomain), []byte(purpose)))
	if err != nil {
		return nil, fmt.Errorf("crypto: wrap: %w", err)
	}
	if !keyIDRe.MatchString(kid) || len(wrapped) == 0 || len(wrapped) > maxWrappedLen {
		return nil, fmt.Errorf("%w: key wrapper returned an invalid key id or wrapped key", ErrConfig)
	}
	out := make([]byte, 0, 4+len(kid)+len(wrapped)+nonceSize+len(plaintext)+tagSize)
	out = append(out, formatV1, byte(len(kid))) //nolint:gosec // kid matched keyIDRe: at most 64 bytes
	out = append(out, kid...)
	out = binary.BigEndian.AppendUint16(out, uint16(len(wrapped))) //nolint:gosec // bounded by maxWrappedLen above
	out = append(out, wrapped...)
	header := len(out)

	g, err := newGCM(dek)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("crypto: nonce: %w", err)
	}
	out = append(out, nonce...)
	return g.Seal(out, nonce, plaintext, dataAAD(purpose, out[:header])), nil
}

// Open decrypts a value sealed for purpose. A wrong purpose, a modified
// byte or a wrong key all give ErrDecrypt (errors.Is); an unknown key id
// ErrUnknownKey; wrapper failures (e.g. a remote KMS) are passed through.
func (e *Envelope) Open(ctx context.Context, purpose string, sealed []byte) ([]byte, error) {
	if purpose == "" {
		return nil, ErrPurpose
	}
	h, err := parseHeader(sealed)
	if err != nil {
		return nil, err
	}
	dek, err := e.w.Unwrap(ctx, h.kid, h.wrapped, lp([]byte(dekAADDomain), []byte(purpose)))
	if err != nil {
		return nil, fmt.Errorf("crypto: unwrap: %w", err)
	}
	defer clear(dek)
	g, err := newGCM(dek)
	if err != nil {
		return nil, ErrDecrypt
	}
	body := sealed[h.size:]
	pt, err := g.Open(nil, body[:nonceSize], body[nonceSize:], dataAAD(purpose, sealed[:h.size]))
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}

// ActiveKeyID returns the key id new values are sealed under; the wrapper
// must implement ActiveKeyer.
func (e *Envelope) ActiveKeyID(ctx context.Context) (string, error) {
	ak, ok := e.w.(ActiveKeyer)
	if !ok {
		return "", fmt.Errorf("%w: the key wrapper cannot name its active key", ErrConfig)
	}
	kid, err := ak.ActiveKeyID(ctx)
	if err != nil {
		return "", fmt.Errorf("crypto: active key: %w", err)
	}
	return kid, nil
}

// Rewrap re-seals a value under the active key (key migration and
// rotation). A value already under the active key is returned unchanged
// with changed=false, so repeating Rewrap is harmless. Otherwise the value
// is opened and sealed again with a fresh data key: the data AAD binds the
// header, so the data itself is re-encrypted; the plaintext exists only in
// memory and is cleared. Errors are those of Open and Seal; nothing is
// returned on error.
func (e *Envelope) Rewrap(ctx context.Context, purpose string, sealed []byte) (out []byte, changed bool, err error) {
	kid, err := KeyID(sealed)
	if err != nil {
		return nil, false, err
	}
	active, err := e.ActiveKeyID(ctx)
	if err != nil {
		return nil, false, err
	}
	if kid == active {
		return sealed, false, nil
	}
	pt, err := e.Open(ctx, purpose, sealed)
	if err != nil {
		return nil, false, err
	}
	defer clear(pt)
	out, err = e.Seal(ctx, purpose, pt)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

// KeyID returns the id of the KEK that wrapped sealed (for rotation jobs).
func KeyID(sealed []byte) (string, error) {
	h, err := parseHeader(sealed)
	if err != nil {
		return "", err
	}
	return h.kid, nil
}

type header struct {
	kid     string
	wrapped []byte
	size    int // bytes before the nonce
}

func parseHeader(b []byte) (header, error) {
	if len(b) < 2 || b[0] != formatV1 {
		return header{}, ErrMalformed
	}
	n := int(b[1])
	pos := 2 + n
	if len(b) < pos+2 {
		return header{}, ErrMalformed
	}
	kid := string(b[2:pos])
	w := int(binary.BigEndian.Uint16(b[pos:]))
	pos += 2
	if !keyIDRe.MatchString(kid) || w == 0 || w > maxWrappedLen || len(b) < pos+w+nonceSize+tagSize {
		return header{}, ErrMalformed
	}
	return header{kid: kid, wrapped: b[pos : pos+w], size: pos + w}, nil
}

func dataAAD(purpose string, header []byte) []byte {
	return lp(lp([]byte(dataAADDomain), []byte(purpose)), header)
}

// ---------------------------------------------------------------------------
// Blind index

// Indexer computes blind indexes. Its key cannot rotate without
// recomputing every stored index, so it is a single key.
type Indexer struct {
	key []byte
}

// NewIndexer returns an Indexer over a key of at least MinMACKeySize bytes.
func NewIndexer(key []byte) (*Indexer, error) {
	if len(key) < MinMACKeySize {
		return nil, fmt.Errorf("%w: blind index key must be at least %d bytes", ErrConfig, MinMACKeySize)
	}
	return &Indexer{key: append([]byte(nil), key...)}, nil
}

// BlindIndex returns HMAC-SHA256 of v under the purpose's subkey (32 bytes).
// Normalize v first (e.g. textx.NormalizeEmail): equal inputs give equal
// indexes. purpose is a code constant; an empty purpose is a programming
// error and panics.
func (x *Indexer) BlindIndex(purpose string, v []byte) []byte {
	if purpose == "" {
		panic(ErrPurpose)
	}
	m := hmac.New(sha256.New, subkey(x.key, "koro/blind-index/v1", purpose))
	m.Write(v)
	return m.Sum(nil)
}

// ---------------------------------------------------------------------------
// Derived tokens

// Deriver computes HMAC tokens with rotating keys: new tokens use
// ActiveKeyID; verification passes the key id stored with the token.
type Deriver struct {
	active string
	keys   map[string][]byte
}

// NewDeriver returns a Deriver; keys[0] is active.
func NewDeriver(keys []Key) (*Deriver, error) {
	m, err := keyring(keys, MinMACKeySize, 1024)
	if err != nil {
		return nil, err
	}
	return &Deriver{active: keys[0].ID, keys: m}, nil
}

// ActiveKeyID returns the id to use for new tokens.
func (d *Deriver) ActiveKeyID() string { return d.active }

// DerivedToken returns HMAC-SHA256 over the length-prefixed parts under the
// (keyID, purpose) subkey (32 bytes). Compare results with hmac.Equal.
func (d *Deriver) DerivedToken(purpose, keyID string, parts ...[]byte) ([]byte, error) {
	if purpose == "" {
		return nil, ErrPurpose
	}
	k, ok := d.keys[keyID]
	if !ok {
		return nil, ErrUnknownKey
	}
	m := hmac.New(sha256.New, subkey(k, "koro/derived-token/v1", purpose))
	var buf []byte
	for _, p := range parts {
		buf = lp(buf[:0], p)
		m.Write(buf)
	}
	return m.Sum(nil), nil
}
