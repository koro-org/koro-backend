// Package netgsmapi is what Koro's Netgsm adapters share (BE-172): the
// subscriber's API access (base URL, usercode, the API sub-user's password),
// the HTTP client that talks to it, and the hygiene of Netgsm's answers.
// The İYS adapter (internal/platform/iys/netgsm, BE-110) and the SMS adapter
// (internal/platform/sms/netgsm, BE-172) use one Netgsm account each; the
// rules below are written once here so the two cannot drift:
//
//   - The base URL is an http(s) URL without credentials, query or fragment
//     (a path prefix is kept); DefaultBaseURL when empty.
//   - The client never follows a redirect (the credentials would follow it)
//     and bounds every request (Timeout, default 10s).
//   - A transport error is scrubbed (Scrub) before it reaches an error or a
//     log line: it may repeat an address or a phone number of the request.
//   - Netgsm sends codes and IDs as strings or numbers (Flex); a code is only
//     ever repeated when it is a plain number (SafeCode), never Netgsm's free
//     text.
//   - Budget is the sliding-window call budget of an adapter (Netgsm limits
//     calls per minute per account).
package netgsmapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"koro/internal/platform/obs"
)

// DefaultBaseURL is Netgsm's API.
const DefaultBaseURL = "https://api.netgsm.com.tr"

// DefaultTimeout bounds one request when the account sets none.
const DefaultTimeout = 10 * time.Second

// Errors of Account.
var (
	// ErrBaseURL: the base URL is not an http(s) URL without credentials or
	// query.
	ErrBaseURL = errors.New("must be an http(s) URL without credentials or query")
	// ErrCredentials: the usercode or the password is missing.
	ErrCredentials = errors.New("the Netgsm usercode and password are required")
)

// Account is a Netgsm subscriber's API access.
type Account struct {
	// BaseURL of Netgsm's API (DefaultBaseURL when empty; a path prefix is
	// kept, a trailing slash dropped).
	BaseURL string
	// Username is the subscriber number (usercode, 850xxxxxxx); Password
	// the password of its "API user" sub-user.
	Username string
	Password string
	// Timeout bounds one request (DefaultTimeout when ≤ 0).
	Timeout time.Duration
	// HTTPClient replaces the client built from Timeout (tests).
	HTTPClient *http.Client
}

// Base returns the validated base URL without a trailing slash; ErrBaseURL
// when it is not usable, ErrCredentials when the usercode or the password
// is missing.
func (a Account) Base() (string, error) {
	raw := strings.TrimSpace(a.BaseURL)
	if raw == "" {
		raw = DefaultBaseURL
	}
	base := strings.TrimRight(raw, "/")
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", ErrBaseURL
	}
	if a.Username == "" || a.Password == "" {
		return base, ErrCredentials
	}
	return base, nil
}

// Client returns HTTPClient, or a client bounded by Timeout that never
// follows a redirect.
func (a Account) Client() *http.Client {
	if a.HTTPClient != nil {
		return a.HTTPClient
	}
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// TransportError returns the text of a failed request without the URL
// (url.Error repeats it) and scrubbed.
func TransportError(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return Scrub(err.Error())
}

// Scrub removes addresses, phone numbers and control characters from a
// transport error and bounds it to 300 characters.
func Scrub(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, obs.Scrub(s))
	if r := []rune(s); len(r) > 300 {
		s = string(r[:300])
	}
	return s
}

var reNumCode = regexp.MustCompile(`^-?[0-9]{1,6}$`)

// IsNumCode reports whether b is a bare numeric code (Netgsm answers some
// calls with the code alone).
func IsNumCode(b []byte) bool { return reNumCode.Match(b) }

// SafeCode is code when it is a plain number ("?" otherwise: an answer's
// text is never repeated).
func SafeCode(code string) string {
	if !reNumCode.MatchString(code) {
		return "?"
	}
	return code
}

// Flex is a JSON scalar read as text: Netgsm sends codes and IDs as
// strings or numbers, flags as strings or booleans. An object or an array
// reads as "".
type Flex string

// UnmarshalJSON implements json.Unmarshaler.
func (f *Flex) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	switch {
	case len(b) == 0 || bytes.Equal(b, []byte("null")):
		*f = ""
	case b[0] == '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = Flex(s)
	case b[0] == '{' || b[0] == '[':
		*f = "" // not a scalar: ignored
	default:
		*f = Flex(b)
	}
	return nil
}

// Budget is a sliding-window call budget: at most N calls per Per. The zero
// value refuses everything; it is safe for concurrent use.
type Budget struct {
	N   int
	Per time.Duration

	mu    sync.Mutex
	calls []time.Time
}

// Take spends one call at now; false when the window is full.
func (b *Budget) Take(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.calls[:0]
	for _, t := range b.calls {
		if now.Sub(t) < b.Per {
			out = append(out, t)
		}
	}
	b.calls = out
	if len(b.calls) >= b.N {
		return false
	}
	b.calls = append(b.calls, now)
	return true
}
