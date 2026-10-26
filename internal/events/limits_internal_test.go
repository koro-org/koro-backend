package events

import (
	"reflect"
	"slices"
	"testing"

	"github.com/danielgtaylor/huma/v2"
)

// bodySchema is the request body schema of an operation input.
func bodySchema(t *testing.T, input any) (*huma.Schema, huma.Registry) {
	t.Helper()
	typ := reflect.TypeOf(input)
	body, ok := typ.FieldByName("Body")
	if !ok {
		t.Fatalf("%s has no Body", typ.Name())
	}
	reg := huma.NewMapRegistry("#/components/schemas/", huma.DefaultSchemaNamer)
	return huma.SchemaFromType(reg, body.Type), reg
}

// TestOpenAPIFieldBounds_MatchServiceLimits (BE-127): the reason fields
// carry the very lengths the service checks with, so the client schema
// rejects a too-short reason before the request.
func TestOpenAPIFieldBounds_MatchServiceLimits(t *testing.T) {
	t.Parallel()
	type bounds struct{ min, max int }
	cases := []struct {
		input any
		field string
		want  bounds
	}{
		{cancelEventInput{}, "cancel_reason", bounds{minCancelReason, maxCancelReason}},
		{cancelEventRegistrationInput{}, "cancel_reason", bounds{minOrgCancel, maxRegCancel}},
		{voidCheckinInput{}, "void_reason", bounds{minVoidReason, maxVoidReason}},
		{publishFormInput{}, "lint_override_reason", bounds{minOverrideReason, maxOverrideReason}},
		{createExportInput{}, "reason", bounds{minExportReason, maxExportReason}},
	}
	for _, c := range cases {
		s, _ := bodySchema(t, c.input)
		p := s.Properties[c.field]
		if p == nil || p.MinLength == nil || p.MaxLength == nil {
			t.Fatalf("%T.%s: no bounds (%+v)", c.input, c.field, p)
		}
		if got := (bounds{*p.MinLength, *p.MaxLength}); got != c.want {
			t.Errorf("%T.%s: bounds %+v, want %+v", c.input, c.field, got, c.want)
		}
	}
}

// TestOpenAPIExportFields_ListTheAllowList (BE-127): createEventExport.fields
// documents the base columns but sets no pattern: a forbidden column (the
// e-mail, T-69) is refused by the service after authz, so the authz matrix
// keeps proving that a non-manager gets 403, not a schema 422.
func TestOpenAPIExportFields_ListTheAllowList(t *testing.T) {
	t.Parallel()
	s, _ := bodySchema(t, createExportInput{})
	f := s.Properties["fields"]
	if f == nil || f.Items == nil || f.MinItems == nil || *f.MinItems != 1 || f.MaxItems == nil || *f.MaxItems != maxExportFields || !f.UniqueItems {
		t.Fatalf("fields = %+v", f)
	}
	if got, ok := f.Items.Extensions[ExtExportBaseFields].([]string); !ok || !slices.Equal(got, ExportBaseFields()) {
		t.Fatalf("%s = %v", ExtExportBaseFields, f.Items.Extensions[ExtExportBaseFields])
	}
	if f.Items.Pattern != "" || len(f.Items.Enum) != 0 {
		t.Fatalf("fields items restrict the values in the schema: pattern %q enum %v", f.Items.Pattern, f.Items.Enum)
	}
}

// TestOpenAPIFormField_ConsentPurposeEnum (BE-127): a consent field's
// purpose is one of the per-registration purposes.
func TestOpenAPIFormField_ConsentPurposeEnum(t *testing.T) {
	t.Parallel()
	reg := huma.NewMapRegistry("#/components/schemas/", huma.DefaultSchemaNamer)
	s := huma.SchemaFromType(reg, reflect.TypeFor[FormField]())
	want := []any{}
	for _, p := range FormConsentPurposes() {
		want = append(want, string(p))
	}
	if got := s.Properties["consent_purpose"].Enum; len(want) == 0 || !slices.Equal(got, want) {
		t.Fatalf("consent_purpose enum = %v, want %v", got, want)
	}
}
