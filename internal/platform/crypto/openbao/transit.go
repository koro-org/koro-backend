// Package openbao is the OpenBao Transit key wrapper of platform/crypto
// (BE-055; 01 §1.3, T-82). The key-encryption key lives in OpenBao and
// never leaves it (KVKK 2018/10: keys are kept apart from the data): Koro
// sends a fresh data key (DEK) to transit/encrypt and stores the returned
// ciphertext inside the envelope header, next to the key id
// "bao.<key>.v<version>". Unwrap sends it to transit/decrypt. The envelope
// purpose travels as Transit associated data (AEAD), so a wrapped DEK opens
// only for its purpose.
//
// Failure is closed: an unreachable, sealed or refusing OpenBao makes Wrap
// and Unwrap fail with crypto.ErrUnavailable; no local fallback key is used
// for new values. Neither the token, nor the DEK, nor the plaintext ever
// appears in an error or a log line.
//
// OpenBao is the MPL-2.0 fork of Vault; the Transit API is the same
// (X-Vault-Token, "vault:v<N>:" ciphertexts). The key must be an AEAD type
// (aes256-gcm96 recommended), not derived, not exportable. Minimal policy:
//
//	path "transit/encrypt/<key>" { capabilities = ["update"] }
//	path "transit/decrypt/<key>" { capabilities = ["update"] }
//	path "transit/keys/<key>"    { capabilities = ["read"] }
//
// plus the default policy (token lookup-self and renew-self).
package openbao

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"koro/internal/platform/clock"
	"koro/internal/platform/crypto"
)

// KeyIDPrefix starts every key id this wrapper writes into an envelope
// header. Local KEK ids must not use it (config rejects them).
const KeyIDPrefix = "bao."

// Defaults.
const (
	DefaultMount   = "transit"
	DefaultKey     = "koro-kek"
	DefaultTimeout = 5 * time.Second

	// activeKeyTTL bounds how long the latest key version is cached; a
	// rotation in OpenBao is seen by ActiveKeyID within this time (Wrap
	// learns it at once from the encrypt answer).
	activeKeyTTL = time.Minute
	maxBody      = 1 << 20
	maxErrText   = 200
)

var (
	reName   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,50}$`)
	reMount  = regexp.MustCompile(`^[A-Za-z0-9_-]+(/[A-Za-z0-9_-]+)*$`)
	reCipher = regexp.MustCompile(`^vault:v([1-9][0-9]{0,8}):[A-Za-z0-9+/=]+$`)
)

// aeadTypes are the Transit key types that honour associated_data.
var aeadTypes = map[string]bool{
	"aes128-gcm96": true, "aes256-gcm96": true, "chacha20-poly1305": true, "xchacha20-poly1305": true,
}

// Config configures Transit.
type Config struct {
	// Addr is the OpenBao address ("https://openbao.internal:8200").
	Addr string
	// Token authenticates every request (X-Vault-Token). A periodic token
	// is renewed by the crypto.renew_token job (RenewToken).
	Token string
	// Mount is the Transit mount path (DefaultMount when empty).
	Mount string
	// Key is the Transit key name (DefaultKey when empty).
	Key string
	// CACertPEM, when set, is the only CA trusted for Addr.
	CACertPEM []byte
	// Timeout bounds one request (DefaultTimeout when zero).
	Timeout time.Duration
	// Clock caches the active key version.
	Clock clock.Clock
	// HTTPClient replaces the client built from the settings above (tests).
	HTTPClient *http.Client
}

// Transit implements crypto.KeyWrapper, KeyOwner and ActiveKeyer over an
// OpenBao Transit key.
type Transit struct {
	base   *url.URL
	token  string
	mount  string
	key    string
	client *http.Client
	clk    clock.Clock

	mu       sync.Mutex
	latest   int       // newest key version seen
	latestAt time.Time // when it was read from transit/keys
}

var (
	_ crypto.KeyWrapper  = (*Transit)(nil)
	_ crypto.KeyOwner    = (*Transit)(nil)
	_ crypto.ActiveKeyer = (*Transit)(nil)
)

// New validates cfg and returns the wrapper. It does not contact OpenBao:
// Check (the /readyz check) does.
func New(cfg Config) (*Transit, error) {
	u, err := url.Parse(strings.TrimRight(cfg.Addr, "/"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, fmt.Errorf("%w: openbao address must be an http(s) URL without credentials", crypto.ErrConfig)
	}
	if cfg.Token == "" {
		return nil, fmt.Errorf("%w: openbao token is empty", crypto.ErrConfig)
	}
	t := &Transit{base: u, token: cfg.Token, mount: cfg.Mount, key: cfg.Key, clk: cfg.Clock, client: cfg.HTTPClient}
	if t.mount == "" {
		t.mount = DefaultMount
	}
	if t.key == "" {
		t.key = DefaultKey
	}
	if !reMount.MatchString(t.mount) || !reName.MatchString(t.key) {
		return nil, fmt.Errorf("%w: openbao mount or key name has invalid characters", crypto.ErrConfig)
	}
	if t.clk == nil {
		return nil, fmt.Errorf("%w: openbao: no clock", crypto.ErrConfig)
	}
	if t.client == nil {
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = DefaultTimeout
		}
		t.client, err = newClient(timeout, cfg.CACertPEM)
		if err != nil {
			return nil, err
		}
	}
	return t, nil
}

func newClient(timeout time.Duration, caPEM []byte) (*http.Client, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if len(caPEM) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("%w: openbao CA certificate is not PEM", crypto.ErrConfig)
		}
		tlsCfg.RootCAs = pool
	}
	tr := &http.Transport{
		Proxy:                 nil, // the key service is internal; never via a proxy
		DialContext:           (&net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       tlsCfg,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   16,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{
		Transport:     tr,
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

// KeyName returns the Transit key name.
func (t *Transit) KeyName() string { return t.key }

// keyID is the envelope key id of version v.
func (t *Transit) keyID(v int) string { return KeyIDPrefix + t.key + ".v" + strconv.Itoa(v) }

// version parses a key id of this wrapper; ok=false for any other id.
func (t *Transit) version(keyID string) (int, bool) {
	rest, ok := strings.CutPrefix(keyID, KeyIDPrefix+t.key+".v")
	if !ok || rest == "" || rest[0] == '0' || len(rest) > 9 {
		return 0, false
	}
	v, err := strconv.Atoi(rest)
	if err != nil || v < 1 {
		return 0, false
	}
	return v, true
}

// Owns implements crypto.KeyOwner.
func (t *Transit) Owns(keyID string) bool {
	_, ok := t.version(keyID)
	return ok
}

// Wrap implements crypto.KeyWrapper: transit/encrypt with aad as associated
// data. The wrapped bytes are the Transit ciphertext ("vault:v<N>:…").
func (t *Transit) Wrap(ctx context.Context, dek, aad []byte) (string, []byte, error) {
	body := map[string]string{
		"plaintext":       base64.StdEncoding.EncodeToString(dek),
		"associated_data": base64.StdEncoding.EncodeToString(aad),
	}
	var out struct {
		Data struct {
			Ciphertext string `json:"ciphertext"`
			KeyVersion int    `json:"key_version"`
		} `json:"data"`
	}
	if err := t.call(ctx, http.MethodPost, t.mount+"/encrypt/"+t.key, body, &out); err != nil {
		return "", nil, err
	}
	v, ok := cipherVersion(out.Data.Ciphertext)
	if !ok || (out.Data.KeyVersion != 0 && out.Data.KeyVersion != v) {
		return "", nil, fmt.Errorf("%w: openbao returned an unexpected ciphertext", crypto.ErrUnavailable)
	}
	t.seen(v)
	return t.keyID(v), []byte(out.Data.Ciphertext), nil
}

// Unwrap implements crypto.KeyWrapper. A ciphertext whose version differs
// from its key id, or that fails authentication (wrong purpose, modified
// byte), is crypto.ErrDecrypt.
func (t *Transit) Unwrap(ctx context.Context, keyID string, wrapped, aad []byte) ([]byte, error) {
	v, ok := t.version(keyID)
	if !ok {
		return nil, crypto.ErrUnknownKey
	}
	if cv, ok := cipherVersion(string(wrapped)); !ok || cv != v {
		return nil, crypto.ErrDecrypt
	}
	body := map[string]string{
		"ciphertext":      string(wrapped),
		"associated_data": base64.StdEncoding.EncodeToString(aad),
	}
	var out struct {
		Data struct {
			Plaintext string `json:"plaintext"`
		} `json:"data"`
	}
	if err := t.call(ctx, http.MethodPost, t.mount+"/decrypt/"+t.key, body, &out); err != nil {
		return nil, err
	}
	dek, err := base64.StdEncoding.DecodeString(out.Data.Plaintext)
	if err != nil || len(dek) != crypto.KeySize {
		clear(dek)
		return nil, crypto.ErrDecrypt
	}
	return dek, nil
}

// ActiveKeyID implements crypto.ActiveKeyer: the newest key version (read
// from transit/keys at most once a minute, or learnt from Wrap).
func (t *Transit) ActiveKeyID(ctx context.Context) (string, error) {
	t.mu.Lock()
	v, fresh := t.latest, !t.latestAt.IsZero() && t.clk.Now().Sub(t.latestAt) < activeKeyTTL
	t.mu.Unlock()
	if fresh && v > 0 {
		return t.keyID(v), nil
	}
	info, err := t.keyInfo(ctx)
	if err != nil {
		return "", err
	}
	return t.keyID(info.LatestVersion), nil
}

// KeyInfo is what Check learns about the Transit key.
type KeyInfo struct {
	Type          string `json:"type"`
	LatestVersion int    `json:"latest_version"`
	Derived       bool   `json:"derived"`
	Exportable    bool   `json:"exportable"`
	SupportsEnc   bool   `json:"supports_encryption"`
	SupportsDec   bool   `json:"supports_decryption"`
}

func (t *Transit) keyInfo(ctx context.Context) (KeyInfo, error) {
	var out struct {
		Data KeyInfo `json:"data"`
	}
	if err := t.call(ctx, http.MethodGet, t.mount+"/keys/"+t.key, nil, &out); err != nil {
		return KeyInfo{}, err
	}
	k := out.Data
	switch {
	case k.LatestVersion < 1:
		return KeyInfo{}, fmt.Errorf("%w: openbao key %q has no version", crypto.ErrUnavailable, t.key)
	case !aeadTypes[k.Type]:
		return KeyInfo{}, fmt.Errorf("%w: openbao key %q has type %q, want an AEAD type (aes256-gcm96)", crypto.ErrConfig, t.key, k.Type)
	case k.Derived:
		return KeyInfo{}, fmt.Errorf("%w: openbao key %q is derived; use a plain AEAD key", crypto.ErrConfig, t.key)
	case !k.SupportsEnc || !k.SupportsDec:
		return KeyInfo{}, fmt.Errorf("%w: openbao key %q cannot encrypt and decrypt", crypto.ErrConfig, t.key)
	}
	t.mu.Lock()
	t.latest, t.latestAt = max(t.latest, k.LatestVersion), t.clk.Now()
	t.mu.Unlock()
	return k, nil
}

// Check is the /readyz check: OpenBao answers, the token is valid and the
// key exists with a usable type. It never encrypts.
func (t *Transit) Check(ctx context.Context) error {
	_, err := t.keyInfo(ctx)
	return err
}

// RenewToken renews the token's lease when it is renewable (a periodic
// token); a non-renewable token (dev root) is left alone. It is the
// crypto.renew_token job.
func (t *Transit) RenewToken(ctx context.Context) (renewed bool, err error) {
	var self struct {
		Data struct {
			Renewable bool `json:"renewable"`
		} `json:"data"`
	}
	if err := t.call(ctx, http.MethodGet, "auth/token/lookup-self", nil, &self); err != nil {
		return false, err
	}
	if !self.Data.Renewable {
		return false, nil
	}
	if err := t.call(ctx, http.MethodPost, "auth/token/renew-self", map[string]string{}, nil); err != nil {
		return false, err
	}
	return true, nil
}

func (t *Transit) seen(v int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if v > t.latest {
		t.latest = v
	}
}

func cipherVersion(c string) (int, bool) {
	m := reCipher.FindStringSubmatch(c)
	if m == nil {
		return 0, false
	}
	v, err := strconv.Atoi(m[1])
	return v, err == nil
}

// call performs one request against /v1/<path>. Errors carry the HTTP
// status and OpenBao's error text (never the request body or the token).
func (t *Transit) call(ctx context.Context, method, path string, in, out any) error {
	var rd io.Reader
	var payload []byte
	if in != nil {
		var err error
		if payload, err = json.Marshal(in); err != nil {
			return fmt.Errorf("openbao: encode request: %w", err)
		}
		defer clear(payload) // holds a DEK on encrypt
		rd = bytes.NewReader(payload)
	}
	u := t.base.JoinPath("v1", path)
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return fmt.Errorf("%w: openbao: %s", crypto.ErrConfig, path)
	}
	req.Header.Set("X-Vault-Token", t.token)
	req.Header.Set("X-Vault-Request", "true")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: openbao %s: %s", crypto.ErrUnavailable, path, transportError(err))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	defer clear(raw) // holds a DEK on decrypt
	if err != nil {
		return fmt.Errorf("%w: openbao %s: reading the answer: %s", crypto.ErrUnavailable, path, transportError(err))
	}
	if resp.StatusCode/100 != 2 {
		return statusError(path, resp.StatusCode, raw)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%w: openbao %s: malformed answer", crypto.ErrUnavailable, path)
	}
	return nil
}

// statusError maps an OpenBao error answer. A decrypt refused for the data
// (authentication failed, bad ciphertext) is crypto.ErrDecrypt; everything
// else is crypto.ErrUnavailable (fail closed).
func statusError(path string, status int, raw []byte) error {
	var body struct {
		Errors []string `json:"errors"`
	}
	_ = json.Unmarshal(raw, &body)
	msg := cleanText(strings.Join(body.Errors, "; "))
	if status == http.StatusBadRequest && strings.Contains(path, "/decrypt/") && dataError(msg) {
		return fmt.Errorf("%w: openbao: %s", crypto.ErrDecrypt, msg)
	}
	return fmt.Errorf("%w: openbao %s: HTTP %d: %s", crypto.ErrUnavailable, path, status, msg)
}

func dataError(msg string) bool {
	m := strings.ToLower(msg)
	for _, s := range []string{"message authentication failed", "invalid ciphertext", "unable to decode", "unable to decrypt", "invalid key version"} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}

// cleanText keeps an error text short and on one line.
func cleanText(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if len(s) > maxErrText {
		s = s[:maxErrText] + "…"
	}
	if s == "" {
		return "no error text"
	}
	return s
}

// transportError reduces a client error to its cause; url.Error would
// repeat the URL (harmless) but nothing else is needed.
func transportError(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return cleanText(err.Error())
}
