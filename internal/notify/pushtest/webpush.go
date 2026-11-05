package pushtest

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

// WebPushEndpointBase is the push service the double impersonates: its
// subscriptions are FCM web push endpoints, so they pass notify's allow-list
// (WebPush.Client routes every host to the double).
const WebPushEndpointBase = "https://fcm.googleapis.com/fcm/send/"

// WebPush is a Web Push (RFC 8030) push service double. Each subscription
// has its own browser key pair and secret; the double checks the VAPID
// token (RFC 8292: ES256 by the key in k=, aud = the endpoint's origin, a
// subject) and decrypts the aes128gcm payload (RFC 8291) as a browser would,
// with an implementation of its own.
type WebPush struct {
	Server *httptest.Server

	mu       sync.Mutex
	subs     map[string]*browser // by subscription ID (the last endpoint segment)
	messages []Message
	vapid    []string // the k= of every accepted request
	dead     map[string]bool
	down     bool
	refused  int
}

type browser struct {
	key  *ecdh.PrivateKey
	auth []byte
}

// WebPushSubscription is what a browser hands to the application.
type WebPushSubscription struct {
	ID       string // the device token of Message.DeviceToken
	Endpoint string
	P256DH   []byte
	Auth     []byte
}

// NewWebPush starts the double (closed with the test).
func NewWebPush(t testing.TB) *WebPush {
	t.Helper()
	w := &WebPush{subs: map[string]*browser{}, dead: map[string]bool{}}
	w.Server = httptest.NewTLSServer(http.HandlerFunc(w.serve))
	t.Cleanup(w.Server.Close)
	return w
}

// NewVAPIDKey returns a fresh VAPID private key in the PUSH_VAPID_PRIVATE_KEY
// form (base64url scalar): made at run time, never stored.
func NewVAPIDKey(t testing.TB) string {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	b, err := k.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Subscribe creates a browser subscription.
func (w *WebPush) Subscribe(t testing.TB) WebPushSubscription {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth, raw := make([]byte, 16), make([]byte, 12)
	_, _ = rand.Read(auth)
	_, _ = rand.Read(raw)
	id := base64.RawURLEncoding.EncodeToString(raw)
	w.mu.Lock()
	w.subs[id] = &browser{key: key, auth: auth}
	w.mu.Unlock()
	return WebPushSubscription{ID: id, Endpoint: WebPushEndpointBase + id, P256DH: key.PublicKey().Bytes(), Auth: auth}
}

// Client sends every request to the double, whatever the host, and trusts
// its certificate. It never follows redirects.
func (w *WebPush) Client() *http.Client {
	pool := x509.NewCertPool()
	pool.AddCert(w.Server.Certificate())
	addr := w.Server.Listener.Addr().String()
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
			TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "example.com", MinVersion: tls.VersionTLS12},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Kill makes the double answer 410 Gone for the subscription ID.
func (w *WebPush) Kill(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.dead[id] = true
}

// SetDown makes the double answer 503.
func (w *WebPush) SetDown(down bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.down = down
}

// Messages returns the pushes the double accepted and decrypted.
func (w *WebPush) Messages() []Message {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]Message(nil), w.messages...)
}

// VAPIDKeys returns the k= public key of every accepted request.
func (w *WebPush) VAPIDKeys() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.vapid...)
}

// Refused counts the requests refused for a bad VAPID token or payload.
func (w *WebPush) Refused() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.refused
}

func (w *WebPush) serve(rw http.ResponseWriter, r *http.Request) {
	id, ok := strings.CutPrefix(r.URL.Path, "/fcm/send/")
	if !ok || r.Method != http.MethodPost {
		http.NotFound(rw, r)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 8192))
	w.mu.Lock()
	defer w.mu.Unlock()
	b := w.subs[id]
	switch {
	case w.down:
		rw.WriteHeader(http.StatusServiceUnavailable)
		return
	case b == nil:
		rw.WriteHeader(http.StatusNotFound)
		return
	case w.dead[id]:
		rw.WriteHeader(http.StatusGone)
		return
	}
	k, ok := checkVAPID(r.Header.Get("Authorization"))
	if !ok || r.Header.Get("TTL") == "" || r.Header.Get("Content-Encoding") != "aes128gcm" || len(r.Header.Get("Topic")) > 32 ||
		!slices.Contains([]string{"very-low", "low", "normal", "high"}, r.Header.Get("Urgency")) {
		w.refused++
		rw.WriteHeader(http.StatusForbidden)
		return
	}
	plain, err := decryptAES128GCM(body, b.key, b.auth)
	var in struct {
		Title string            `json:"title"`
		Body  string            `json:"body"`
		Tag   string            `json:"tag"`
		Data  map[string]string `json:"data"`
	}
	if err != nil || json.Unmarshal(plain, &in) != nil {
		w.refused++
		rw.WriteHeader(http.StatusBadRequest)
		return
	}
	w.vapid = append(w.vapid, k)
	w.messages = append(w.messages, Message{DeviceToken: id, Title: in.Title, Body: in.Body, Data: in.Data,
		CollapseID: r.Header.Get("Topic"), Priority: r.Header.Get("Urgency")})
	rw.Header().Set("Location", fmt.Sprintf("https://fcm.googleapis.com/0:msg-%d", len(w.messages)))
	rw.WriteHeader(http.StatusCreated)
}

// checkVAPID verifies "vapid t=<jwt>, k=<key>" (RFC 8292 §3) for the FCM
// origin and returns k.
func checkVAPID(h string) (string, bool) {
	rest, ok := strings.CutPrefix(h, "vapid ")
	if !ok {
		return "", false
	}
	var tok, k string
	for part := range strings.SplitSeq(rest, ",") {
		part = strings.TrimSpace(part)
		if v, ok := strings.CutPrefix(part, "t="); ok {
			tok = v
		} else if v, ok := strings.CutPrefix(part, "k="); ok {
			k = v
		}
	}
	raw, err := base64.RawURLEncoding.DecodeString(k)
	if err != nil {
		return "", false
	}
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), raw)
	if err != nil {
		return "", false
	}
	header, claims, ok := parseJWT(tok, func(input string, sig []byte) bool {
		if len(sig) != 64 {
			return false
		}
		sum := sha256.Sum256([]byte(input))
		return ecdsa.Verify(pub, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:]))
	})
	sub, _ := claims["sub"].(string)
	_, hasExp := claims["exp"].(float64)
	if !ok || header["alg"] != "ES256" || claims["aud"] != "https://fcm.googleapis.com" || !hasExp ||
		(!strings.HasPrefix(sub, "mailto:") && !strings.HasPrefix(sub, "https://")) {
		return "", false
	}
	return k, true
}

// decryptAES128GCM opens one aes128gcm record (RFC 8188) with the browser's
// key and secret (RFC 8291 §3.4).
func decryptAES128GCM(body []byte, key *ecdh.PrivateKey, auth []byte) ([]byte, error) {
	if len(body) < 21 {
		return nil, errors.New("short body")
	}
	salt, rs, idlen := body[:16], binary.BigEndian.Uint32(body[16:20]), int(body[20])
	if len(body) < 21+idlen || rs < 18 {
		return nil, errors.New("bad header")
	}
	asPub, err := ecdh.P256().NewPublicKey(body[21 : 21+idlen])
	if err != nil {
		return nil, err
	}
	shared, err := key.ECDH(asPub)
	if err != nil {
		return nil, err
	}
	info := append(append([]byte("WebPush: info\x00"), key.PublicKey().Bytes()...), asPub.Bytes()...)
	ikm, err := hkdf.Key(sha256.New, shared, auth, string(info), 32)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	record, err := gcm.Open(nil, nonce, body[21+idlen:], nil)
	if err != nil {
		return nil, err
	}
	end := strings.LastIndexByte(string(record), 0x02) // the last-record delimiter, then zero padding
	if end < 0 || strings.Trim(string(record[end+1:]), "\x00") != "" {
		return nil, errors.New("no delimiter")
	}
	return record[:end], nil
}
