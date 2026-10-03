// Package pushtest provides FCM HTTP v1 and APNs doubles (httptest TLS
// servers) for the push adapter tests of notify and cmd/koro. They verify
// the OAuth assertion / provider token signatures with the keys they
// generate, record what they receive and can be told to reject device
// tokens. Never import it outside tests.
package pushtest

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// Message is one push the doubles accepted.
type Message struct {
	DeviceToken string
	Title, Body string
	Data        map[string]string
	CollapseID  string
}

// ---------------------------------------------------------------- FCM

// FCM is an FCM HTTP v1 + OAuth token endpoint double.
type FCM struct {
	Server      *httptest.Server
	ProjectID   string
	Credentials []byte // service account JSON (token_uri points here)
	PrivateKey  string // the PEM inside Credentials (secret-leak tests)

	key *rsa.PrivateKey

	mu       sync.Mutex
	messages []Message
	dead     map[string]bool
	attempts map[string]int
	issued   []string
	down     bool

	TokenExchanges atomic.Int64
}

// NewFCM starts the double (closed with the test).
func NewFCM(t testing.TB) *FCM {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	f := &FCM{ProjectID: "koro-test-project", key: key, dead: map[string]bool{}, attempts: map[string]int{}}
	f.PrivateKey = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	f.Server = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Server.Close)
	f.Credentials, _ = json.Marshal(map[string]string{
		"type": "service_account", "project_id": f.ProjectID, "private_key_id": "kid-1",
		"private_key": f.PrivateKey, "client_email": "push@koro-test-project.iam.gserviceaccount.com",
		"token_uri": f.Server.URL + "/token",
	})
	return f
}

// Client trusts the double's certificate.
func (f *FCM) Client() *http.Client { return f.Server.Client() }

// Kill makes the double answer UNREGISTERED for token.
func (f *FCM) Kill(token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dead[token] = true
}

// SetDown makes every send answer 503.
func (f *FCM) SetDown(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = down
}

// Messages returns the accepted pushes.
func (f *FCM) Messages() []Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Message(nil), f.messages...)
}

// Attempts returns how often token was tried.
func (f *FCM) Attempts(token string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts[token]
}

// IssuedAccessTokens returns the access tokens handed out.
func (f *FCM) IssuedAccessTokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.issued...)
}

func (f *FCM) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/token" && r.Method == http.MethodPost:
		f.token(w, r)
	case r.URL.Path == "/v1/projects/"+f.ProjectID+"/messages:send" && r.Method == http.MethodPost:
		f.send(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *FCM) token(w http.ResponseWriter, r *http.Request) {
	f.TokenExchanges.Add(1)
	if err := r.ParseForm(); err != nil || r.PostForm.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported_grant_type"})
		return
	}
	claims, ok := verifyJWT(r.PostForm.Get("assertion"), func(input string, sig []byte) bool {
		sum := sha256.Sum256([]byte(input))
		return rsa.VerifyPKCS1v15(&f.key.PublicKey, crypto.SHA256, sum[:], sig) == nil
	})
	if !ok || claims["aud"] != f.Server.URL+"/token" || claims["scope"] != "https://www.googleapis.com/auth/firebase.messaging" ||
		claims["iss"] != "push@koro-test-project.iam.gserviceaccount.com" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant", "error_description": "bad assertion"})
		return
	}
	f.mu.Lock()
	tok := fmt.Sprintf("ya29.fake-access-%d", len(f.issued)+1)
	f.issued = append(f.issued, tok)
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"access_token": tok, "expires_in": 3599, "token_type": "Bearer"})
}

func (f *FCM) send(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	issued := append([]string(nil), f.issued...)
	down := f.down
	f.mu.Unlock()
	auth, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if len(issued) == 0 || auth != issued[len(issued)-1] {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{"code": 401, "status": "UNAUTHENTICATED", "message": "bad token"}})
		return
	}
	var in struct {
		Message struct {
			Token        string            `json:"token"`
			Notification map[string]string `json:"notification"`
			Data         map[string]string `json:"data"`
			Android      struct {
				CollapseKey string `json:"collapse_key"`
			} `json:"android"`
		} `json:"message"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"status": "INVALID_ARGUMENT", "message": "bad json"}})
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts[in.Message.Token]++
	switch {
	case down:
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]any{"code": 503, "status": "UNAVAILABLE", "message": "try later"}})
	case f.dead[in.Message.Token]:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{
			"code": 404, "status": "NOT_FOUND", "message": "Requested entity was not found.",
			"details": []map[string]string{{"@type": "type.googleapis.com/google.firebase.fcm.v1.FcmError", "errorCode": "UNREGISTERED"}},
		}})
	default:
		f.messages = append(f.messages, Message{DeviceToken: in.Message.Token, Title: in.Message.Notification["title"],
			Body: in.Message.Notification["body"], Data: in.Message.Data, CollapseID: in.Message.Android.CollapseKey})
		writeJSON(w, http.StatusOK, map[string]string{"name": fmt.Sprintf("projects/%s/messages/%d", f.ProjectID, len(f.messages))})
	}
}

// ---------------------------------------------------------------- APNs

// APNs is an APNs (HTTP/2) double with token-based authentication.
type APNs struct {
	Server *httptest.Server
	KeyPEM []byte
	KeyID  string
	TeamID string
	Topic  string

	key *ecdsa.PrivateKey

	mu         sync.Mutex
	messages   []Message
	dead       map[string]bool
	tokens     []string // provider tokens seen, in order
	expireNext bool
	nonHTTP2   int
}

// NewAPNs starts the double (closed with the test).
func NewAPNs(t testing.TB) *APNs {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	a := &APNs{KeyID: "ABC123DEFG", TeamID: "TEAM123456", Topic: "app.koro.mobile", key: key, dead: map[string]bool{},
		KeyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})}
	a.Server = httptest.NewUnstartedServer(http.HandlerFunc(a.serve))
	a.Server.EnableHTTP2 = true
	a.Server.StartTLS()
	t.Cleanup(a.Server.Close)
	return a
}

// Client trusts the double's certificate and speaks HTTP/2.
func (a *APNs) Client() *http.Client { return a.Server.Client() }

// Kill makes the double answer 410 Unregistered for token.
func (a *APNs) Kill(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dead[token] = true
}

// ExpireNextToken makes the next request fail with ExpiredProviderToken.
func (a *APNs) ExpireNextToken() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.expireNext = true
}

// ProviderTokens returns the provider tokens seen, in order.
func (a *APNs) ProviderTokens() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.tokens...)
}

// Messages returns the accepted pushes.
func (a *APNs) Messages() []Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Message(nil), a.messages...)
}

// NonHTTP2Requests counts requests that did not use HTTP/2 (must be 0).
func (a *APNs) NonHTTP2Requests() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.nonHTTP2
}

// IssuedAt returns the iat claim of a provider token (0 when invalid).
func (a *APNs) IssuedAt(tok string) int64 {
	claims, ok := verifyJWT(tok, a.verify)
	if !ok {
		return 0
	}
	iat, _ := claims["iat"].(float64)
	return int64(iat)
}

func (a *APNs) verify(input string, sig []byte) bool {
	if len(sig) != 64 {
		return false
	}
	sum := sha256.Sum256([]byte(input))
	r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
	return ecdsa.Verify(&a.key.PublicKey, sum[:], r, s)
}

func (a *APNs) serve(w http.ResponseWriter, r *http.Request) {
	device, ok := strings.CutPrefix(r.URL.Path, "/3/device/")
	if !ok || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if r.ProtoMajor != 2 {
		a.nonHTTP2++
	}
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "bearer ")
	a.tokens = append(a.tokens, tok)
	header, claims, ok := parseJWT(tok, a.verify)
	switch {
	case !ok || header["kid"] != a.KeyID || header["alg"] != "ES256" || claims["iss"] != a.TeamID:
		writeJSON(w, http.StatusForbidden, map[string]string{"reason": "InvalidProviderToken"})
		return
	case a.expireNext:
		a.expireNext = false
		writeJSON(w, http.StatusForbidden, map[string]string{"reason": "ExpiredProviderToken"})
		return
	case r.Header.Get("apns-topic") != a.Topic:
		writeJSON(w, http.StatusBadRequest, map[string]string{"reason": "BadTopic"})
		return
	case a.dead[device]:
		writeJSON(w, http.StatusGone, map[string]any{"reason": "Unregistered", "timestamp": 1791000000000})
		return
	}
	var in struct {
		APS struct {
			Alert map[string]string `json:"alert"`
		} `json:"aps"`
		Data map[string]string `json:"koro"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"reason": "BadPayload"})
		return
	}
	a.messages = append(a.messages, Message{DeviceToken: device, Title: in.APS.Alert["title"], Body: in.APS.Alert["body"],
		Data: in.Data, CollapseID: r.Header.Get("apns-collapse-id")})
	w.Header().Set("apns-id", fmt.Sprintf("00000000-0000-4000-8000-%012d", len(a.messages)))
	w.WriteHeader(http.StatusOK)
}

// ---------------------------------------------------------------- JWT

func verifyJWT(tok string, verify func(input string, sig []byte) bool) (map[string]any, bool) {
	_, claims, ok := parseJWT(tok, verify)
	return claims, ok
}

func parseJWT(tok string, verify func(input string, sig []byte) bool) (header, claims map[string]any, ok bool) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil, nil, false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !verify(parts[0]+"."+parts[1], sig) {
		return nil, nil, false
	}
	hb, err1 := base64.RawURLEncoding.DecodeString(parts[0])
	cb, err2 := base64.RawURLEncoding.DecodeString(parts[1])
	if err1 != nil || err2 != nil || json.Unmarshal(hb, &header) != nil || json.Unmarshal(cb, &claims) != nil {
		return nil, nil, false
	}
	return header, claims, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
