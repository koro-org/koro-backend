package problem_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
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

// TestFieldCode_AtAndCodedFields (BE-140): At is a field error that is the
// sentinel itself (its title, its code); CodedFields gives every entry
// without a code of its own the problem's code and keeps the ones that
// have one; neither touches the sentinel.
func TestFieldCode_AtAndCodedFields(t *testing.T) {
	t.Parallel()
	if got := errTestCapacity.At("body.lines[1].offering_id"); got != (problem.FieldError{
		Location: "body.lines[1].offering_id", Message: "Event is full", Code: "testmod.capacity_full",
	}) {
		t.Fatalf("At = %+v", got)
	}
	e := problem.ErrValidation.WithFields(
		problem.FieldError{Location: "body.a", Message: "required"},
		errTestCapacity.At("body.b"),
	)
	got := e.CodedFields()
	want := []problem.FieldError{
		{Location: "body.a", Message: "required", Code: "validation_failed"},
		{Location: "body.b", Message: "Event is full", Code: "testmod.capacity_full"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("CodedFields = %+v, want %+v", got, want)
	}
	if e.Fields()[0].Code != "" || len(problem.ErrValidation.Fields()) != 0 {
		t.Fatal("CodedFields mutated the error or its sentinel")
	}
}

// TestFromError_FieldCode_OnlyCatalogued (BE-140): a field error's code is
// rendered (and every rendered code is in the catalog, problems.json); an
// entry without a code has no code member — existing responses are
// unchanged — and an uncatalogued code never reaches the client.
func TestFromError_FieldCode_OnlyCatalogued(t *testing.T) {
	t.Parallel()
	e := errTestCapacity.WithFields(
		errTestCapacity.At("body.a"),
		problem.ErrValidation.At("body.b"),
		problem.FieldError{Location: "body.c", Message: "plain"},
		problem.FieldError{Location: "body.d", Message: "bogus", Code: "testmod.never_declared"},
	)
	p := problem.FromError(e, "urn:test:")
	codes := []string{"testmod.capacity_full", "validation_failed", "", ""}
	catalogued := map[string]bool{}
	for _, c := range problem.Catalog("") {
		catalogued[c.Code] = true
	}
	for i, f := range p.Errors {
		if f.Code != codes[i] {
			t.Errorf("errors[%d].code = %q, want %q", i, f.Code, codes[i])
		}
		if f.Code != "" && !catalogued[f.Code] {
			t.Errorf("errors[%d].code %q is not catalogued", i, f.Code)
		}
	}
	if p.Errors[0].Code != p.Code {
		t.Errorf("errors[0].code = %q, problem code %q", p.Errors[0].Code, p.Code)
	}
	raw, err := json.Marshal(p.Errors[2])
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"location":"body.c","message":"plain"}` {
		t.Errorf("an entry without a code = %s, want no code member", raw)
	}
	if e.Fields()[3].Code != "testmod.never_declared" {
		t.Error("FromError mutated the error's fields")
	}
}
