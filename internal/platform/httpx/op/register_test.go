package op_test

import (
	"context"
	"net/http"
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
