// Package problem is the error model of Koro (docs/design/05 §4, 04 §1.10, 06 H-14).
//
// Services return *Error values (sentinels declared with New, optionally wrapped
// or enriched with field errors); handlers return them unchanged; the HTTP layer
// (internal/platform/httpx) converts them into RFC 9457 application/problem+json
// responses exactly once. Unknown errors never reach the client: they become a
// 500 internal_error without their text.
//
// The package is a leaf: it imports only the standard library, so authz and
// every feature module can depend on it.
package problem

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultTypeBase is the default prefix of the RFC 9457 "type" member
// (PROBLEM_TYPE_BASE; 05 §4.4). The full type is base + code.
const DefaultTypeBase = "urn:koro:problem:"

// Kind classifies an error and determines its HTTP status.
type Kind uint8

// Error kinds (05 §4.2 plus the platform-wide precondition codes of 04 §1.13).
const (
	Invalid              Kind = iota + 1 // 422 business rule / field validation
	Unauthenticated                      // 401
	Forbidden                            // 403
	NotFound                             // 404 missing OR not visible (IDOR rule, 05 §5.4)
	Conflict                             // 409 uniqueness, state, capacity
	Gone                                 // 410
	PreconditionFailed                   // 412 If-Match mismatch
	PreconditionRequired                 // 428 If-Match missing
	RateLimited                          // 429
	Unavailable                          // 503 dependency unavailable
	Internal                             // 500 unexpected; details never reach the client
)

// Status returns the HTTP status code of k.
func (k Kind) Status() int {
	switch k {
	case Invalid:
		return http.StatusUnprocessableEntity
	case Unauthenticated:
		return http.StatusUnauthorized
	case Forbidden:
		return http.StatusForbidden
	case NotFound:
		return http.StatusNotFound
	case Conflict:
		return http.StatusConflict
	case Gone:
		return http.StatusGone
	case PreconditionFailed:
		return http.StatusPreconditionFailed
	case PreconditionRequired:
		return http.StatusPreconditionRequired
	case RateLimited:
		return http.StatusTooManyRequests
	case Unavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// FieldError points at an invalid input location. It deliberately has no
// value member: input values (passwords included) are never echoed back.
// Code is optional (BE-140): an entry that is a refusal of its own — one
// line of a cart refused for another reason than the problem's — carries
// that refusal's catalogued code, so a client translates each entry. An
// entry without one is read under the problem's code; a code that is not in
// the catalog (problems.json) never reaches the client (FromError).
type FieldError struct {
	Location string `json:"location,omitempty" doc:"Where the error occurred, e.g. 'body.email' or 'path.org_id'"`
	Message  string `json:"message" doc:"Error message text"`
	Code     string `json:"code,omitempty" doc:"Stable code of this entry's own refusal, always one of problems.json (BE-140); absent: read the problem's code" example:"catalog.sold_out"`
}

// Error is a typed application error with a stable machine-readable code.
// Declare sentinels once at package level with New; derive variants with
// Wrap and WithFields (both return copies and never mutate the sentinel).
type Error struct {
	kind       Kind
	code       string
	title      string
	fields     []FieldError
	cause      error
	retryAfter time.Duration
}

// Error returns the title and, for logs, the wrapped cause. Clients never
// see this text: the HTTP layer renders only the code and the title
// (FromError).
func (e *Error) Error() string {
	if e.cause != nil {
		return e.title + ": " + e.cause.Error()
	}
	return e.title
}

// Unwrap returns the wrapped cause (log only).
func (e *Error) Unwrap() error { return e.cause }

// Is reports whether target is an *Error with the same code.
func (e *Error) Is(target error) bool {
	var t *Error
	if !errors.As(target, &t) {
		return false
	}
	return t.code == e.code
}

// Kind returns the error kind.
func (e *Error) Kind() Kind { return e.kind }

// Code returns the stable code ("validation_failed", "events.capacity_full").
func (e *Error) Code() string { return e.code }

// Title returns the fixed English title of the code.
func (e *Error) Title() string { return e.title }

// Status returns the HTTP status of the error kind.
func (e *Error) Status() int { return e.kind.Status() }

// Fields returns a copy of the field errors.
func (e *Error) Fields() []FieldError { return slices.Clone(e.fields) }

// At is a field error at location that is e itself (BE-140): e's title as
// the message and e's code, so the entry keeps its own code when it is
// merged into another problem's errors[] (a cart of several refusals).
func (e *Error) At(location string) FieldError {
	return FieldError{Location: location, Message: e.title, Code: e.code}
}

// CodedFields returns a copy of the field errors in which every entry that
// carries no code of its own carries e's (BE-140): the entries of e, ready
// to be merged with another problem's under that problem's code without
// losing why each was refused.
func (e *Error) CodedFields() []FieldError {
	f := e.Fields()
	for i := range f {
		if f[i].Code == "" {
			f[i].Code = e.code
		}
	}
	return f
}

// Wrap returns a copy of e carrying cause for logs.
func (e *Error) Wrap(cause error) *Error {
	c := *e
	c.cause = cause
	return &c
}

// WithFields returns a copy of e with the given field errors.
func (e *Error) WithFields(f ...FieldError) *Error {
	c := *e
	c.fields = slices.Clone(f)
	return &c
}

// WithRetryAfter returns a copy of e that tells the client to wait d before
// trying again (BE-178): the HTTP layer sends it as the Retry-After header
// in whole seconds (RetryAfterSeconds). For a 429 that comes from a
// limiter, d is the limiter's own remaining wait. d <= 0 removes it.
func (e *Error) WithRetryAfter(d time.Duration) *Error {
	c := *e
	c.retryAfter = max(d, 0)
	return &c
}

// RetryAfter returns the wait set with WithRetryAfter (0: none).
func (e *Error) RetryAfter() time.Duration { return e.retryAfter }

// GetHeaders returns the response headers e carries: Retry-After when
// WithRetryAfter set a wait, nil otherwise. huma's handler path sets the
// headers of a returned error that has this method (huma.HeadersError,
// matched structurally: this package does not import huma).
func (e *Error) GetHeaders() http.Header {
	if e.retryAfter <= 0 {
		return nil
	}
	return http.Header{"Retry-After": {RetryAfterSeconds(e.retryAfter)}}
}

// RetryAfterSeconds formats a wait as the delay-seconds form of
// Retry-After (RFC 9110 §10.2.3): whole seconds, rounded up, at least 1.
// The rate limit middlewares and WithRetryAfter share it.
func RetryAfterSeconds(d time.Duration) string {
	return strconv.Itoa(max(1, int(math.Ceil(d.Seconds()))))
}

// IsClientError reports whether the HTTP layer answers err with a 4xx
// status: the first *Error of its chain (the one httpx renders) has a
// client kind. Such an error is a decision about the request
// (validation, authorization, a refused re-authentication proof, a rate
// limit). nil, a 5xx *Error and an error holding none (a context deadline,
// a driver error) are not: the server could not decide, for instance
// because the request ran out of time (05 §10.6; httpx answers 503 or
// 500). Callers that record a verdict (the denied audit row of a refused
// re-authentication, BE-072) record it only for a client error.
func IsClientError(err error) bool {
	pe, ok := errors.AsType[*Error](err)
	return ok && pe.Status() < http.StatusInternalServerError
}

// Entry is one row of the problem catalog (openapi/problems.json).
type Entry struct {
	Code   string `json:"code"`
	Status int    `json:"status"`
	Title  string `json:"title"`
	Type   string `json:"type"`
}

var (
	codeRe = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)?$`)

	catalogMu sync.Mutex
	catalog   = map[string]Entry{}
)

// New declares an error code and returns its sentinel. Codes are
// "<module>.<snake_case>" (or prefix-less platform codes) and must be unique:
// a duplicate or malformed code panics at process start.
func New(k Kind, code, title string) *Error {
	if k < Invalid || k > Internal {
		panic(fmt.Sprintf("problem: invalid kind %d for code %q", k, code))
	}
	register(code, k.Status(), title)
	return &Error{kind: k, code: code, title: title}
}

func register(code string, status int, title string) bool {
	if !codeRe.MatchString(code) {
		panic(fmt.Sprintf("problem: malformed code %q", code))
	}
	if strings.TrimSpace(title) == "" {
		panic(fmt.Sprintf("problem: empty title for code %q", code))
	}
	catalogMu.Lock()
	defer catalogMu.Unlock()
	if _, dup := catalog[code]; dup {
		panic(fmt.Sprintf("problem: duplicate code %q", code))
	}
	catalog[code] = Entry{Code: code, Status: status, Title: title}
	return true
}

// Catalog returns every declared code sorted by code, with Type built from base.
func Catalog(base string) []Entry {
	catalogMu.Lock()
	out := make([]Entry, 0, len(catalog))
	for _, e := range catalog {
		e.Type = base + e.Code
		out = append(out, e)
	}
	catalogMu.Unlock()
	slices.SortFunc(out, func(a, b Entry) int { return strings.Compare(a.Code, b.Code) })
	return out
}

// lookup returns the catalog entry of code.
func lookup(code string) (Entry, bool) {
	catalogMu.Lock()
	defer catalogMu.Unlock()
	e, ok := catalog[code]
	return e, ok
}
