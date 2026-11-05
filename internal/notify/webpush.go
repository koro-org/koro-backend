package notify

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"koro/internal/platform/clock"
)

// Web push (BE-173): the browsers' Push API subscriptions, reached over the
// Web Push protocol (RFC 8030) with the payload encrypted for the browser
// (RFC 8291, aes128gcm of RFC 8188) and the sender identified by a VAPID
// token (RFC 8292). Everything is standard library crypto (crypto/ecdh,
// crypto/hkdf, AES-GCM, ES256): no third-party dependency.
const (
	// DefaultWebPushTTL is how long the push service keeps an undelivered
	// message (the TTL header).
	DefaultWebPushTTL = 24 * time.Hour
	// vapidTokenTTL is the lifetime of a VAPID token (RFC 8292 §2: at most
	// 24 hours); a cached token is renewed one hour before it ends.
	vapidTokenTTL    = 12 * time.Hour
	vapidTokenMargin = time.Hour
	// webPushRecordSize is the aes128gcm record size: one record carries the
	// whole payload (push services accept at most 4096 bytes).
	webPushRecordSize = 4096
	// maxWebPushPayload bounds the plaintext: 4096 minus the header (86),
	// the tag (16) and the delimiter (1), with a margin.
	maxWebPushPayload = 3800
	// MaxWebPushEndpoint bounds a subscription endpoint (the table CHECK).
	MaxWebPushEndpoint = 2048
	// maxWebPushMessageID bounds the message ID kept from Location.
	maxWebPushMessageID = 200
	// webPushKeyIDLen is the idlen of the aes128gcm header: the sender's
	// public point (RFC 8291 §4).
	webPushKeyIDLen = 65
)

// Lengths of the subscription keys (RFC 8291 §3.2).
const (
	WebPushP256DHLen = 65 // uncompressed P-256 point
	WebPushAuthLen   = 16
)

// webPushHosts and webPushHostSuffixes are the push services a subscription
// endpoint may name (the SSRF guard: Koro only ever posts to these). FCM
// serves Chrome (and other Chromium browsers), Mozilla Firefox, Apple
// Safari, Microsoft (WNS) Edge.
var (
	webPushHosts        = []string{"fcm.googleapis.com", "updates.push.services.mozilla.com", "web.push.apple.com"}
	webPushHostSuffixes = []string{".push.services.mozilla.com", ".push.apple.com", ".notify.windows.com"}
)

// ValidWebPushEndpoint reports whether endpoint is an https URL of a known
// push service: no credentials, no port other than 443, no IP literal, no
// fragment, at most MaxWebPushEndpoint bytes.
func ValidWebPushEndpoint(endpoint string) bool {
	if endpoint == "" || len(endpoint) > MaxWebPushEndpoint || strings.IndexFunc(endpoint, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r) || r > unicode.MaxASCII
	}) >= 0 {
		return false
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" || u.Fragment != "" ||
		(u.Port() != "" && u.Port() != "443") {
		return false
	}
	host := u.Hostname()
	if host != strings.ToLower(host) || net.ParseIP(host) != nil {
		return false
	}
	for _, h := range webPushHosts {
		if host == h {
			return true
		}
	}
	for _, sfx := range webPushHostSuffixes {
		if strings.HasSuffix(host, sfx) && len(host) > len(sfx) {
			return true
		}
	}
	return false
}

// WebPushConfig configures the web push adapter.
type WebPushConfig struct {
	// PrivateKey is the VAPID key: the P-256 private scalar, base64url
	// (PUSH_VAPID_PRIVATE_KEY, a Docker secret; `npx web-push
	// generate-vapid-keys` prints this form).
	PrivateKey string
	// Subject is the contact of the sender, a mailto: or https: URI
	// (PUSH_VAPID_SUBJECT; RFC 8292 §2.1).
	Subject string
	// TTL is the TTL header; zero → DefaultWebPushTTL.
	TTL     time.Duration
	Timeout time.Duration
	Clock   clock.Clock
	// HTTPClient replaces the client built from Timeout (tests). It must not
	// follow redirects.
	HTTPClient *http.Client
}

// WebPush sends to browser push services. The VAPID token of each push
// service origin is cached for vapidTokenTTL.
type WebPush struct {
	key     *ecdsa.PrivateKey
	public  []byte // uncompressed point
	subject string
	ttl     time.Duration
	client  *http.Client
	clk     clock.Clock

	mu     sync.Mutex
	tokens map[string]vapidToken // by audience (the endpoint's origin)
}

type vapidToken struct {
	header  string
	expires time.Time
}

// ParseVAPIDKey decodes a base64url P-256 private scalar. Errors never
// repeat the key.
func ParseVAPIDKey(raw string) (*ecdsa.PrivateKey, error) {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(strings.TrimSpace(raw), "="))
	if err != nil {
		return nil, errors.New("notify: PUSH_VAPID_PRIVATE_KEY must be base64url")
	}
	key, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), b)
	if err != nil {
		return nil, errors.New("notify: PUSH_VAPID_PRIVATE_KEY must be a P-256 private key (32 bytes, base64url)")
	}
	return key, nil
}

// ValidVAPIDSubject reports whether s is a mailto: address or an https URL
// (RFC 8292 §2.1).
func ValidVAPIDSubject(s string) bool {
	if addr, ok := strings.CutPrefix(s, "mailto:"); ok {
		a, err := mail.ParseAddress(addr)
		return err == nil && a.Address == addr
	}
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil
}

// NewWebPush parses the key and validates the settings.
func NewWebPush(c WebPushConfig) (*WebPush, error) {
	if c.Clock == nil {
		return nil, errors.New("notify: web push needs a clock")
	}
	key, err := ParseVAPIDKey(c.PrivateKey)
	if err != nil {
		return nil, err
	}
	if !ValidVAPIDSubject(c.Subject) {
		return nil, errors.New("notify: PUSH_VAPID_SUBJECT must be a mailto: address or an https URL")
	}
	public, err := key.PublicKey.Bytes()
	if err != nil {
		return nil, fmt.Errorf("notify: VAPID public key: %w", err)
	}
	ttl := c.TTL
	if ttl <= 0 {
		ttl = DefaultWebPushTTL
	}
	client := c.HTTPClient
	if client == nil {
		client = pushHTTPClient(c.Timeout)
	}
	return &WebPush{key: key, public: public, subject: c.Subject, ttl: ttl, client: client, clk: c.Clock,
		tokens: map[string]vapidToken{}}, nil
}

// PublicKey is the VAPID public key, base64url of the uncompressed point:
// the applicationServerKey of PushManager.subscribe in the browser.
func (w *WebPush) PublicKey() string { return base64.RawURLEncoding.EncodeToString(w.public) }

func (*WebPush) name() string { return "webpush" }

// vapid returns the Authorization header value for the push service at
// audience (scheme://host).
func (w *WebPush) vapid(audience string) (string, error) {
	now := w.clk.Now()
	w.mu.Lock()
	defer w.mu.Unlock()
	if t, ok := w.tokens[audience]; ok && now.Before(t.expires.Add(-vapidTokenMargin)) {
		return t.header, nil
	}
	exp := now.Add(vapidTokenTTL)
	input, err := jwtInput(map[string]string{"typ": "JWT", "alg": "ES256"},
		map[string]any{"aud": audience, "exp": exp.Unix(), "sub": w.subject})
	if err != nil {
		return "", err
	}
	tok, err := signES256(w.key, input)
	if err != nil {
		return "", fmt.Errorf("webpush: %w", err)
	}
	header := "vapid t=" + tok + ", k=" + w.PublicKey()
	w.tokens[audience] = vapidToken{header: header, expires: exp}
	return header, nil
}

// webPushPayload is the JSON the service worker receives (push event
// data): the fixed text, the delivery ID as notification tag and the data
// of the mobile pushes (kind, delivery_id, link, opaque IDs).
type webPushPayload struct {
	Title string            `json:"title"`
	Body  string            `json:"body,omitempty"`
	Tag   string            `json:"tag"`
	Data  map[string]string `json:"data"`
}

// send delivers m to one browser subscription.
func (w *WebPush) send(ctx context.Context, s WebPushSubscription, m PushMessage) (string, error) {
	if !ValidWebPushEndpoint(s.Endpoint) {
		return "", fmt.Errorf("%w: not a known push service", errTokenInvalid)
	}
	ua, err := ecdh.P256().NewPublicKey(s.P256DH)
	if err != nil || len(s.Auth) != WebPushAuthLen {
		return "", fmt.Errorf("%w: malformed subscription keys", errTokenInvalid)
	}
	plain, err := json.Marshal(webPushPayload{Title: m.Title, Body: m.Body, Tag: m.DeliveryID.String(), Data: pushData(m)})
	if err != nil {
		return "", fmt.Errorf("webpush: encode: %w", err)
	}
	if len(plain) > maxWebPushPayload {
		return "", Permanent(errors.New("webpush: payload too large"))
	}
	as, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return "", fmt.Errorf("webpush: key: %w", err)
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("webpush: salt: %w", err)
	}
	body, err := encryptWebPush(plain, ua, s.Auth, as, salt)
	if err != nil {
		return "", err
	}
	u, _ := url.Parse(s.Endpoint) // valid: checked above
	auth, err := w.vapid(u.Scheme + "://" + u.Host)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.Endpoint, bytes.NewReader(body))
	if err != nil {
		return "", errors.New("webpush: cannot build the request")
	}
	topic := m.DeliveryID // collapse key (RFC 8030 §5.4: at most 32 base64url characters)
	req.Header.Set("Authorization", auth)
	req.Header.Set("TTL", strconv.FormatInt(int64(w.ttl/time.Second), 10))
	req.Header.Set("Urgency", "normal")
	req.Header.Set("Topic", base64.RawURLEncoding.EncodeToString(topic[:]))
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := w.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("webpush: %s", transportText(err)) // never the URL: the endpoint is a capability
	}
	defer resp.Body.Close()
	switch code := resp.StatusCode; {
	case code >= 200 && code < 300:
		return webPushMessageID(resp.Header.Get("Location")), nil
	case code == http.StatusNotFound || code == http.StatusGone:
		return "", fmt.Errorf("%w: webpush: HTTP %d", errTokenInvalid, code)
	case code == http.StatusTooManyRequests || code >= 500:
		return "", fmt.Errorf("webpush: HTTP %d", code)
	default: // 400 malformed, 401/403 VAPID refused, 413 too large
		return "", Permanent(fmt.Errorf("webpush: HTTP %d", code))
	}
}

// webPushMessageID keeps the last path segment of the message resource URI
// (RFC 8030 §5: the Location of a created push message).
func webPushMessageID(location string) string {
	u, err := url.Parse(location)
	if err != nil || u.Path == "" {
		return ""
	}
	id := path.Base(u.Path)
	if id == "/" || id == "." || len(id) > maxWebPushMessageID {
		return ""
	}
	return providerText(id)
}

// encryptWebPush encrypts plain for the browser key ua and its
// authentication secret (RFC 8291 §3.4) with the sender's ephemeral key as
// and salt: one aes128gcm record (RFC 8188 §2) with the last-record
// delimiter and no padding, behind the header salt ‖ rs ‖ idlen ‖ keyid
// (keyid = as's public point).
func encryptWebPush(plain []byte, ua *ecdh.PublicKey, authSecret []byte, as *ecdh.PrivateKey, salt []byte) ([]byte, error) {
	if len(as.PublicKey().Bytes()) != webPushKeyIDLen {
		return nil, errors.New("webpush: the sender key is not a P-256 key")
	}
	shared, err := as.ECDH(ua)
	if err != nil {
		return nil, fmt.Errorf("webpush: ecdh: %w", err)
	}
	uaPub, asPub := ua.Bytes(), as.PublicKey().Bytes()
	prkKey, err := hkdf.Extract(sha256.New, shared, authSecret)
	if err != nil {
		return nil, fmt.Errorf("webpush: hkdf: %w", err)
	}
	ikm, err := hkdf.Expand(sha256.New, prkKey, "WebPush: info\x00"+string(uaPub)+string(asPub), 32)
	if err != nil {
		return nil, fmt.Errorf("webpush: hkdf: %w", err)
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, fmt.Errorf("webpush: hkdf: %w", err)
	}
	cek, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, fmt.Errorf("webpush: hkdf: %w", err)
	}
	nonce, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, fmt.Errorf("webpush: hkdf: %w", err)
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, fmt.Errorf("webpush: aes: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("webpush: gcm: %w", err)
	}
	header := make([]byte, 0, 16+4+1+len(asPub))
	header = append(header, salt...)
	header = binary.BigEndian.AppendUint32(header, webPushRecordSize)
	header = append(header, webPushKeyIDLen) // keyid = the uncompressed point
	header = append(header, asPub...)
	record := append(append(make([]byte, 0, len(plain)+1), plain...), 0x02) // last record, no padding
	return gcm.Seal(header, nonce, record, nil), nil
}
