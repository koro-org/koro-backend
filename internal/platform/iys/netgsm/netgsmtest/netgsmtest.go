// Package netgsmtest is a double of the Netgsm İYS API (NetİYS; BE-110)
// for the tests of the netgsm adapter, consent and cmd/koro. It speaks the
// contract of Netgsm's documentation (https://www.netgsm.com.tr/dokuman/,
// "NETIYS", 2026-10-25):
//
//   - POST /iys/add: {"header": {username, password, brandCode[, refid]},
//     "body": {"data": [items, at most 500]}} → {"code": "0", "error":
//     "false", "uid": …} with erroritem for an address of the wrong form;
//     codes 30 (credentials), 60 (brand code), 70 (parameters), 8010 (more
//     than 500 addresses), 101 (rate limit).
//   - POST /iys/search: {"header": …, "body": {"data": [{type, recipient,
//     recipientType}]}} → {"code": "0", "query": {…}} or code 50.
//   - The webhook: İYS's answer to every uploaded item (resultstatus
//     success with transactionid, or failure with errcode: H174, H175,
//     H178, H467 as İYS's rules decide) and every act a recipient performs
//     in İYS itself (Act: an "IYS_*" source). Deliveries go to the
//     WithWebhook sink right after an upload is accepted (before the
//     answer, so a test sees it at once) unless HoldResults keeps them for
//     Flush; DropWebhooks loses them (a missed delivery).
//
// Assumptions (not documented by Netgsm; to be checked with Netgsm, 00
// T-368): the search answer carries source, consentDate and creationDate
// as in Netgsm's PHP package README (WithoutDates drops them, as in the
// documentation's sample); refid is accepted in the header and in the
// item and echoed as user_refcode; the error text of an item failure
// repeats the address. Never import it outside tests.
package netgsmtest

import (
	"context"
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
	Username  = "8500000000"
	Password  = "koro-netgsm-test-password" //nolint:gosec // G101: test double credential
	IYSCode   = "700123"
	BrandCode = "700124"
	// WebhookToken is a webhook secret for the tests that use the double.
	WebhookToken = "koro-netgsm-webhook-test-token-0123456789" //nolint:gosec // G101: test double secret
)

const (
	dateLayout = "2006-01-02 15:04:05"
	maxItems   = 500
)

// Upload is one item the double accepted.
type Upload struct {
	Type, Source, Recipient, Status, ConsentDate, RecipientType string
	// RefID is the reference of the request (header or item); RefIn says
	// where it came ("header", "item", "").
	RefID, RefIn string
}

// Action is a consent act a recipient performs in İYS itself (İYS web or
// mobile, e-Devlet, 3338): it changes the record and is delivered by
// webhook.
type Action struct {
	Recipient     string
	Type          string // EPOSTA (default), MESAJ, ARAMA
	Status        string // ONAY, RET (default)
	Source        string // IYS_WEB (default) or another İYS source
	RecipientType string // BIREYSEL (default) or TACIR
}

type recordKey struct{ recipient, typ, recipientType string }

type record struct {
	status, source, txID    string
	consentDate, creationAt time.Time
}

// Server is the Netgsm double.
type Server struct {
	*httptest.Server
	clk clock.Clock

	mu         sync.Mutex
	records    map[recordKey]record
	uploads    []Upload
	sink       func(ctx context.Context, body []byte) error
	held       [][]byte
	hold       bool
	drop       bool
	down       bool
	noDates    bool
	noRef      bool
	failNext   string
	limit      int
	callTimes  []time.Time
	calls      map[string]int
	tx, submit int
	sinkErrs   []error
}

// Option configures the double.
type Option func(*Server)

// WithClock makes the double keep time with c (default: the real clock).
func WithClock(c clock.Clock) Option { return func(s *Server) { s.clk = c } }

// WithWebhook sends every webhook delivery (a JSON list) to sink.
func WithWebhook(sink func(ctx context.Context, body []byte) error) Option {
	return func(s *Server) { s.sink = sink }
}

// WithRateLimit answers code 101 after n calls within a minute.
func WithRateLimit(n int) Option { return func(s *Server) { s.limit = n } }

// WithoutDates leaves source, consentDate and creationDate out of the
// search answer (the documentation's sample).
func WithoutDates() Option { return func(s *Server) { s.noDates = true } }

// WithoutRefEcho leaves user_refcode out of İYS's answers (an upload
// whose refid Netgsm did not keep).
func WithoutRefEcho() Option { return func(s *Server) { s.noRef = true } }

// New starts the double (closed with the test).
func New(t testing.TB, opts ...Option) *Server {
	t.Helper()
	s := &Server{clk: clock.Real(), records: map[recordKey]record{}, calls: map[string]int{}}
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

// HoldResults keeps İYS's answers to uploads until Flush (true) or
// delivers them at once (false).
func (s *Server) HoldResults(hold bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hold = hold
}

// DropWebhooks loses every following webhook delivery (true).
func (s *Server) DropWebhooks(drop bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drop = drop
}

// FailNext makes İYS answer the next uploaded item with the failure code
// (e.g. "H103": an İYS system error).
func (s *Server) FailNext(code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext = code
}

// Flush delivers the held answers.
func (s *Server) Flush(ctx context.Context) {
	s.mu.Lock()
	held := s.held
	s.held = nil
	s.mu.Unlock()
	for _, b := range held {
		s.deliver(ctx, b)
	}
}

// Act records a consent act of a recipient in İYS itself, now, and
// delivers it by webhook.
func (s *Server) Act(ctx context.Context, a Action) {
	s.mu.Lock()
	k := recordKey{recipient: norm(a.Recipient), typ: def(a.Type, "EPOSTA"), recipientType: def(a.RecipientType, "BIREYSEL")}
	now := s.clk.Now().Truncate(time.Second)
	s.tx++
	rec := record{status: def(a.Status, "RET"), source: def(a.Source, "IYS_WEB"), txID: txID(s.tx), consentDate: now, creationAt: now}
	s.records[k] = rec
	body := s.webhook(webhookItem{
		ResultStatus: "success", CreationDate: format(now), TransactionID: rec.txID, Type: k.typ, Source: rec.source,
		Recipient: a.Recipient, Status: rec.status, ConsentDate: format(now), RecipientType: k.recipientType,
	})
	drop := s.drop
	s.mu.Unlock()
	if !drop {
		s.deliver(ctx, body)
	}
}

// Held returns İYS's record of recipient for typ (BIREYSEL): status,
// consent date and source.
func (s *Server) Held(recipient, typ string) (status string, consentDate time.Time, source string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[recordKey{recipient: norm(recipient), typ: typ, recipientType: "BIREYSEL"}]
	return r.status, r.consentDate, r.source, ok
}

// Uploads returns the accepted upload items.
func (s *Server) Uploads() []Upload {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Upload(nil), s.uploads...)
}

// Calls counts the requests of an operation: "add", "search".
func (s *Server) Calls(op string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[op]
}

// SinkErrors returns the errors the webhook sink answered.
func (s *Server) SinkErrors() []error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]error(nil), s.sinkErrs...)
}

func (s *Server) deliver(ctx context.Context, body []byte) {
	if s.sink == nil {
		return
	}
	if err := s.sink(ctx, body); err != nil {
		s.mu.Lock()
		s.sinkErrs = append(s.sinkErrs, err)
		s.mu.Unlock()
	}
}

type header struct {
	Username  string `json:"username"`
	Password  string `json:"password"`
	BrandCode string `json:"brandCode"`
	RefID     string `json:"refid"`
}

type addItem struct {
	Type          string `json:"type"`
	Source        string `json:"source"`
	Recipient     string `json:"recipient"`
	Status        string `json:"status"`
	ConsentDate   string `json:"consentDate"`
	RecipientType string `json:"recipientType"`
	RefID         string `json:"refid"`
}

type request[T any] struct {
	Header header `json:"header"`
	Body   struct {
		Data []T `json:"data"`
	} `json:"body"`
}

// webhookItem is one item of a delivery, in the documentation's form.
type webhookItem struct {
	IYSCode       int    `json:"iyscode"`
	BrandCode     int    `json:"brandcode"`
	SubmitID      string `json:"submitid"`
	ResultStatus  string `json:"resultstatus"`
	ErrCode       string `json:"errcode"`
	ErrMsg        string `json:"errmsg"`
	CreationDate  string `json:"creationdate"`
	UserRefCode   string `json:"user_refcode"`
	TransactionID string `json:"transactionid,omitempty"`
	Type          string `json:"type"`
	Source        string `json:"source"`
	Recipient     string `json:"recipient"`
	Status        string `json:"status"`
	ConsentDate   string `json:"consentDate"`
	RecipientType string `json:"recipientType"`
	RetailerCode  string `json:"retailerCode"`
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.down {
		s.mu.Unlock()
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"message": "service unavailable"})
		return
	}
	op := strings.TrimPrefix(r.URL.Path, "/iys/")
	s.calls[op]++
	if s.limit > 0 {
		now := s.clk.Now()
		kept := s.callTimes[:0]
		for _, t := range s.callTimes {
			if now.Sub(t) < time.Minute {
				kept = append(kept, t)
			}
		}
		s.callTimes = append(kept, now)
		if len(s.callTimes) > s.limit {
			s.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]string{"code": "101", "error": "Sistem hatası, sınır aşımı."})
			return
		}
	}
	var answer any
	var deliveries [][]byte
	switch {
	case r.Method != http.MethodPost:
		answer = map[string]string{"code": "70", "error": "Hatalı istek."}
	case op == "add":
		answer, deliveries = s.add(r)
	case op == "search":
		answer = s.search(r)
	default:
		s.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	s.mu.Unlock()
	for _, b := range deliveries {
		s.deliver(r.Context(), b)
	}
	writeJSON(w, http.StatusOK, answer)
}

func (s *Server) authorised(h header) (map[string]string, bool) {
	switch {
	case h.Username != Username || subtle.ConstantTimeCompare([]byte(h.Password), []byte(Password)) != 1:
		return map[string]string{"code": "30", "error": "Geçersiz kullanıcı adı, şifre veya API erişim izni yok."}, false
	case h.BrandCode != BrandCode:
		return map[string]string{"code": "60", "error": "Brandcode hatalı."}, false
	}
	return nil, true
}

// add applies İYS's rules to every valid item and queues its answer.
func (s *Server) add(r *http.Request) (any, [][]byte) {
	var in request[addItem]
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		return map[string]string{"code": "70", "error": "Hatalı JSON formatı."}, nil
	}
	if bad, ok := s.authorised(in.Header); !ok {
		return bad, nil
	}
	switch {
	case len(in.Body.Data) == 0:
		return map[string]string{"code": "70", "error": "data boş olamaz."}, nil
	case len(in.Body.Data) > maxItems:
		return map[string]string{"code": "8010", "error": "En fazla 500 adres yüklenebilir."}, nil
	}
	s.submit++
	uid := fmt.Sprintf("00000000-0000-4000-8000-%012d", s.submit)
	errItems := map[string]map[string]string{}
	var deliveries [][]byte
	accepted := 0
	for i, it := range in.Body.Data {
		if msg := invalid(it); msg != "" {
			errItems[strconv.Itoa(i+1)] = map[string]string{"recipient": msg + it.Recipient}
			continue
		}
		accepted++
		ref, refIn := in.Header.RefID, ""
		switch {
		case it.RefID != "":
			ref, refIn = it.RefID, "item"
		case ref != "":
			refIn = "header"
		}
		s.uploads = append(s.uploads, Upload{Type: it.Type, Source: it.Source, Recipient: it.Recipient, Status: it.Status,
			ConsentDate: it.ConsentDate, RecipientType: it.RecipientType, RefID: ref, RefIn: refIn})
		res := s.apply(it)
		res.SubmitID = uid
		if !s.noRef {
			res.UserRefCode = ref
		}
		body := s.webhook(res)
		switch {
		case s.drop:
		case s.hold:
			s.held = append(s.held, body)
		default:
			deliveries = append(deliveries, body)
		}
	}
	if accepted == 0 {
		return map[string]any{"code": "70", "error": "Parametre hatası.", "erroritem": errItems}, nil
	}
	out := map[string]any{"code": "0", "error": "false", "uid": uid}
	if len(errItems) > 0 {
		out["erroritem"] = errItems
	}
	return out, deliveries
}

// apply is İYS's processing of one item: the answer the webhook carries.
func (s *Server) apply(it addItem) webhookItem {
	at, _ := time.ParseInLocation(dateLayout, it.ConsentDate, clock.Istanbul())
	now := s.clk.Now().Truncate(time.Second)
	k := recordKey{recipient: norm(it.Recipient), typ: it.Type, recipientType: it.RecipientType}
	cur, found := s.records[k]
	out := webhookItem{Type: it.Type, Source: it.Source, Recipient: it.Recipient, Status: it.Status,
		ConsentDate: it.ConsentDate, RecipientType: it.RecipientType}
	fail := func(code, msg string) webhookItem {
		out.ResultStatus, out.ErrCode, out.ErrMsg = "failure", code, msg+" ("+it.Recipient+")"
		return out
	}
	if code := s.failNext; code != "" {
		s.failNext = ""
		return fail(code, "İYS sistem hatası.")
	}
	switch {
	case !now.Before(iys.RegistrationDeadline(at)):
		return fail("H467", "Üç iş günü öncesinde alınan izinler kaydedilemez.")
	case it.Status == "RET" && !found:
		return fail("H175", "İlk defa kaydedilen bir iznin durum (status) bilgisi RET olmamalıdır.")
	case it.Status == "RET" && cur.status == "RET":
		return fail("H174", "İzin durumu (status) güncellemesi için farklı bir durum girilmelidir. İzin durumu: RET")
	case found && at.Before(cur.consentDate):
		return fail("H178", "Sistemdeki iznin tarihinden önceki tarihli izinlerle güncelleme yapılamaz.")
	}
	s.tx++
	s.records[k] = record{status: it.Status, source: it.Source, txID: txID(s.tx), consentDate: at.UTC(), creationAt: now}
	out.ResultStatus, out.CreationDate, out.TransactionID = "success", format(now), txID(s.tx)
	return out
}

func (s *Server) search(r *http.Request) any {
	var in request[struct {
		Type          string `json:"type"`
		Recipient     string `json:"recipient"`
		RecipientType string `json:"recipientType"`
	}]
	if json.NewDecoder(r.Body).Decode(&in) != nil || len(in.Body.Data) != 1 {
		return map[string]string{"code": "70", "error": "Hatalı sorgulama."}
	}
	if bad, ok := s.authorised(in.Header); !ok {
		return bad
	}
	q := in.Body.Data[0]
	if q.Recipient == "" || (q.Type != "EPOSTA" && q.Type != "MESAJ" && q.Type != "ARAMA") ||
		(q.RecipientType != "BIREYSEL" && q.RecipientType != "TACIR") {
		return map[string]string{"code": "70", "error": "Hatalı sorgulama."}
	}
	rec, ok := s.records[recordKey{recipient: norm(q.Recipient), typ: q.Type, recipientType: q.RecipientType}]
	if !ok {
		return map[string]string{"code": "50", "error": "Kayıt Bulunamadı."}
	}
	query := map[string]any{"recipientType": q.RecipientType, "status": rec.status, "type": q.Type,
		"recipient": norm(q.Recipient), "transactionId": rec.txID}
	if !s.noDates {
		query["source"], query["consentDate"], query["creationDate"] = rec.source, format(rec.consentDate), format(rec.creationAt)
		query["retailerAccessCount"] = 0
	}
	return map[string]any{"code": "0", "error": "false", "query": query}
}

// webhook encodes one delivery of item (call with mu held).
func (s *Server) webhook(it webhookItem) []byte {
	it.IYSCode, it.BrandCode, it.RetailerCode = atoi(IYSCode), atoi(BrandCode), "0"
	b, err := json.Marshal([]webhookItem{it})
	if err != nil {
		panic(err)
	}
	return b
}

func invalid(it addItem) string {
	_, err := time.Parse(dateLayout, it.ConsentDate)
	switch {
	case it.Type != "EPOSTA" && it.Type != "MESAJ" && it.Type != "ARAMA":
		return "type hatalı: "
	case it.Status != "ONAY" && it.Status != "RET":
		return "status hatalı: "
	case it.RecipientType != "BIREYSEL" && it.RecipientType != "TACIR":
		return "recipientType hatalı: "
	case !strings.HasPrefix(it.Source, "HS_"):
		return "source hatalı: "
	case err != nil:
		return "consentDate hatalı: "
	case it.Type == "EPOSTA" && !strings.Contains(it.Recipient, "@"):
		return "E-posta adresi hatalı: "
	}
	return ""
}

func format(t time.Time) string { return t.In(clock.Istanbul()).Format(dateLayout) }

// norm is İYS's address form: e-mail addresses are kept in lower case.
func norm(a string) string {
	if strings.Contains(a, "@") {
		return strings.ToLower(strings.TrimSpace(a))
	}
	return strings.TrimSpace(a)
}

func def(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func txID(n int) string { return fmt.Sprintf("%064x", n) }

func atoi(s string) int {
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
	}
	return n
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
