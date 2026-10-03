package problem_test

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"koro/internal/platform/problem"
)

var errTestCapacity = problem.New(problem.Conflict, "testmod.capacity_full", "Event is full")

func TestError_WrapAndWithFields_KeepSentinelIntact(t *testing.T) {
	t.Parallel()
	cause := errors.New("pq: duplicate key 10.0.0.5")
	wrapped := errTestCapacity.Wrap(cause).WithFields(problem.FieldError{Location: "body.x", Message: "bad"})

	if !errors.Is(wrapped, errTestCapacity) {
		t.Fatal("wrapped error must match its sentinel")
	}
	if !errors.Is(wrapped, cause) {
		t.Fatal("cause must stay reachable for logs")
	}
	if len(errTestCapacity.Fields()) != 0 {
		t.Fatal("WithFields mutated the sentinel")
	}
	if errors.Unwrap(errTestCapacity) != nil {
		t.Fatal("Wrap mutated the sentinel")
	}
	if got := wrapped.Error(); got != "Event is full: pq: duplicate key 10.0.0.5" {
		t.Fatalf("Error() = %q, want title + cause for logs", got)
	}
	if p := problem.FromError(wrapped, problem.DefaultTypeBase); strings.Contains(fmt.Sprintf("%+v", *p), "10.0.0.5") {
		t.Fatalf("response body leaks the cause: %+v", *p)
	}
	if errors.Is(wrapped, problem.ErrConflict) {
		t.Fatal("different codes must not match")
	}
}

func TestNew_DuplicateOrMalformedCode_Panics(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		code string
	}{
		{"duplicate", "testmod.capacity_full"},
		{"upper case", "testmod.Bad"},
		{"three segments", "a.b.c"},
		{"empty", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Fatalf("New(%q) did not panic", tt.code)
				}
			}()
			problem.New(problem.Invalid, tt.code, "x")
		})
	}
}

func TestCatalog_SortedWithTypeBase(t *testing.T) {
	t.Parallel()
	cat := problem.Catalog(problem.DefaultTypeBase)
	if !slices.IsSortedFunc(cat, func(a, b problem.Entry) int { return strings.Compare(a.Code, b.Code) }) {
		t.Fatal("catalog is not sorted by code")
	}
	want := map[string]int{
		"validation_failed": http.StatusUnprocessableEntity,
		"internal_error":    http.StatusInternalServerError,
		"not_found":         http.StatusNotFound,
		"csrf_rejected":     http.StatusForbidden,
		"payload_too_large": http.StatusRequestEntityTooLarge,
	}
	for _, e := range cat {
		if e.Type != problem.DefaultTypeBase+e.Code {
			t.Fatalf("entry %q has type %q", e.Code, e.Type)
		}
		if st, ok := want[e.Code]; ok {
			if st != e.Status {
				t.Fatalf("entry %q status = %d, want %d", e.Code, e.Status, st)
			}
			delete(want, e.Code)
		}
	}
	if len(want) != 0 {
		t.Fatalf("catalog misses %v", want)
	}
}

func TestFromError_InternalHasNoFields(t *testing.T) {
	t.Parallel()
	e := problem.ErrInternal.Wrap(fmt.Errorf("db: %w", errors.New("secret detail"))).
		WithFields(problem.FieldError{Location: "body.x", Message: "leak"})
	p := problem.FromError(e, "urn:test:")
	if p.Status != http.StatusInternalServerError || p.Code != "internal_error" || p.Type != "urn:test:internal_error" {
		t.Fatalf("unexpected problem %+v", p)
	}
	if len(p.Errors) != 0 || p.Detail != "" {
		t.Fatalf("internal problem must not carry details: %+v", p)
	}
}

func TestCodeForStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		status int
		want   string
	}{
		{http.StatusBadRequest, "bad_request"},
		{http.StatusUnauthorized, "unauthenticated"},
		{http.StatusNotFound, "not_found"},
		{http.StatusMethodNotAllowed, "method_not_allowed"},
		{http.StatusRequestEntityTooLarge, "payload_too_large"},
		{http.StatusUnprocessableEntity, "validation_failed"},
		{http.StatusTooManyRequests, "rate_limited"},
		{http.StatusTeapot, "bad_request"},
		{http.StatusServiceUnavailable, "unavailable"},
		{http.StatusBadGateway, "internal_error"},
		{0, "internal_error"},
	}
	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			t.Parallel()
			if got := problem.CodeForStatus(tt.status); got != tt.want {
				t.Fatalf("CodeForStatus(%d) = %q, want %q", tt.status, got, tt.want)
			}
		})
	}
}
