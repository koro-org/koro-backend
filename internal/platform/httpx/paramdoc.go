package httpx

import (
	"strings"

	"github.com/danielgtaylor/huma/v2"
)

// Parameter document rules (BE-170, BE-175). Client types (Orval/Zod) and
// the contract tests (Schemathesis) read the syntax of a parameter from the
// document, so the document states it even where huma does not check it:
//
//   - An ID query or path parameter ("id" or "<x>_id", a string or a list
//     of strings) without a format or pattern is documented as format
//     uuid. Every module parses it itself: a malformed query ID is 422
//     validation_failed "must be a UUID", a malformed path ID mostly 404
//     (an unknown ID) — some before the authorisation, some after it
//     (03 §7.2: 404/403 first) — so only the document changes and every
//     status and order stays. An ID that is not a UUID declares its own
//     syntax (op.Meta.QuerySyntax/PathSyntax, which name it in
//     MetaOwnSyntax) or a huma format/pattern tag; the rule leaves it.
//   - A required string parameter (query, header, cookie) that may be
//     empty in the schema is documented with minLength 1: huma treats an
//     empty value as a missing one ("required … parameter is missing",
//     422), so "" was never a valid value.
//
// The rules copy the parameter's schema before changing it: huma validates
// a request against the schema it built at registration, which the copy
// leaves as it was. A tag (format:"uuid", minLength:"1") on the field is
// the way to have huma check it as well.

// MetaOwnSyntax is the huma.Operation.Metadata key of the parameters whose
// syntax the operation documents itself (a map[string]bool of parameter
// names, written by op.Register): the rules leave them alone.
const MetaOwnSyntax = "koro.own_param_syntax"

// installParamDocs adds the rules to oapi's OnAddOperation hooks.
func installParamDocs(oapi *huma.OpenAPI) {
	oapi.OnAddOperation = append(oapi.OnAddOperation, func(_ *huma.OpenAPI, op *huma.Operation) {
		DocumentParams(op)
	})
}

// DocumentParams applies the parameter document rules to op.
func DocumentParams(op *huma.Operation) {
	own, _ := op.Metadata[MetaOwnSyntax].(map[string]bool)
	for _, p := range op.Parameters {
		if p == nil || p.Schema == nil || own[p.Name] {
			continue
		}
		if (p.In == "query" || p.In == "path") && IDParamName(p.Name) {
			if s := stringTarget(p.Schema); s != nil && s.Format == "" && s.Pattern == "" {
				p.Schema = withString(p.Schema, func(s *huma.Schema) { s.Format = "uuid" })
			}
		}
		if p.Required && p.In != "path" && p.Schema.Type == huma.TypeString &&
			p.Schema.MinLength == nil && len(p.Schema.Enum) == 0 && p.Schema.Const == nil {
			p.Schema = withString(p.Schema, func(s *huma.Schema) { one := 1; s.MinLength = &one })
		}
	}
}

// IDParamName reports whether a parameter name carries an ID under the
// rule: "id" or a name ending in "_id".
func IDParamName(name string) bool {
	return name == "id" || strings.HasSuffix(name, "_id")
}

// stringTarget returns the string schema of s (s itself, or the items of
// a list), nil for any other type.
func stringTarget(s *huma.Schema) *huma.Schema {
	if s.Type == huma.TypeArray {
		s = s.Items
	}
	if s == nil || s.Type != huma.TypeString || s.Ref != "" {
		return nil
	}
	return s
}

// withString returns a copy of s whose string schema (s, or the items of
// a list) is changed by set; s is not modified.
func withString(s *huma.Schema, set func(*huma.Schema)) *huma.Schema {
	c := *s
	if c.Type == huma.TypeArray && c.Items != nil {
		items := *c.Items
		set(&items)
		c.Items = &items
		return &c
	}
	set(&c)
	return &c
}
