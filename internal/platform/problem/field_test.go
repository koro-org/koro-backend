package problem_test

import (
	"errors"
	"net/http"
	"testing"

	"koro/internal/platform/problem"
)

func TestField_ValidationAtOneLocation(t *testing.T) {
	t.Parallel()
	err := problem.Field("body.title", "required")
	if !errors.Is(err, problem.ErrValidation) {
		t.Fatalf("Field = %v, not ErrValidation", err)
	}
	p, ok := errors.AsType[*problem.Error](err)
	if !ok || p.Status() != http.StatusUnprocessableEntity {
		t.Fatalf("Field = %v, want 422", err)
	}
	if f := p.Fields(); len(f) != 1 || f[0] != (problem.FieldError{Location: "body.title", Message: "required"}) {
		t.Fatalf("fields = %+v", f)
	}
	if len(problem.ErrValidation.Fields()) != 0 {
		t.Fatal("Field mutated the ErrValidation sentinel")
	}
}
