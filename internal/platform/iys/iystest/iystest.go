// Package iystest is a double of İYS's official API (httptest server,
// openapi-v1.json 3.2.3) for the tests of the official adapter, consent and
// cmd/koro. It speaks the documented contract: OAuth password and refresh
// sign-in, consent add (H174, H175, H178, H467), the change list (PULL:
// source=IYS, after/limit, the last hour hidden, seven days kept, 204), the
// single status query and the bulk approval query. Every answer carries an
// X-Trace-Id. Switches cover outages, rejected recipients, expired tokens
// and İYS's hourly limits (H084, H086).
//
// Two answers are not documented by İYS and are this double's assumption
// (to be checked against İYS's sandbox): a single status query of a
// recipient İYS has no record of answers 404; the bulk query lists the
// approvals only. Never import it outside tests.
package iystest

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"koro/internal/platform/clock"
	"koro/internal/platform/iys"
)

// Credentials and codes the double accepts (synthetic test values).
const (
	Username  = "koro-iys-user"
	Password  = "koro-iys-password-123" //nolint:gosec // G101: test double credential
	IYSCode   = "700123"
	BrandCode = "700124"
)

const (
	dateLayout = "2006-01-02 15:04:05"
	pageLimit  = 1000
	bulkLimit  = 1000
	lag        = time.Hour
	window     = 7 * 24 * time.Hour
	tokenTTL   = 2 * time.Hour
	refreshTTL = 4 * time.Hour
)

// Consent is one consent add the double accepted.
type Consent struct {
	ConsentDate   string `json:"consentDate"`
	Source        string `json:"source"`
	Recipient     string `json:"recipient"`
	RecipientType string `json:"recipientType"`
	Status        string `json:"status"`
	Type          string `json:"type"`
}

// Action is a consent act a recipient performs in İYS itself (İYS web or
// mobile, e-Devlet, 3338): it changes the record and enters the change list.
type Action struct {
	Recipient string
	Type      string // EPOSTA (default), MESAJ, ARAMA
	Status    string // ONAY, RET (default)
	Source    string // IYS_WEB (default) or another İYS source
	// RecipientType is BIREYSEL (default) or TACIR.
	RecipientType string
	// CitizenName is sent with IYS_WEB_YENI (personal data the adapter must drop).
	CitizenName string
}

type recordKey struct{ recipient, typ, recipientType string }

type record struct {
	status, source, txID string
	consentDate          time.Time
	creationDate         time.Time
}

type change struct {
	key         recordKey
	rec         record
	citizenName string
}

// Server is the İYS double.
type Server struct {
	*httptest.Server
	clk clock.Clock

	mu          sync.Mutex
	records     map[recordKey]record
	feed        []change
	consents    []Consent
	access      map[string]time.Time // access token → expiry
	refresh     map[string]time.Time // refresh token → expiry
	tokens      int
	tx          int
	traces      int
	lastTrace   string
	down        bool
	reject      map[string]bool
	expire      bool
	signIns     int
	refreshes   int
	calls       map[string]int
	statusLimit int
	statusTimes []time.Time
	tokenLimit  int
	tokenTimes  []time.Time
}

// Option configures the double.
type Option func(*Server)

// WithClock makes the double keep time with c (default: the real clock).
func WithClock(c clock.Clock) Option { return func(s *Server) { s.clk = c } }

// WithStatusLimit answers 429 H086 after n single status queries within an
// hour (İYS: 1000 per IP and hour).
func WithStatusLimit(n int) Option { return func(s *Server) { s.statusLimit = n } }

// WithTokenLimit answers 429 H084 after n token requests within an hour.
func WithTokenLimit(n int) Option { return func(s *Server) { s.tokenLimit = n } }

// New starts the double (closed with the test).
func New(t testing.TB, opts ...Option) *Server {
	t.Helper()
	s := &Server{
		clk: clock.Real(), records: map[recordKey]record{}, access: map[string]time.Time{}, refresh: map[string]time.Time{},
		reject: map[string]bool{}, calls: map[string]int{},
	}
	for _, o := range opts {
		o(s)
	}
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
	s.reject[norm(recipient)] = true
}

// ExpireTokens makes the next authorised request answer 401.
func (s *Server) ExpireTokens() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire = true
}

// RevokeTokens invalidates every access token issued so far (requests
// answer 401 until a new sign-in).
func (s *Server) RevokeTokens() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.access)
}

// Act records a consent act of a recipient in İYS itself, now.
func (s *Server) Act(a Action) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := recordKey{recipient: norm(a.Recipient), typ: def(a.Type, "EPOSTA"), recipientType: def(a.RecipientType, "BIREYSEL")}
	now := s.clk.Now().Truncate(time.Second)
	s.tx++
	rec := record{status: def(a.Status, "RET"), source: def(a.Source, "IYS_WEB"), txID: txID(s.tx), consentDate: now, creationDate: now}
	s.records[k] = rec
	s.feed = append(s.feed, change{key: k, rec: rec, citizenName: a.CitizenName})
}

// Held returns İYS's record of recipient for typ (BIREYSEL): status,
// consent date and source.
func (s *Server) Held(recipient, typ string) (status string, consentDate time.Time, source string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[recordKey{recipient: norm(recipient), typ: typ, recipientType: "BIREYSEL"}]
	return r.status, r.consentDate, r.source, ok
}

// Consents returns the accepted consent adds.
func (s *Server) Consents() []Consent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Consent(nil), s.consents...)
}

// SignIns counts the password sign-ins; Refreshes the refresh-token ones.
func (s *Server) SignIns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.signIns
}

// Refreshes counts the refresh-token sign-ins.
func (s *Server) Refreshes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshes
}

// Calls counts the requests of an operation: "token", "add", "changes",
// "status" (single), "bulk".
func (s *Server) Calls(op string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[op]
}

// LastTrace is the X-Trace-Id of the latest answer.
func (s *Server) LastTrace() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastTrace
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.traces++
	s.lastTrace = fmt.Sprintf("%032x", s.traces)
	w.Header().Set("X-Trace-Id", s.lastTrace)
	if s.down {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"message": "service unavailable"})
		return
	}
	brand := "/sps/" + IYSCode + "/brands/" + BrandCode + "/consents"
	path := r.URL.Path
	switch {
	case path == "/oauth2/token" && r.Method == http.MethodPost:
		s.calls["token"]++
		s.token(w, r)
	case !s.authorised(w, r):
	case path == brand && r.Method == http.MethodPost:
		s.calls["add"]++
		s.add(w, r)
	case path == brand+"/changes" && r.Method == http.MethodGet:
		s.calls["changes"]++
		s.changes(w, r)
	case path == brand+"/status" && r.Method == http.MethodPost:
		s.calls["status"]++
		s.status(w, r)
	case strings.HasPrefix(path, brand+"/") && strings.Contains(path, "/status/") && r.Method == http.MethodPost:
		s.calls["bulk"]++
		s.bulk(w, r, strings.TrimPrefix(path, brand+"/"))
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	now := s.clk.Now()
	if s.tokenLimit > 0 {
		s.tokenTimes = within(s.tokenTimes, now)
		if len(s.tokenTimes) >= s.tokenLimit {
			s.tokenTimes = append(s.tokenTimes, now) // a retry restarts the hour (İYS)
			writeErr(w, http.StatusTooManyRequests, "H084", "Saatte kabul edilen jeton alma istek limitine ulaşıldı.")
			return
		}
		s.tokenTimes = append(s.tokenTimes, now)
	}
	var in map[string]string
	_ = json.NewDecoder(r.Body).Decode(&in)
	switch in["grant_type"] {
	case "password":
		if in["username"] != Username || subtle.ConstantTimeCompare([]byte(in["password"]), []byte(Password)) != 1 {
			writeErr(w, http.StatusBadRequest, "H001", "invalid credentials")
			return
		}
		s.signIns++
	case "refresh_token":
		exp, ok := s.refresh[in["refresh_token"]]
		if !ok || !now.Before(exp) {
			writeErr(w, http.StatusBadRequest, "H002", "invalid refresh token")
			return
		}
		delete(s.refresh, in["refresh_token"])
		s.refreshes++
	default:
		writeErr(w, http.StatusBadRequest, "H003", "unsupported grant type")
		return
	}
	s.tokens++
	access := fmt.Sprintf("iys-access-%d", s.tokens)
	refresh := fmt.Sprintf("iys-refresh-%d", s.tokens)
	s.access[access], s.refresh[refresh] = now.Add(tokenTTL), now.Add(refreshTTL)
	writeJSON(w, http.StatusOK, map[string]any{"access_token": access, "expires_in": int(tokenTTL / time.Second),
		"refresh_token": refresh, "refresh_expires_in": int(refreshTTL / time.Second), "token_type": "bearer"})
}

func (s *Server) authorised(w http.ResponseWriter, r *http.Request) bool {
	auth, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	exp, ok := s.access[auth]
	if s.expire || !ok || !s.clk.Now().Before(exp) {
		s.expire = false
		writeErr(w, http.StatusUnauthorized, "H351", "Eksik veya geçersiz jeton!")
		return false
	}
	return true
}

func (s *Server) add(w http.ResponseWriter, r *http.Request) {
	var c Consent
	if json.NewDecoder(r.Body).Decode(&c) != nil || !valid(c) {
		writeErr(w, http.StatusBadRequest, "H100", "invalid request")
		return
	}
	at, _ := time.ParseInLocation(dateLayout, c.ConsentDate, clock.Istanbul())
	now := s.clk.Now()
	k := recordKey{recipient: norm(c.Recipient), typ: c.Type, recipientType: c.RecipientType}
	cur, found := s.records[k]
	switch {
	case s.reject[k.recipient]:
		writeErr(w, http.StatusUnprocessableEntity, "H464", "alıcı geçersiz: "+c.Recipient)
		return
	case !now.Before(iys.RegistrationDeadline(at)):
		writeErr(w, http.StatusUnprocessableEntity, "H467", "Üç iş günü öncesinde alınan izinler kaydedilemez.")
		return
	case c.Status == "RET" && !found:
		writeErr(w, http.StatusBadRequest, "H175", "İlk defa kaydedilen bir iznin durum (status) bilgisi RET olmamalıdır.")
		return
	case c.Status == "RET" && cur.status == "RET":
		writeErr(w, http.StatusBadRequest, "H174", "İzin durumu (status) güncellemesi için farklı bir durum girilmelidir. İzin durumu: RET")
		return
	case found && at.Before(cur.consentDate):
		writeErr(w, http.StatusBadRequest, "H178", "Sistemdeki iznin tarihinden önceki tarihli izinlerle güncelleme yapılamaz.")
		return
	}
	s.tx++
	s.records[k] = record{status: c.Status, source: c.Source, txID: txID(s.tx), consentDate: at, creationDate: now.Truncate(time.Second)}
	s.consents = append(s.consents, c)
	writeJSON(w, http.StatusOK, map[string]string{"transactionId": txID(s.tx), "creationDate": now.In(clock.Istanbul()).Format(dateLayout)})
}

type item struct {
	ConsentDate   string `json:"consentDate"`
	CreationDate  string `json:"creationDate"`
	Source        string `json:"source"`
	Recipient     string `json:"recipient"`
	RecipientType string `json:"recipientType"`
	Status        string `json:"status"`
	Type          string `json:"type"`
	TransactionID string `json:"transactionId"`
	CitizenName   string `json:"citizenName,omitempty"`
}

func (s *Server) changes(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if src := q.Get("source"); src != "" && src != "IYS" && src != "HS" {
		writeErr(w, http.StatusUnprocessableEntity, "H506", "İzin kaynağı (source) bulunamadı.")
		return
	}
	limit := pageLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > pageLimit {
			writeErr(w, http.StatusUnprocessableEntity, "H507", "limit alanı için karakter üst sınırı 1000 olmalıdır.")
			return
		}
		limit = n
	}
	if q.Get("source") == "HS" {
		w.WriteHeader(http.StatusNoContent) // the brand's own records are not modelled
		return
	}
	start := 0
	if after := q.Get("after"); after != "" {
		start = -1
		for i, c := range s.feed {
			if after == cursorOf(c) {
				start = i + 1
			}
		}
		if start < 0 {
			writeErr(w, http.StatusBadRequest, "H097", "after değeri geçersiz.")
			return
		}
	}
	now := s.clk.Now()
	var list []item
	last := ""
	for _, c := range s.feed[start:] {
		if c.rec.creationDate.After(now.Add(-lag)) {
			break
		}
		if c.rec.creationDate.Before(now.Add(-window)) {
			continue
		}
		if len(list) == limit {
			break
		}
		ist := clock.Istanbul()
		list = append(list, item{ConsentDate: c.rec.consentDate.In(ist).Format(dateLayout), CreationDate: c.rec.creationDate.In(ist).Format(dateLayout),
			Source: c.rec.source, Recipient: c.key.recipient, RecipientType: c.key.recipientType, Status: c.rec.status, Type: c.key.typ,
			TransactionID: c.rec.txID, CitizenName: c.citizenName})
		last = cursorOf(c)
	}
	if len(list) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"after": last, "list": list})
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	now := s.clk.Now()
	if s.statusLimit > 0 {
		s.statusTimes = within(s.statusTimes, now)
		if len(s.statusTimes) >= s.statusLimit {
			writeErr(w, http.StatusTooManyRequests, "H086", "Saatte kabul edilen tekil izin durumu sorgulama istek limitine ulaşıldı.")
			return
		}
		s.statusTimes = append(s.statusTimes, now)
	}
	var in struct{ Recipient, RecipientType, Type string }
	if json.NewDecoder(r.Body).Decode(&in) != nil || in.Recipient == "" || in.RecipientType == "" || in.Type == "" {
		writeErr(w, http.StatusUnprocessableEntity, "H111", "zorunlu alan eksik")
		return
	}
	rec, ok := s.records[recordKey{recipient: norm(in.Recipient), typ: in.Type, recipientType: in.RecipientType}]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "not found (iystest assumption)"})
		return
	}
	ist := clock.Istanbul()
	writeJSON(w, http.StatusOK, item{ConsentDate: rec.consentDate.In(ist).Format(dateLayout), CreationDate: rec.creationDate.In(ist).Format(dateLayout),
		Source: rec.source, Recipient: norm(in.Recipient), RecipientType: in.RecipientType, Status: rec.status, Type: in.Type,
		TransactionID: rec.txID})
}

func (s *Server) bulk(w http.ResponseWriter, r *http.Request, rest string) {
	recipientType, typ, ok := strings.Cut(rest, "/status/")
	if !ok || (recipientType != "BIREYSEL" && recipientType != "TACIR") || (typ != "EPOSTA" && typ != "MESAJ" && typ != "ARAMA") {
		writeErr(w, http.StatusUnprocessableEntity, "H120", "geçersiz alıcı tipi veya izin tipi")
		return
	}
	var in struct {
		Recipients []string `json:"recipients"`
	}
	switch {
	case json.NewDecoder(r.Body).Decode(&in) != nil || in.Recipients == nil:
		writeErr(w, http.StatusUnprocessableEntity, "H168", "recipients alanı zorunludur")
		return
	case len(in.Recipients) == 0:
		writeErr(w, http.StatusUnprocessableEntity, "H164", "liste boş olamaz")
		return
	case len(in.Recipients) > bulkLimit:
		writeErr(w, http.StatusUnprocessableEntity, "H165", "en fazla 1000 adres")
		return
	}
	seen := map[string]bool{}
	list := []string{}
	for _, a := range in.Recipients {
		if seen[norm(a)] {
			writeErr(w, http.StatusUnprocessableEntity, "H193", "tekrar eden adres")
			return
		}
		seen[norm(a)] = true
		if rec, ok := s.records[recordKey{recipient: norm(a), typ: typ, recipientType: recipientType}]; ok && rec.status == "ONAY" {
			list = append(list, norm(a))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"requestId": fmt.Sprintf("req-%d", s.traces), "list": list})
}

func valid(c Consent) bool {
	_, err := time.Parse(dateLayout, c.ConsentDate)
	return err == nil && c.Recipient != "" &&
		(c.Status == "ONAY" || c.Status == "RET") &&
		(c.Type == "EPOSTA" || c.Type == "MESAJ" || c.Type == "ARAMA") &&
		(c.RecipientType == "BIREYSEL" || c.RecipientType == "TACIR") &&
		strings.HasPrefix(c.Source, "HS_")
}

// norm is İYS's address form: e-mail addresses are kept in lower case.
func norm(a string) string {
	if strings.Contains(a, "@") {
		return strings.ToLower(a)
	}
	return a
}

func def(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func txID(n int) string { return fmt.Sprintf("%064x", n) }

func cursorOf(c change) string {
	return strconv.FormatInt(c.rec.creationDate.Unix(), 10) + "_" + c.rec.txID
}

func within(ts []time.Time, now time.Time) []time.Time {
	out := ts[:0]
	for _, t := range ts {
		if now.Sub(t) < time.Hour {
			out = append(out, t)
		}
	}
	return out
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"errors": []map[string]string{{"code": code, "message": msg}}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
