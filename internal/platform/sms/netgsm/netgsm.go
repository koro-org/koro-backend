// Package netgsm is the SMS adapter over Netgsm's OTP SMS API (BE-172; the
// pilot's provider: the İYS integrator is Netgsm too, BE-110). Sources:
// Netgsm's API documentation (https://www.netgsm.com.tr/dokuman/, "OTP
// SMS", REST v2) and its SDK @netgsm/sms 1.1.11 (read 2026-11-05):
//
//	POST {base}/sms/rest/v2/otp   Authorization: Basic base64(usercode:password)
//	                              {"msgheader": "<sender name>", "msg": "<text>", "no": "5XXXXXXXXX"[, "appname": "…"]}
//	                              → {"code": "00", "jobid": "…", "description": "…"} (HTTP 200; refusals
//	                              carry their code, also with HTTP 406)
//
// The OTP route is Netgsm's priority route for one-time codes: one
// recipient per call, a short ASCII text (no Turkish characters), a sender
// name (msgheader) approved on the account and an OTP package on the
// account.
//
// Rules:
//
//   - Netgsm's codes: 00 success; 50, 51, 52 (the number) are
//     sms.ErrInvalidNumber; 20 (text), 30 (credentials, API access, IP
//     restriction), 40 and 41 (sender name), 60 (no OTP credit) and 70
//     (parameters) are sms.ErrRejected; 100 and 101 (system error) and HTTP
//     429 are sms.ErrThrottled; anything else, HTTP 5xx and transport
//     errors are transient.
//   - Budget (per process): RequestsPerMinute calls (default 60); a spent
//     budget is sms.ErrThrottled without a call. Netgsm's own OTP limits
//     are not documented (an open question of BE-172, 00).
//   - Every error carries Netgsm's code or the HTTP status, never the
//     number, the text, a credential or Netgsm's free text (description).
//   - The account (base URL, usercode, password, HTTP client) is
//     netgsmapi.Account, shared with the İYS adapter.
package netgsm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"koro/internal/platform/clock"
	"koro/internal/platform/netgsmapi"
	"koro/internal/platform/obs"
	"koro/internal/platform/sms"
)

// Defaults and bounds.
const (
	// OTPPath is the OTP route of Netgsm's REST v2 API.
	OTPPath = "/sms/rest/v2/otp"
	// DefaultRequestsPerMinute bounds the calls of one process.
	DefaultRequestsPerMinute = 60
	maxBody                  = 64 << 10
)

// reHeader is Netgsm's sender name: 2–11 letters, digits, spaces, dots or
// dashes (the name approved on the account).
var reHeader = regexp.MustCompile(`^[A-Za-z0-9 .\-]{2,11}$`)

// Config configures the adapter.
type Config struct {
	// Account is the Netgsm subscriber's API access (shared rules with the
	// İYS adapter: netgsmapi).
	Account netgsmapi.Account
	// Header is the approved sender name (msgheader).
	Header string
	// AppName is Netgsm's optional application label ("" sends none).
	AppName string
	// RequestsPerMinute bounds the calls (default 60).
	RequestsPerMinute int
	// Clock times the budget (default clock.Real()).
	Clock clock.Clock
	// Log receives one debug line per call (HTTP status, Netgsm's code,
	// the caller's reference); nil discards.
	Log *slog.Logger
}

// Client is the adapter.
type Client struct {
	base   string
	user   string
	pass   string
	header string
	app    string
	http   *http.Client
	clock  clock.Clock
	log    *slog.Logger
	budget *netgsmapi.Budget
}

// New validates c.
func New(c Config) (*Client, error) {
	base, err := c.Account.Base()
	switch {
	case errors.Is(err, netgsmapi.ErrBaseURL):
		return nil, fmt.Errorf("sms netgsm: SMS_NETGSM_BASE_URL %w", err)
	case err != nil:
		return nil, errors.New("sms netgsm: SMS_NETGSM_USERNAME and SMS_NETGSM_PASSWORD are required")
	case !reHeader.MatchString(c.Header):
		return nil, errors.New("sms netgsm: SMS_NETGSM_HEADER must be the approved sender name (2-11 letters, digits, spaces, dots or dashes)")
	case c.RequestsPerMinute < 0 || c.RequestsPerMinute > 6000:
		return nil, errors.New("sms netgsm: RequestsPerMinute must be between 0 (default) and 6000")
	}
	if c.RequestsPerMinute == 0 {
		c.RequestsPerMinute = DefaultRequestsPerMinute
	}
	if c.Clock == nil {
		c.Clock = clock.Real()
	}
	if c.Log == nil {
		c.Log = obs.Discard()
	}
	return &Client{base: base, user: c.Account.Username, pass: c.Account.Password, header: c.Header,
		app: strings.TrimSpace(c.AppName), http: c.Account.Client(), clock: c.Clock, log: c.Log,
		budget: &netgsmapi.Budget{N: c.RequestsPerMinute, Per: time.Minute}}, nil
}

// Name names the adapter (SMS_PROVIDER).
func (*Client) Name() string { return "netgsm" }

// request is the body of the OTP route.
type request struct {
	Header  string `json:"msgheader"`
	Text    string `json:"msg"`
	No      string `json:"no"`
	AppName string `json:"appname,omitempty"`
}

// answer is Netgsm's answer (description is never read: it is free text).
type answer struct {
	Code   netgsmapi.Flex `json:"code"`
	JobID  netgsmapi.Flex `json:"jobid"`
	JobID2 netgsmapi.Flex `json:"jobId"`
}

// Send sends one message by Netgsm's OTP route.
func (c *Client) Send(ctx context.Context, m sms.Message) (sms.Receipt, error) {
	if err := sms.CheckMessage(m); err != nil {
		return sms.Receipt{}, err
	}
	if err := ctx.Err(); err != nil {
		return sms.Receipt{}, err
	}
	if !c.budget.Take(c.clock.Now()) {
		return sms.Receipt{}, fmt.Errorf("%w: Netgsm call budget of %d per minute spent", sms.ErrThrottled, c.budget.N)
	}
	raw, err := json.Marshal(request{Header: c.header, Text: m.Text, No: sms.National(m.To), AppName: c.app})
	if err != nil {
		return sms.Receipt{}, fmt.Errorf("sms netgsm: encode: %w", err)
	}
	defer clear(raw) // holds the number and the code
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+OTPPath, bytes.NewReader(raw))
	if err != nil {
		return sms.Receipt{}, errors.New("sms netgsm: cannot build the request")
	}
	req.SetBasicAuth(c.user, c.pass)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return sms.Receipt{}, fmt.Errorf("sms netgsm: %s", netgsmapi.TransportError(err))
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	defer clear(data)
	var a answer
	decoded := json.Unmarshal(bytes.TrimSpace(data), &a) == nil
	if !decoded && netgsmapi.IsNumCode(bytes.TrimSpace(data)) {
		a, decoded = answer{Code: netgsmapi.Flex(bytes.TrimSpace(data))}, true
	}
	code := strings.TrimSpace(string(a.Code))
	c.log.LogAttrs(ctx, slog.LevelDebug, "sms netgsm call", slog.Int("status", resp.StatusCode),
		slog.String("code", netgsmapi.SafeCode(code)), slog.String("ref", m.Ref))
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return sms.Receipt{}, &apiError{status: resp.StatusCode, class: []error{sms.ErrThrottled}}
	case resp.StatusCode >= 500:
		return sms.Receipt{}, &apiError{status: resp.StatusCode}
	case !decoded || code == "":
		if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusNotAcceptable {
			return sms.Receipt{}, &apiError{status: resp.StatusCode, class: []error{sms.ErrRejected}}
		}
		return sms.Receipt{}, errors.New("sms netgsm: malformed answer")
	case code == "00" || code == "0":
		id := string(a.JobID)
		if id == "" {
			id = string(a.JobID2)
		}
		if id == "" {
			return sms.Receipt{}, errors.New("sms netgsm: success answer without a job ID")
		}
		return sms.Receipt{ID: id}, nil
	}
	return sms.Receipt{}, refusal(code)
}

// refusal maps Netgsm's code of a refused message.
func refusal(code string) error {
	rejected := []error{sms.ErrRejected}
	switch code {
	case "50", "51", "52":
		return &apiError{code: code, text: "the number was refused", class: []error{sms.ErrRejected, sms.ErrInvalidNumber}}
	case "20":
		return &apiError{code: code, text: "the text was refused", class: rejected}
	case "30":
		return &apiError{code: code, text: "credentials, API access or IP restriction", class: rejected}
	case "40", "41":
		return &apiError{code: code, text: "the sender name is not approved", class: rejected}
	case "60":
		return &apiError{code: code, text: "no OTP credit on the account", class: rejected}
	case "70":
		return &apiError{code: code, text: "parameter error", class: rejected}
	case "100", "101":
		return &apiError{code: code, text: "Netgsm system error", class: []error{sms.ErrThrottled}}
	}
	return &apiError{code: netgsmapi.SafeCode(code), text: "unknown Netgsm code"}
}

// apiError is a refused call: Netgsm's code (or the HTTP status) and a
// fixed text; it unwraps to the port errors of its class.
type apiError struct {
	status int
	code   string
	text   string
	class  []error
}

func (e *apiError) Error() string {
	s := "sms netgsm: "
	if e.status != 0 {
		s += fmt.Sprintf("HTTP %d", e.status)
	} else {
		s += "code " + e.code
	}
	if e.text != "" {
		s += " (" + e.text + ")"
	}
	return s
}

func (e *apiError) Unwrap() []error { return e.class }
