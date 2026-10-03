// Package iystest provides an İYS API double (httptest server) for the
// consent adapter tests of consent and cmd/koro: OAuth password sign-in and
// the consent add endpoint, with switches for outages, rejected recipients
// and expired tokens. Never import it outside tests.
package iystest

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Credentials and codes the double accepts.
const (
	Username  = "koro-iys-user"
	Password  = "koro-iys-password-123" //nolint:gosec // G101: test double credential
	IYSCode   = "700123"
	BrandCode = "700124"
)

// Consent is one record the double accepted.
type Consent struct {
	ConsentDate   string `json:"consentDate"`
	Source        string `json:"source"`
	Recipient     string `json:"recipient"`
	RecipientType string `json:"recipientType"`
	Status        string `json:"status"`
	Type          string `json:"type"`
}

// Server is the İYS double.
type Server struct {
	*httptest.Server

	mu       sync.Mutex
	consents []Consent
	issued   []string
	down     bool
	reject   map[string]bool
	expire   bool
	signIns  int
}

// New starts the double (closed with the test).
func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{reject: map[string]bool{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// SetDown makes every request answer 503.
func (s *Server) SetDown(down bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.down = down
}

// Reject makes İYS refuse records of recipient (422, H-coded error that
// repeats the address, as İYS does).
func (s *Server) Reject(recipient string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reject[recipient] = true
}

// ExpireTokens makes the next consent request answer 401.
func (s *Server) ExpireTokens() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire = true
}

// Consents returns the accepted records.
func (s *Server) Consents() []Consent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Consent(nil), s.consents...)
}

// SignIns counts the password sign-ins.
func (s *Server) SignIns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.signIns
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.down {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"message": "service unavailable"})
		return
	}
	switch {
	case r.URL.Path == "/oauth2/token" && r.Method == http.MethodPost:
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["grant_type"] != "password" || in["username"] != Username ||
			subtle.ConstantTimeCompare([]byte(in["password"]), []byte(Password)) != 1 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"errors": []map[string]string{{"code": "H001", "message": "invalid credentials"}}})
			return
		}
		s.signIns++
		tok := fmt.Sprintf("iys-access-%d", len(s.issued)+1)
		s.issued = append(s.issued, tok)
		writeJSON(w, http.StatusOK, map[string]any{"access_token": tok, "expires_in": 7200, "refresh_token": "iys-refresh", "token_type": "bearer"})
	case r.URL.Path == "/sps/"+IYSCode+"/brands/"+BrandCode+"/consents" && r.Method == http.MethodPost:
		auth, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if s.expire || len(s.issued) == 0 || auth != s.issued[len(s.issued)-1] {
			s.expire = false
			writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "token expired"})
			return
		}
		var c Consent
		if json.NewDecoder(r.Body).Decode(&c) != nil || !valid(c) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"errors": []map[string]string{{"code": "H100", "message": "invalid request"}}})
			return
		}
		if s.reject[c.Recipient] {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"errors": []map[string]string{
				{"code": "H174", "message": "alıcı geçersiz: " + c.Recipient},
			}})
			return
		}
		s.consents = append(s.consents, c)
		writeJSON(w, http.StatusOK, map[string]string{"transactionId": fmt.Sprintf("iys-tx-%d", len(s.consents)), "creationDate": c.ConsentDate})
	default:
		http.NotFound(w, r)
	}
}

func valid(c Consent) bool {
	_, err := time.Parse("2006-01-02 15:04:05", c.ConsentDate)
	return err == nil && c.Recipient != "" &&
		(c.Status == "ONAY" || c.Status == "RET") &&
		(c.Type == "EPOSTA" || c.Type == "MESAJ" || c.Type == "ARAMA") &&
		(c.RecipientType == "BIREYSEL" || c.RecipientType == "TACIR") &&
		strings.HasPrefix(c.Source, "HS_")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
