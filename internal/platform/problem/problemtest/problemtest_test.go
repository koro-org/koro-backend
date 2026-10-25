package problemtest_test

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"koro/internal/platform/problem"
	"koro/internal/platform/problem/problemtest"
)

// fakeTB records Fatalf instead of stopping the test.
type fakeTB struct {
	testing.TB
	failed bool
}

func (f *fakeTB) Helper()               {}
func (f *fakeTB) Fatalf(string, ...any) { f.failed = true }

func TestWantErr(t *testing.T) {
	t.Parallel()
	wrapped := fmt.Errorf("op: %w", problem.ErrNotFound)
	ok := &fakeTB{TB: t}
	problemtest.WantErr(ok, wrapped, problem.ErrNotFound)
	if ok.failed {
		t.Fatal("WantErr failed on a wrapped match")
	}
	bad := &fakeTB{TB: t}
	problemtest.WantErr(bad, errors.New("other"), problem.ErrNotFound)
	if !bad.failed {
		t.Fatal("WantErr passed on a mismatch")
	}
}

func TestWantProblem(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("op: %w", problem.Field("body.title", "required"))
	if s, c := problemtest.StatusCode(err); s != http.StatusUnprocessableEntity || c != "validation_failed" {
		t.Fatalf("StatusCode = %d %s", s, c)
	}
	ok := &fakeTB{TB: t}
	problemtest.WantProblem(ok, err, http.StatusUnprocessableEntity, "validation_failed")
	if ok.failed {
		t.Fatal("WantProblem failed on a match")
	}
	for _, e := range []error{errors.New("plain"), problem.ErrNotFound} {
		bad := &fakeTB{TB: t}
		problemtest.WantProblem(bad, e, http.StatusUnprocessableEntity, "validation_failed")
		if !bad.failed {
			t.Fatalf("WantProblem passed on %v", e)
		}
	}
	if s, c := problemtest.StatusCode(nil); s != 0 || c != "" {
		t.Fatalf("StatusCode(nil) = %d %q", s, c)
	}
}
