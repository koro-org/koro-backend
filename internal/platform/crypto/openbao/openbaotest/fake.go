// Package openbaotest provides OpenBao Transit doubles for tests: Fake, an
// in-process httptest server that speaks the subset of the Transit API
// openbao.Transit uses, and Start, a real OpenBao dev container
// (integration tests). Never import it outside tests.
package openbaotest

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// FakeToken is the token Fake accepts.
const FakeToken = "fake-openbao-token-0123456789abcdef" //nolint:gosec // G101: test double credential

// Fake is an in-memory Transit server with one AEAD key per name.
type Fake struct {
	Server *httptest.Server
	Mount  string

	mu   sync.Mutex
	keys map[string][][]byte // name → versions (index 0 = v1)
	down atomic.Bool

	Encrypts atomic.Int64
	Decrypts atomic.Int64
	Renews   atomic.Int64
}

// NewFake starts a Fake with mount "transit" and the keys names (one
// version each); it is closed when the test ends.
func NewFake(t testing.TB, names ...string) *Fake {
	t.Helper()
	f := &Fake{Mount: "transit", keys: map[string][][]byte{}}
	for _, n := range names {
		f.Rotate(n)
	}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Server.Close)
	return f
}

// Rotate adds a key version (creating the key on first use).
func (f *Fake) Rotate(name string) {
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys[name] = append(f.keys[name], k)
}

// SetDown makes every request answer 503 (sealed / unreachable OpenBao).
func (f *Fake) SetDown(down bool) { f.down.Store(down) }

type jsonMap = map[string]any

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	if f.down.Load() {
		writeJSON(w, http.StatusServiceUnavailable, jsonMap{"errors": []string{"Vault is sealed"}})
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Vault-Token")), []byte(FakeToken)) != 1 {
		writeJSON(w, http.StatusForbidden, jsonMap{"errors": []string{"permission denied"}})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/")
	switch {
	case path == "auth/token/lookup-self" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, jsonMap{"data": jsonMap{"renewable": true, "ttl": 3600}})
	case path == "auth/token/renew-self" && r.Method == http.MethodPost:
		f.Renews.Add(1)
		writeJSON(w, http.StatusOK, jsonMap{"auth": jsonMap{"lease_duration": 3600}})
	case strings.HasPrefix(path, f.Mount+"/keys/") && r.Method == http.MethodGet:
		name := strings.TrimPrefix(path, f.Mount+"/keys/")
		f.mu.Lock()
		n := len(f.keys[name])
		f.mu.Unlock()
		if n == 0 {
			writeJSON(w, http.StatusNotFound, jsonMap{"errors": []string{}})
			return
		}
		writeJSON(w, http.StatusOK, jsonMap{"data": jsonMap{
			"name": name, "type": "aes256-gcm96", "latest_version": n, "derived": false, "exportable": false,
			"supports_encryption": true, "supports_decryption": true,
		}})
	case strings.HasPrefix(path, f.Mount+"/encrypt/") && r.Method == http.MethodPost:
		f.Encrypts.Add(1)
		f.encrypt(w, r, strings.TrimPrefix(path, f.Mount+"/encrypt/"))
	case strings.HasPrefix(path, f.Mount+"/decrypt/") && r.Method == http.MethodPost:
		f.Decrypts.Add(1)
		f.decrypt(w, r, strings.TrimPrefix(path, f.Mount+"/decrypt/"))
	default:
		writeJSON(w, http.StatusNotFound, jsonMap{"errors": []string{"unsupported path"}})
	}
}

func (f *Fake) gcm(name string, version int) (cipher.AEAD, int, bool) {
	f.mu.Lock()
	vs := f.keys[name]
	f.mu.Unlock()
	if len(vs) == 0 {
		return nil, 0, false
	}
	if version == 0 {
		version = len(vs)
	}
	if version < 1 || version > len(vs) {
		return nil, 0, false
	}
	b, _ := aes.NewCipher(vs[version-1])
	g, _ := cipher.NewGCM(b)
	return g, version, true
}

func (f *Fake) encrypt(w http.ResponseWriter, r *http.Request, name string) {
	var in struct {
		Plaintext      string `json:"plaintext"`
		AssociatedData string `json:"associated_data"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		writeJSON(w, http.StatusBadRequest, jsonMap{"errors": []string{"bad json"}})
		return
	}
	pt, err1 := base64.StdEncoding.DecodeString(in.Plaintext)
	ad, err2 := base64.StdEncoding.DecodeString(in.AssociatedData)
	g, v, ok := f.gcm(name, 0)
	if err1 != nil || err2 != nil || !ok {
		writeJSON(w, http.StatusBadRequest, jsonMap{"errors": []string{"encryption key not found"}})
		return
	}
	nonce := make([]byte, g.NonceSize())
	_, _ = rand.Read(nonce)
	ct := g.Seal(nonce, nonce, pt, ad)
	writeJSON(w, http.StatusOK, jsonMap{"data": jsonMap{
		"ciphertext": "vault:v" + strconv.Itoa(v) + ":" + base64.StdEncoding.EncodeToString(ct), "key_version": v,
	}})
}

func (f *Fake) decrypt(w http.ResponseWriter, r *http.Request, name string) {
	var in struct {
		Ciphertext     string `json:"ciphertext"`
		AssociatedData string `json:"associated_data"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		writeJSON(w, http.StatusBadRequest, jsonMap{"errors": []string{"bad json"}})
		return
	}
	bad := func() {
		writeJSON(w, http.StatusBadRequest, jsonMap{"errors": []string{"invalid ciphertext: unable to decrypt"}})
	}
	rest, ok := strings.CutPrefix(in.Ciphertext, "vault:v")
	vs, b64, ok2 := strings.Cut(rest, ":")
	v, err := strconv.Atoi(vs)
	ct, err2 := base64.StdEncoding.DecodeString(b64)
	ad, err3 := base64.StdEncoding.DecodeString(in.AssociatedData)
	if !ok || !ok2 || err != nil || err2 != nil || err3 != nil {
		bad()
		return
	}
	g, _, ok := f.gcm(name, v)
	if !ok || len(ct) < g.NonceSize() {
		bad()
		return
	}
	pt, err := g.Open(nil, ct[:g.NonceSize()], ct[g.NonceSize():], ad)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, jsonMap{"errors": []string{"cipher: message authentication failed"}})
		return
	}
	writeJSON(w, http.StatusOK, jsonMap{"data": jsonMap{"plaintext": base64.StdEncoding.EncodeToString(pt)}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
