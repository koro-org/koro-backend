// Package problemtest holds the error assertions of module tests. It is
// imported only by tests.
package problemtest

import (
	"errors"
	"testing"

	"koro/internal/platform/problem"
)

// WantErr fails the test unless errors.Is(err, want).
func WantErr(t testing.TB, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

// StatusCode returns the HTTP status and the code of the first
// *problem.Error in err's chain (0, "" without one).
func StatusCode(err error) (int, string) {
	if p, ok := errors.AsType[*problem.Error](err); ok {
		return p.Status(), p.Code()
	}
	return 0, ""
}

// WantProblem fails the test unless err holds a *problem.Error with the
// given status and code.
func WantProblem(t testing.TB, err error, status int, code string) {
	t.Helper()
	if s, c := StatusCode(err); s != status || c != code {
		t.Fatalf("error = %v (%d %s), want %d %s", err, s, c, status, code)
	}
}
