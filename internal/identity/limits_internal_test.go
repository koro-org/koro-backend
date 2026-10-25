package identity

import (
	"reflect"
	"testing"

	"github.com/danielgtaylor/huma/v2"
)

// TestOpenAPIFieldBounds_MatchServiceLimits: the request schemas carry the
// very limits the service checks with (BE-117), so a struct tag can never
// widen or narrow them again.
func TestOpenAPIFieldBounds_MatchServiceLimits(t *testing.T) {
	t.Parallel()
	type bounds struct{ min, max int } // min 0 = unset
	name := bounds{1, maxNameLen}
	email := bounds{0, maxEmailLen}
	reason := bounds{minReasonLen, maxReasonLen}
	cases := []struct {
		input any
		field string
		want  bounds
	}{
		{registerInput{}, "first_name", name},
		{registerInput{}, "last_name", name},
		{registerInput{}, "email", email},
		{registerInput{}, "password", bounds{minPasswordLen, maxPasswordLen}},
		{updateMeInput{}, "first_name", name},
		{updateMeInput{}, "last_name", name},
		{emailInput{}, "email", email},
		{emailChangeInput{}, "new_email", email},
		{startEduInput{}, "edu_email", email},
		{resetConfirmInput{}, "new_password", bounds{minPasswordLenMFA, maxPasswordLen}},
		{changePasswordInput{}, "new_password", bounds{minPasswordLenMFA, maxPasswordLen}},
		{reasonInput{}, "reason", reason},
		{resetMFAInput{}, "reason", reason},
		{grantRoleInput{}, "reason", reason},
		{revokeRoleInput{}, "reason", reason},
		{decideEduInput{}, "review_note", bounds{0, maxReviewNote}},
	}
	for _, c := range cases {
		typ := reflect.TypeOf(c.input)
		body, ok := typ.FieldByName("Body")
		if !ok {
			t.Fatalf("%s has no Body", typ.Name())
		}
		reg := huma.NewMapRegistry("#/components/schemas/", huma.DefaultSchemaNamer)
		s := huma.SchemaFromType(reg, body.Type)
		p := s.Properties[c.field]
		if p == nil {
			t.Fatalf("%s.%s: no such property", typ.Name(), c.field)
		}
		got := bounds{max: -1}
		if p.MinLength != nil {
			got.min = *p.MinLength
		}
		if p.MaxLength != nil {
			got.max = *p.MaxLength
		}
		if got != c.want {
			t.Errorf("%s.%s: bounds %+v, want %+v", typ.Name(), c.field, got, c.want)
		}
	}
}
