package op_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"

	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/op"
)

func TestRegister_SetsTagAndExtensions(t *testing.T) {
	t.Parallel()
	_, api := humatest.New(t)
	op.Register(api, "events", huma.Operation{
		OperationID: "listThings", Method: http.MethodGet, Path: "/things", Tags: []string{"wrong"},
	}, op.Meta{Action: "events.event.list", Surface: op.SurfacePublic},
		func(context.Context, *struct{}) (*struct{}, error) { return nil, nil })
	got := api.OpenAPI().Paths["/things"].Get
	if got == nil {
		t.Fatal("operation not registered")
	}
	if len(got.Tags) != 1 || got.Tags[0] != "events" {
		t.Fatalf("Tags = %v, want [events]", got.Tags)
	}
	if got.Extensions[op.ExtAction] != "events.event.list" || got.Extensions[op.ExtSurface] != "public" {
		t.Fatalf("extensions = %v", got.Extensions)
	}
}

type pendingDTO struct {
	RequestID string `json:"request_id"`
}

type doneDTO struct {
	Done bool `json:"done"`
}

// TestRegister_DocumentsExtraResponses (BE-128): a 202 the output answers
// at run time is in the spec with its body schema, next to the default
// status and huma's error response; a body-less extra status has no
// content.
func TestRegister_DocumentsExtraResponses(t *testing.T) {
	t.Parallel()
	_, api := humatest.New(t)
	op.Register(api, "identity", huma.Operation{
		OperationID: "suspendThing", Method: http.MethodPost, Path: "/things/{id}/suspend",
	}, op.Meta{Action: "identity.user.suspend", Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional,
		Responses: []op.Response{{Status: http.StatusAccepted, Description: "Four-eyes request opened", Body: pendingDTO{}},
			{Status: http.StatusNoContent, Description: "Nothing to do"}}},
		func(context.Context, *struct {
			ID string `path:"id"`
		}) (*struct{ Body doneDTO }, error) {
			return nil, nil
		})
	got := api.OpenAPI().Paths["/things/{id}/suspend"].Post
	if got == nil {
		t.Fatal("operation not registered")
	}
	ref := func(status string) string {
		r := got.Responses[status]
		if r == nil {
			return "<missing>"
		}
		for _, mt := range r.Content {
			return mt.Schema.Ref
		}
		return ""
	}
	for status, want := range map[string]string{"200": "#/components/schemas/DoneDTO", "202": "#/components/schemas/PendingDTO", "204": ""} {
		if got := ref(status); got != want {
			t.Errorf("response %s schema = %q, want %q", status, got, want)
		}
	}
	if d := got.Responses["default"]; d == nil || d.Description != "Error" || len(d.Content) != 1 {
		t.Fatalf("default error response = %+v", d)
	}
	if got.Responses["202"].Description != "Four-eyes request opened" {
		t.Fatalf("202 = %+v", got.Responses["202"])
	}
}

// TestRegister_ExtraResponsesValidated: a non-2xx, repeated or undescribed
// extra status, or Responses through Koro alone, stops the registration.
func TestRegister_ExtraResponsesValidated(t *testing.T) {
	t.Parallel()
	o := huma.Operation{OperationID: "suspendThing", Method: http.MethodPost, Path: "/things", Tags: []string{"identity"}}
	m := op.Meta{Action: "identity.user.suspend", Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional}
	for name, c := range map[string]struct {
		rs   []op.Response
		koro bool
		want string
	}{
		"4xx":            {rs: []op.Response{{Status: 409, Description: "x"}}, want: "described 2xx"},
		"default status": {rs: []op.Response{{Status: 200, Description: "x"}}, want: "described 2xx"},
		"twice":          {rs: []op.Response{{Status: 202, Description: "x"}, {Status: 202, Description: "y"}}, want: "described 2xx"},
		"no description": {rs: []op.Response{{Status: 202}}, want: "described 2xx"},
		"through Koro":   {rs: []op.Response{{Status: 202, Description: "x"}}, koro: true, want: "needs op.Register"},
	} {
		func() {
			defer func() {
				if msg, _ := recover().(string); !strings.Contains(msg, c.want) {
					t.Errorf("%s: panic %q, want %q", name, msg, c.want)
				}
			}()
			mm := m
			mm.Responses = c.rs
			if c.koro {
				op.Koro(o, mm)
				return
			}
			_, api := humatest.New(t)
			op.Register(api, "identity", o, mm, func(context.Context, *struct{}) (*struct{}, error) { return nil, nil })
		}()
	}
}

// TestRegister_AccessReasonRequired (BE-169): the flag documents the
// X-Koro-Access-Reason header as required, while the run-time check stays
// the service's (a call without it reaches the handler); an operation
// without the header panics.
func TestRegister_AccessReasonRequired(t *testing.T) {
	t.Parallel()
	_, api := humatest.New(t)
	reached := false
	op.Register(api, "talent", huma.Operation{
		OperationID: "getThing", Method: http.MethodGet, Path: "/admin/things",
	}, op.Meta{Action: "talent.thing.read", Surface: op.SurfaceAdmin, AccessReasonRequired: true},
		func(context.Context, *struct{ httpx.AccessReasonHeader }) (*struct{}, error) {
			reached = true
			return nil, nil
		})
	var p *huma.Param
	for _, q := range api.OpenAPI().Paths["/admin/things"].Get.Parameters {
		if q.Name == httpx.HeaderAccessReason {
			p = q
		}
	}
	if p == nil || p.In != "header" || !p.Required {
		t.Fatalf("access reason parameter = %+v, want a required header", p)
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin/things", nil)
	rec := httptest.NewRecorder()
	api.Adapter().ServeHTTP(rec, req)
	if !reached {
		t.Fatalf("a call without the header did not reach the handler: %d %s", rec.Code, rec.Body)
	}

	defer func() {
		if r := recover(); r == nil || !strings.Contains(r.(string), "AccessReasonRequired") {
			t.Fatalf("recover() = %v, want the AccessReasonRequired panic", r)
		}
	}()
	op.Register(api, "talent", huma.Operation{
		OperationID: "getOther", Method: http.MethodGet, Path: "/admin/others",
	}, op.Meta{Action: "talent.thing.read", Surface: op.SurfaceAdmin, AccessReasonRequired: true},
		func(context.Context, *struct{}) (*struct{}, error) { return nil, nil })
}

// TestRegister_QuerySyntax (BE-170): Meta.QuerySyntax documents the format
// or pattern of a query parameter while the run-time check stays the
// operation's (a malformed value reaches the handler); a missing parameter
// or one huma already checks panics.
func TestRegister_QuerySyntax(t *testing.T) {
	t.Parallel()
	_, api := humatest.New(t)
	var got string
	op.Register(api, "ledger", huma.Operation{
		OperationID: "listBounds", Method: http.MethodGet, Path: "/admin/bounds",
	}, op.Meta{Action: "ledger.thing.list", Surface: op.SurfacePublic,
		QuerySyntax: map[string]op.ParamSyntax{"from": op.DayOrTime, "day": op.Day}},
		func(_ context.Context, in *struct {
			From string `query:"from" maxLength:"40"`
			Day  string `query:"day"`
		}) (*struct{}, error) {
			got = in.From
			return nil, nil
		})
	for _, p := range api.OpenAPI().Paths["/admin/bounds"].Get.Parameters {
		switch p.Name {
		case "from":
			if p.Schema.Pattern != op.DayOrTimePattern || p.Schema.MaxLength == nil || *p.Schema.MaxLength != 40 {
				t.Errorf("from schema = %+v, want the DayOrTime pattern and maxLength 40", p.Schema)
			}
		case "day":
			if p.Schema.Format != "date" {
				t.Errorf("day format = %q, want date", p.Schema.Format)
			}
		}
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin/bounds?from=yesterday", nil)
	rec := httptest.NewRecorder()
	api.Adapter().ServeHTTP(rec, req)
	if got != "yesterday" {
		t.Fatalf("a malformed bound did not reach the handler: %d %s", rec.Code, rec.Body)
	}

	for name, in := range map[string]func(){
		"missing": func() {
			op.Register(api, "ledger", huma.Operation{OperationID: "listMissing", Method: http.MethodGet, Path: "/missing"},
				op.Meta{Action: "ledger.thing.list", Surface: op.SurfacePublic, QuerySyntax: map[string]op.ParamSyntax{"from": op.Day}},
				func(context.Context, *struct{}) (*struct{}, error) { return nil, nil })
		},
		"tagged": func() {
			op.Register(api, "ledger", huma.Operation{OperationID: "listTagged", Method: http.MethodGet, Path: "/tagged"},
				op.Meta{Action: "ledger.thing.list", Surface: op.SurfacePublic, QuerySyntax: map[string]op.ParamSyntax{"from": op.Day}},
				func(context.Context, *struct {
					From string `query:"from" format:"date"`
				}) (*struct{}, error) {
					return nil, nil
				})
		},
		"through Koro": func() {
			_ = op.Koro(huma.Operation{OperationID: "listKoro", Method: http.MethodGet, Path: "/koro", Tags: []string{"ledger"}},
				op.Meta{Action: "ledger.thing.list", Surface: op.SurfacePublic, QuerySyntax: map[string]op.ParamSyntax{"from": op.Day}})
		},
	} {
		func() {
			defer func() {
				if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), "QuerySyntax") {
					t.Errorf("%s: recover() = %v, want a QuerySyntax panic", name, r)
				}
			}()
			in()
		}()
	}
}

// TestRegister_PathSyntax (BE-175): Meta.PathSyntax documents the syntax
// of an ID path parameter that is not a UUID instead of the httpx rule's
// format uuid, while a malformed value still reaches the handler; another
// ID path parameter keeps the rule. A missing parameter, one huma already
// checks, or Koro alone panics.
func TestRegister_PathSyntax(t *testing.T) {
	t.Parallel()
	// humatest, not httpx.New: httpx.New installs huma.NewError (a package
	// global) once, which races with the parallel tests that read it in
	// huma.Register. The document rule is hooked in the way httpx.New does.
	_, api := humatest.New(t)
	api.OpenAPI().OnAddOperation = append(api.OpenAPI().OnAddOperation,
		func(_ *huma.OpenAPI, o *huma.Operation) { httpx.DocumentParams(o) })
	var got string
	op.Register(api, "discovery", huma.Operation{
		OperationID: "getAnon", Method: http.MethodGet, Path: "/orgs/{org_id}/anons/{anon_id}",
	}, op.Meta{Action: "discovery.thing.read", Surface: op.SurfacePublic,
		PathSyntax: map[string]op.ParamSyntax{"anon_id": {Pattern: "^[a-z2-7]{16}$"}}},
		func(_ context.Context, in *struct {
			OrgID  string `path:"org_id"`
			AnonID string `path:"anon_id"`
		}) (*struct{}, error) {
			got = in.AnonID
			return nil, nil
		})
	for _, p := range api.OpenAPI().Paths["/orgs/{org_id}/anons/{anon_id}"].Get.Parameters {
		switch p.Name {
		case "anon_id":
			if p.Schema.Format != "" || p.Schema.Pattern != "^[a-z2-7]{16}$" {
				t.Errorf("anon_id schema = %+v, want only the declared pattern", p.Schema)
			}
		case "org_id":
			if p.Schema.Format != "uuid" {
				t.Errorf("org_id format = %q, want uuid (the httpx rule)", p.Schema.Format)
			}
		}
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/orgs/019a0000-0000-7000-8000-000000000001/anons/NOT-A-PSEUDONYM", nil)
	rec := httptest.NewRecorder()
	api.Adapter().ServeHTTP(rec, req)
	if got != "NOT-A-PSEUDONYM" {
		t.Fatalf("a malformed path value did not reach the handler: %d %s", rec.Code, rec.Body)
	}

	for name, in := range map[string]func(){
		"missing": func() {
			op.Register(api, "discovery", huma.Operation{OperationID: "getMissing", Method: http.MethodGet, Path: "/missing"},
				op.Meta{Action: "discovery.thing.read", Surface: op.SurfacePublic, PathSyntax: map[string]op.ParamSyntax{"anon_id": op.Day}},
				func(context.Context, *struct{}) (*struct{}, error) { return nil, nil })
		},
		"tagged": func() {
			op.Register(api, "discovery", huma.Operation{OperationID: "getTagged", Method: http.MethodGet, Path: "/tagged/{anon_id}"},
				op.Meta{Action: "discovery.thing.read", Surface: op.SurfacePublic, PathSyntax: map[string]op.ParamSyntax{"anon_id": op.Day}},
				func(context.Context, *struct {
					AnonID string `path:"anon_id" format:"date"`
				}) (*struct{}, error) {
					return nil, nil
				})
		},
		"through Koro": func() {
			_ = op.Koro(huma.Operation{OperationID: "getKoro", Method: http.MethodGet, Path: "/koro/{anon_id}", Tags: []string{"discovery"}},
				op.Meta{Action: "discovery.thing.read", Surface: op.SurfacePublic, PathSyntax: map[string]op.ParamSyntax{"anon_id": op.Day}})
		},
	} {
		func() {
			defer func() {
				if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), "PathSyntax") {
					t.Errorf("%s: recover() = %v, want a PathSyntax panic", name, r)
				}
			}()
			in()
		}()
	}
}

// TestDayOrTimePattern (BE-170): the documented pattern takes what the
// bound parsers take (RFC 3339 with or without fractions, a YYYY-MM-DD
// day) and refuses what they refuse.
func TestDayOrTimePattern(t *testing.T) {
	t.Parallel()
	re := regexp.MustCompile(op.DayOrTimePattern)
	for _, s := range []string{"2026-11-04", "2026-11-04T10:00:00Z", "2026-11-04T10:00:00.123456789+03:00", "2026-01-31T23:59:59-05:30"} {
		if !re.MatchString(s) {
			t.Errorf("%q does not match", s)
		}
		if _, err := time.Parse(time.RFC3339Nano, s); err != nil {
			if _, err := time.Parse(time.DateOnly, s); err != nil {
				t.Errorf("%q matches but no parser takes it", s)
			}
		}
	}
	for _, s := range []string{"", "2026-13-01", "2026-11-4", "04.11.2026", "2026-11-04T10:00", "2026-11-04T10:00:00", "2026-11-04 10:00:00Z", "yesterday", "2026-11-04T25:00:00Z"} {
		if re.MatchString(s) {
			t.Errorf("%q matches", s)
		}
	}
}
