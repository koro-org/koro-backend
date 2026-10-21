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
// parameter of that name. The only place a talent value appears are the
// seven talent_auditor operations of 04 §T (BE-052), and only they are
// exempt (talentAuditorOperations): an operation is exempt when its
// operationId, method and path are exactly one listed row AND it is tagged
// "talent", declares a talent.* action (x-koro-action) on the admin
// surface (x-koro-surface). Anything else — another operation of the
// talent tag, a listed operationId on another path or under another tag,
// an admin operation of another module — is checked like every other
// operation. The exemption covers the operation, not its schemas: another
// operation that refers to a talent DTO is caught.
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

// talentAuditorOperations are the talent_auditor operations of 04 §T
// (operationId → "METHOD /api/v1/path"): the getTalentReport and
// getTalentDistribution responses carry the band and the scores, the
// ledger rows their points and point_event_id, the reversal its
// point_event_id path parameter. Adding a row is a design change (04 §T,
// 03 §5.12) and needs a reason here.
var talentAuditorOperations = map[string]string{
	"getTalentReport":            "GET " + APIPrefix + "/admin/talent/subjects/{user_id}/report",
	"getTalentDistribution":      "GET " + APIPrefix + "/admin/talent/distribution",
	"createTalentNeutralization": "POST " + APIPrefix + "/admin/talent/subjects/{user_id}/neutralizations",
	"liftTalentNeutralization":   "POST " + APIPrefix + "/admin/talent/neutralizations/{neutralization_id}/lift",
	"reverseTalentPointEvent":    "POST " + APIPrefix + "/admin/talent/point-events/{point_event_id}/reversal",
	"getTalentConfig":            "GET " + APIPrefix + "/admin/talent/config",
	"createTalentConfigVersion":  "POST " + APIPrefix + "/admin/talent/config/versions",
}

// Extensions of op (internal/platform/httpx/op imports httpx, so their
// names are repeated here; op's tests pin them).
const (
	extAction  = "x-koro-action"
	extSurface = "x-koro-surface"
)

// talentAuditorOperation reports whether op is exactly one of the
// talent_auditor operations (see the guard's description).
func talentAuditorOperation(op *huma.Operation) bool {
	want, listed := talentAuditorOperations[op.OperationID]
	if !listed || want != op.Method+" "+op.Path || len(op.Tags) != 1 || op.Tags[0] != talentTag {
		return false
	}
	action, _ := op.Extensions[extAction].(string)
	surface, _ := op.Extensions[extSurface].(string)
	return strings.HasPrefix(action, talentTag+".") && surface == "admin"
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
// oapi outside the talent_auditor operations, sorted.
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
	if talentAuditorOperation(op) {
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
	if s.Ref != "" {
		name := strings.TrimPrefix(s.Ref, "#/components/schemas/")
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
			w.v = append(w.v, fmt.Sprintf("%s: %s member %q", w.at, where, prop))
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
