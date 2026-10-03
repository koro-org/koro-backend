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
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
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
type FieldError struct {
	Location string `json:"location,omitempty" doc:"Where the error occurred, e.g. 'body.email' or 'path.org_id'"`
	Message  string `json:"message" doc:"Error message text"`
}

// Error is a typed application error with a stable machine-readable code.
// Declare sentinels once at package level with New; derive variants with
// Wrap and WithFields (both return copies and never mutate the sentinel).
type Error struct {
	kind   Kind
	code   string
	title  string
	fields []FieldError
	cause  error
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
