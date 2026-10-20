package httpx

import (
	"fmt"
	"slices"
	"strings"

	"github.com/danielgtaylor/huma/v2"
)

// The talent field guard (01 §4.11, 03 §3.4 Y1, 04 §T and §V.0-2, BE-047):
// nobody sees a talent value, so no request or response schema of the API
// may carry a member named talent*, band, band_*, score* or point*, nor a
// parameter of that name. The talent_auditor operations (BE-052, tag
// "talent") are the only place a talent value appears and are exempt.
//
// New installs it on every App: each operation is checked when it is
// added to the OpenAPI document (huma's OnAddOperation), following $refs
// through the schema registry, and a violation panics at registration,
// the way op.Koro rejects an inconsistent operation: `koro api`, `koro
// openapi` and the tests all register every operation, so a talent field
// never ships. TalentFieldViolations checks a whole document (the spec
// test).

// talentTag is the tag (module) of the talent_auditor operations.
const talentTag = "talent"

// talentFieldExempt are reasoned exceptions, "<schema>.<member>".
var talentFieldExempt = map[string]string{
	// payments adminGetIncomingTransfer suggestions[].score (04 §2.16,
	// BE-041): the confidence that a bank transfer matches an order (1, 0.7,
	// 0.5, 0), not a person. 04 §V.0-2 bans score* in every DTO; the
	// rename is a doc patch proposal of BE-047.
	"SuggestionDTO.score": "bank transfer ↔ order match confidence (payments), not a talent value",
}

// talentFieldName reports a member or parameter name the guard rejects.
func talentFieldName(name string) bool {
	n := strings.ToLower(name)
	return strings.HasPrefix(n, "talent") || n == "band" || strings.HasPrefix(n, "band_") ||
		strings.HasPrefix(n, "score") || strings.HasPrefix(n, "point")
}

// installTalentFieldGuard adds the guard to oapi's OnAddOperation hooks.
func installTalentFieldGuard(oapi *huma.OpenAPI) {
	oapi.OnAddOperation = append(oapi.OnAddOperation, func(o *huma.OpenAPI, op *huma.Operation) {
		if v := operationTalentFields(o, op); len(v) > 0 {
			panic(fmt.Sprintf("httpx: operation %s %s (%s) exposes a talent field (01 §0-7, 03 Y1): %s",
				op.Method, op.Path, op.OperationID, strings.Join(v, "; ")))
		}
	})
}

// TalentFieldViolations returns every talent field of the operations of
// oapi outside the talent operations, sorted.
func TalentFieldViolations(oapi *huma.OpenAPI) []string {
	var v []string
	for _, item := range oapi.Paths {
		for _, op := range []*huma.Operation{item.Get, item.Put, item.Post, item.Delete, item.Options, item.Head, item.Patch, item.Trace} {
			if op != nil {
				v = append(v, operationTalentFields(oapi, op)...)
			}
		}
	}
	slices.Sort(v)
	return slices.Compact(v)
}

// operationTalentFields checks the parameters, the request body and the
// responses of op.
func operationTalentFields(oapi *huma.OpenAPI, op *huma.Operation) []string {
	if slices.Contains(op.Tags, talentTag) {
		return nil
	}
	w := talentWalker{oapi: oapi, seen: map[*huma.Schema]bool{}, at: op.OperationID}
	for _, p := range op.Parameters {
		if p == nil {
			continue
		}
		if talentFieldName(p.Name) {
			w.v = append(w.v, fmt.Sprintf("%s: %s parameter %q", w.at, p.In, p.Name))
		}
		w.walk(p.Schema, "parameter "+p.Name)
	}
	if op.RequestBody != nil {
		for _, mt := range op.RequestBody.Content {
			if mt != nil {
				w.walk(mt.Schema, "request body")
			}
		}
	}
	for status, r := range op.Responses {
		if r == nil {
			continue
		}
		for _, mt := range r.Content {
			if mt != nil {
				w.walk(mt.Schema, "response "+status)
			}
		}
	}
	return w.v
}

// talentWalker walks schemas, following $refs once each.
type talentWalker struct {
	oapi *huma.OpenAPI
	seen map[*huma.Schema]bool
	at   string
	v    []string
}

func (w *talentWalker) walk(s *huma.Schema, where string) {
	if s == nil {
		return
	}
	name := ""
	if s.Ref != "" {
		name = strings.TrimPrefix(s.Ref, "#/components/schemas/")
		if w.oapi.Components == nil || w.oapi.Components.Schemas == nil {
			return
		}
		s = w.oapi.Components.Schemas.SchemaFromRef(s.Ref)
		if s == nil {
			return
		}
		where = name
	}
	if w.seen[s] {
		return
	}
	w.seen[s] = true
	for prop, ps := range s.Properties {
		if talentFieldName(prop) {
			if _, ok := talentFieldExempt[name+"."+prop]; !ok {
				w.v = append(w.v, fmt.Sprintf("%s: %s member %q", w.at, where, prop))
			}
		}
		w.walk(ps, where+"."+prop)
	}
	w.walk(s.Items, where+"[]")
	if ap, ok := s.AdditionalProperties.(*huma.Schema); ok {
		w.walk(ap, where+"{}")
	}
	for _, list := range [][]*huma.Schema{s.AllOf, s.AnyOf, s.OneOf} {
		for _, sub := range list {
			w.walk(sub, where)
		}
	}
	w.walk(s.Not, where)
}
