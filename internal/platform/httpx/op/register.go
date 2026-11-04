package op

import (
	"context"
	"fmt"
	"reflect"
	"strconv"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/platform/httpx"
)

// Register registers o under the OpenAPI tag of its module: it sets
// o.Tags to [tag], adds the Koro extensions (Koro) and calls huma.Register.
// Every module registers its operations with it instead of a private reg
// helper (BE-111). Go has no generic methods, so the tag is an argument
// rather than a bound registrar value.
func Register[I, O any](api huma.API, tag string, o huma.Operation, m Meta, fn func(context.Context, *I) (*O, error)) {
	o.Tags = []string{tag}
	o = koro(o, m)
	if len(m.Responses) > 0 {
		documentResponses(api.OpenAPI().Components.Schemas, &o, m.Responses)
	}
	huma.Register(api, o, fn)
	if m.AccessReasonRequired {
		requireAccessReasonDoc(api.OpenAPI(), o)
	}
	if len(m.QuerySyntax) > 0 {
		documentQuerySyntax(api.OpenAPI(), o, m.QuerySyntax)
	}
}

// registered returns the operation of oapi with o's ID. The document's
// path carries the group prefix (/api/v1), o.Path not: the operation is
// found by its ID.
func registered(oapi *huma.OpenAPI, o huma.Operation) *huma.Operation {
	for _, item := range oapi.Paths {
		for _, reg := range []*huma.Operation{item.Get, item.Put, item.Post, item.Delete, item.Options, item.Head, item.Patch} {
			if reg != nil && reg.OperationID == o.OperationID {
				return reg
			}
		}
	}
	panic(fmt.Sprintf("op.Register(%s %s %q): not in the document", o.Method, o.Path, o.OperationID))
}

// documentQuerySyntax writes the syntaxes of Meta.QuerySyntax into copies
// of the parameter schemas of the registered o: huma validates against the
// schema it built at registration, which stays as it was, so the
// operation's own check (and its order) is unchanged. A parameter that is
// not a string query parameter, or that huma already checks (a format or
// pattern tag), panics.
func documentQuerySyntax(oapi *huma.OpenAPI, o huma.Operation, syntax map[string]ParamSyntax) {
	reg := registered(oapi, o)
	for name, sx := range syntax {
		var p *huma.Param
		for _, q := range reg.Parameters {
			if q.In == "query" && q.Name == name {
				p = q
			}
		}
		if p == nil || p.Schema == nil || p.Schema.Type != huma.TypeString || p.Schema.Format != "" || p.Schema.Pattern != "" ||
			(sx.Format == "" && sx.Pattern == "") {
			panic(fmt.Sprintf("op.Register(%s %s %q): Meta.QuerySyntax[%q] needs a string query parameter without a format or pattern tag",
				o.Method, o.Path, o.OperationID, name))
		}
		c := *p.Schema
		c.Format, c.Pattern = sx.Format, sx.Pattern
		p.Schema = &c
	}
}

// requireAccessReasonDoc marks the X-Koro-Access-Reason parameter of the
// registered o required in the document (Meta.AccessReasonRequired). huma
// enforces a parameter from its struct tag, read at registration, so the
// run-time order (authz, then the service's 422) is unchanged. An
// operation without the header panics: the flag would document nothing.
func requireAccessReasonDoc(oapi *huma.OpenAPI, o huma.Operation) {
	for _, p := range registered(oapi, o).Parameters {
		if p.In == "header" && p.Name == httpx.HeaderAccessReason {
			p.Required = true
			return
		}
	}
	panic(fmt.Sprintf("op.Register(%s %s %q): Meta.AccessReasonRequired without an %s header (embed httpx.AccessReasonHeader)",
		o.Method, o.Path, o.OperationID, httpx.HeaderAccessReason))
}

// documentResponses writes rs into o.Responses before huma.Register adds
// the default status. huma adds its error response ("default") only to an
// operation with at most one response, so it is written here the way huma
// would (defineErrors: the huma.NewError model and its content type).
func documentResponses(reg huma.Registry, o *huma.Operation, rs []Response) {
	if o.Responses == nil {
		o.Responses = map[string]*huma.Response{}
	}
	for _, r := range rs {
		resp := &huma.Response{Description: r.Description}
		if r.Body != nil {
			resp.Content = map[string]*huma.MediaType{"application/json": {Schema: reg.Schema(reflect.TypeOf(r.Body), true, "")}}
		}
		o.Responses[strconv.Itoa(r.Status)] = resp
	}
	model := huma.NewError(0, "")
	ct := "application/json"
	if f, ok := model.(huma.ContentTypeFilter); ok {
		ct = f.ContentType(ct)
	}
	t := reflect.TypeOf(model)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	hint := t.Name()
	if hint == "" {
		hint = "Error"
	}
	o.Responses["default"] = &huma.Response{Description: "Error",
		Content: map[string]*huma.MediaType{ct: {Schema: reg.Schema(t, true, hint)}}}
}
