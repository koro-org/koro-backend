package netgsm_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"koro/internal/platform/clock"
	"koro/internal/platform/netgsmapi"
	"koro/internal/platform/sms"
	"koro/internal/platform/sms/netgsm"
	"koro/internal/platform/sms/smscontract"
)

// Test credentials of the double (no real account).
const (
	testUser   = "8500000000"
	testPass   = "test-api-user-password"
	testHeader = "KORO"
)

// double is Netgsm's OTP route over httptest: it checks the call like
// Netgsm would and answers the queued outcome (or success).
type double struct {
	mu        sync.Mutex
	srv       *httptest.Server
	delivered []sms.Message
	bodies    []map[string]any
	next      []smscontract.Outcome
	raw       []string // a raw answer queued for the next call (tests of odd answers)
}

func newDouble(t *testing.T) *double {
	t.Helper()
	d := &double{}
	d.srv = httptest.NewServer(http.HandlerFunc(d.serve))
	t.Cleanup(d.srv.Close)
	return d
}

func (d *double) serve(w http.ResponseWriter, r *http.Request) {
	user, pass, ok := r.BasicAuth()
	if r.Method != http.MethodPost || r.URL.Path != netgsm.OTPPath || !ok || user != testUser || pass != testPass {
		w.WriteHeader(http.StatusNotAcceptable)
		_, _ = io.WriteString(w, `{"code":"30","description":"Gecersiz kullanici adi"}`)
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusNotAcceptable)
		_, _ = io.WriteString(w, `{"code":"70"}`)
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.bodies = append(d.bodies, body)
	no, _ := body["no"].(string)
	if len(d.raw) > 0 {
		ans := d.raw[0]
		d.raw = d.raw[1:]
		_, _ = io.WriteString(w, ans)
		return
	}
	if len(d.next) > 0 {
		o := d.next[0]
		d.next = d.next[1:]
		switch o {
		case smscontract.BadNumber: // Netgsm's free text repeats the number: never echoed
			w.WriteHeader(http.StatusNotAcceptable)
			_, _ = io.WriteString(w, `{"code":"50","description":"Numara `+no+` hatali"}`)
		case smscontract.Throttled:
			_, _ = io.WriteString(w, `{"code":"101","description":"Sistem hatasi"}`)
		case smscontract.Refused:
			w.WriteHeader(http.StatusNotAcceptable)
			_, _ = io.WriteString(w, `{"code":"60","description":"OTP paketi yok"}`)
		case smscontract.Down:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		return
	}
	text, _ := body["msg"].(string)
	d.delivered = append(d.delivered, sms.Message{To: "+90" + no, Text: text})
	_, _ = io.WriteString(w, `{"code":"00","jobid":"1700000000","description":"queued"}`)
}

func (d *double) Delivered() []sms.Message {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]sms.Message(nil), d.delivered...)
}

func (d *double) Next(o smscontract.Outcome) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.next = append(d.next, o)
}

func (d *double) answerNext(raw string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.raw = append(d.raw, raw)
}

func newClient(t *testing.T, d *double, mutate ...func(*netgsm.Config)) *netgsm.Client {
	t.Helper()
	c := netgsm.Config{
		Account: netgsmapi.Account{BaseURL: d.srv.URL, Username: testUser, Password: testPass, HTTPClient: d.srv.Client()},
		Header:  testHeader, AppName: "koro",
	}
	for _, m := range mutate {
		m(&c)
	}
	cl, err := netgsm.New(c)
	if err != nil {
		t.Fatal(err)
	}
	return cl
}

// TestNetgsmSMS_Contract: the Netgsm adapter passes the SMS contract over
// its double.
func TestNetgsmSMS_Contract(t *testing.T) {
	t.Parallel()
	smscontract.Run(t, func(t *testing.T) smscontract.Harness {
		d := newDouble(t)
		return smscontract.Harness{Adapter: newClient(t, d), Delivered: d.Delivered, Next: d.Next}
	})
}

// TestNetgsmSMS_RequestFormat pins the call: the OTP route, Basic auth and
// the body fields (the national number, the approved header, the app name).
func TestNetgsmSMS_RequestFormat(t *testing.T) {
	t.Parallel()
	d := newDouble(t)
	rc, err := newClient(t, d).Send(t.Context(), sms.Message{To: smscontract.Number, Text: smscontract.Text, Ref: "r1"})
	if err != nil || rc.ID != "1700000000" {
		t.Fatalf("Send = %+v, %v", rc, err)
	}
	b := d.bodies[0]
	if b["no"] != "5550000001" || b["msgheader"] != testHeader || b["msg"] != smscontract.Text || b["appname"] != "koro" || len(b) != 4 {
		t.Fatalf("body = %v", b)
	}
}

// TestNetgsmSMS_OddAnswers: a numeric code alone, the jobId spelling of the
// SDK, an unknown code (transient, the code only), a success without a job
// ID and a non-JSON answer.
func TestNetgsmSMS_OddAnswers(t *testing.T) {
	t.Parallel()
	d := newDouble(t)
	c := newClient(t, d)
	m := sms.Message{To: smscontract.Number, Text: smscontract.Text, Ref: "r1"}
	d.answerNext(`{"code":0,"jobId":42}`)
	if rc, err := c.Send(t.Context(), m); err != nil || rc.ID != "42" {
		t.Fatalf("jobId answer: %+v, %v", rc, err)
	}
	d.answerNext(`30`)
	if _, err := c.Send(t.Context(), m); !errors.Is(err, sms.ErrRejected) {
		t.Fatalf("bare code 30: %v", err)
	}
	d.answerNext(`{"code":"777","description":"5550000001 bilinmeyen"}`)
	_, err := c.Send(t.Context(), m)
	if err == nil || errors.Is(err, sms.ErrRejected) || errors.Is(err, sms.ErrThrottled) ||
		err.Error() != "sms netgsm: code 777 (unknown Netgsm code)" {
		t.Fatalf("unknown code: %v", err)
	}
	d.answerNext(`{"code":"00"}`)
	if _, err := c.Send(t.Context(), m); err == nil {
		t.Fatal("success without a job ID accepted")
	}
	d.answerNext(`<html>5550000001</html>`)
	if _, err := c.Send(t.Context(), m); err == nil || strings.Contains(err.Error(), "5550000001") {
		t.Fatalf("non-JSON answer: %v", err)
	}
}

// TestNetgsmSMS_Budget: a spent per-minute budget is ErrThrottled without a
// call; it refills after a minute.
func TestNetgsmSMS_Budget(t *testing.T) {
	t.Parallel()
	d := newDouble(t)
	clk := clock.NewFake(time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC))
	c := newClient(t, d, func(c *netgsm.Config) { c.RequestsPerMinute, c.Clock = 2, clk })
	m := sms.Message{To: smscontract.Number, Text: smscontract.Text, Ref: "r1"}
	for range 2 {
		if _, err := c.Send(t.Context(), m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Send(t.Context(), m); !errors.Is(err, sms.ErrThrottled) {
		t.Fatalf("third call: %v", err)
	}
	if n := len(d.Delivered()); n != 2 {
		t.Fatalf("%d calls reached Netgsm", n)
	}
	clk.Advance(time.Minute)
	if _, err := c.Send(t.Context(), m); err != nil {
		t.Fatalf("after a minute: %v", err)
	}
}

// TestNetgsmSMS_ConfigValidation: the account, the header and the budget
// are checked at construction; no error repeats a credential.
func TestNetgsmSMS_ConfigValidation(t *testing.T) {
	t.Parallel()
	ok := netgsm.Config{Account: netgsmapi.Account{Username: testUser, Password: testPass}, Header: testHeader}
	c, err := netgsm.New(ok)
	if err != nil || c.Name() != "netgsm" {
		t.Fatalf("New(defaults) = %v, %v", c, err)
	}
	for name, mutate := range map[string]func(*netgsm.Config){
		"no password":    func(c *netgsm.Config) { c.Account.Password = "" },
		"no username":    func(c *netgsm.Config) { c.Account.Username = "" },
		"base with user": func(c *netgsm.Config) { c.Account.BaseURL = "https://u:p@api.netgsm.com.tr" },
		"base ftp":       func(c *netgsm.Config) { c.Account.BaseURL = "ftp://api.netgsm.com.tr" },
		"base query":     func(c *netgsm.Config) { c.Account.BaseURL = "https://api.netgsm.com.tr/?x=1" },
		"no header":      func(c *netgsm.Config) { c.Header = "" },
		"long header":    func(c *netgsm.Config) { c.Header = "KORO-PLATFORMU" },
		"header symbols": func(c *netgsm.Config) { c.Header = "KORO!" },
		"budget":         func(c *netgsm.Config) { c.RequestsPerMinute = -1 },
	} {
		cfg := ok
		mutate(&cfg)
		_, err := netgsm.New(cfg)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), testPass) {
			t.Errorf("%s: the error repeats the password", name)
		}
	}
}
