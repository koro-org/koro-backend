package catalog

import (
	"reflect"
	"slices"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/platform/httpx/op"
)

// TestOpenAPISponsorFlags_Enum (BE-127): blocked_sponsor_flags lists the
// flags the service accepts, in the request and in the management view.
func TestOpenAPISponsorFlags_Enum(t *testing.T) {
	t.Parallel()
	want := op.Enum(SponsorFlags())
	for _, typ := range []reflect.Type{reflect.TypeFor[OfferingBody](), reflect.TypeFor[OfferingApprovalDTO]()} {
		reg := huma.NewMapRegistry("#/components/schemas/", huma.DefaultSchemaNamer)
		s := huma.SchemaFromType(reg, typ)
		p := s.Properties["blocked_sponsor_flags"]
		if p == nil || p.Items == nil || !slices.Equal(p.Items.Enum, want) {
			t.Errorf("%s.blocked_sponsor_flags = %+v, want items enum %v", typ.Name(), p, want)
		}
	}
}
