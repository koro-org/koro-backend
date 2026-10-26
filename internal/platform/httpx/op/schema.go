package op

import "github.com/danielgtaylor/huma/v2"

// Schema helpers of the request and response field types whose OpenAPI
// bounds and enums come from the constants the service checks with
// (BE-117): a struct tag cannot name a constant, so the type sets them in
// TransformSchema (huma.SchemaTransformer) and the generated contract (and
// the client Zod schemas built from it) can never drift from the Go rule.

// LengthBounds sets the character bounds of a string schema; minLen 0
// leaves the minimum unset, maxLen 0 the maximum. huma counts characters
// as the services do; a service still checks after normalising the text.
func LengthBounds(s *huma.Schema, minLen, maxLen int) *huma.Schema {
	if minLen > 0 {
		s.MinLength = &minLen
	}
	if maxLen > 0 {
		s.MaxLength = &maxLen
	}
	return s
}

// Enum returns vals as the values of a schema enum, in order.
func Enum[T ~string](vals []T) []any {
	out := make([]any, len(vals))
	for i, v := range vals {
		out[i] = string(v)
	}
	return out
}

// SetEnum replaces the enum of the property prop of an object schema s
// (a DTO's TransformSchema: the field keeps its plain string type). A
// missing property is a programming error caught when the API is built.
func SetEnum[T ~string](s *huma.Schema, prop string, vals []T) *huma.Schema {
	p := s.Properties[prop]
	if p == nil {
		panic("op: schema has no property " + prop)
	}
	if p.Type == huma.TypeArray && p.Items != nil {
		p = p.Items
	}
	p.Enum = Enum(vals)
	return s
}
