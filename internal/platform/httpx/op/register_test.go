package op_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"

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
