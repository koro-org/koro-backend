package op_test

import (
	"slices"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/platform/httpx/op"
)

type color string

// TestSchemaHelpers_SetBoundsAndEnums (BE-127): LengthBounds leaves a zero
// bound unset; Enum keeps the order; SetEnum reaches the items of an array
// property and refuses a property the schema does not have.
func TestSchemaHelpers_SetBoundsAndEnums(t *testing.T) {
	t.Parallel()
	s := op.LengthBounds(&huma.Schema{Type: huma.TypeString}, 0, 300)
	if s.MinLength != nil || s.MaxLength == nil || *s.MaxLength != 300 {
		t.Fatalf("LengthBounds(0, 300) = min %v max %v", s.MinLength, s.MaxLength)
	}
	if s = op.LengthBounds(&huma.Schema{}, 5, 0); s.MinLength == nil || *s.MinLength != 5 || s.MaxLength != nil {
		t.Fatalf("LengthBounds(5, 0) = min %v max %v", s.MinLength, s.MaxLength)
	}
	if got := op.Enum([]color{"red", "blue"}); !slices.Equal(got, []any{"red", "blue"}) {
		t.Fatalf("Enum = %v", got)
	}
	obj := &huma.Schema{Type: huma.TypeObject, Properties: map[string]*huma.Schema{
		"one":  {Type: huma.TypeString},
		"many": {Type: huma.TypeArray, Items: &huma.Schema{Type: huma.TypeString}},
	}}
	op.SetEnum(obj, "one", []color{"red"})
	op.SetEnum(obj, "many", []color{"blue"})
	if !slices.Equal(obj.Properties["one"].Enum, []any{"red"}) || !slices.Equal(obj.Properties["many"].Items.Enum, []any{"blue"}) {
		t.Fatalf("SetEnum: one %v, many %v", obj.Properties["one"].Enum, obj.Properties["many"].Items.Enum)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("SetEnum on a missing property did not panic")
		}
	}()
	op.SetEnum(obj, "none", []color{"red"})
}
