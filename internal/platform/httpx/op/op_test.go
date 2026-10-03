package op_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/op"
)

func TestKoro_WritesExtensionsAndSecurity(t *testing.T) {
	t.Parallel()
	o := op.Koro(huma.Operation{
		OperationID: "createEvent", Method: http.MethodPost, Path: "/orgs/{org_id}/events", Tags: []string{"events"},
		Extensions: map[string]any{"x-other": 1},
	}, op.Meta{Action: "events.event.create", Surface: op.SurfaceOrg, Idempotency: op.IdempotencyRequired, Reauth: true})

	want := map[string]any{
		op.ExtAction: "events.event.create", op.ExtSurface: "org", op.ExtRateLimit: "write_default",
		op.ExtIdempotency: "required", op.ExtReauth: true, "x-other": 1,
	}
	for k, v := range want {
		if o.Extensions[k] != v {
			t.Errorf("extension %s = %v, want %v", k, o.Extensions[k], v)
		}
	}
	if _, has := o.Extensions[op.ExtBreakGlass]; has {
		t.Error("x-koro-break-glass must be omitted when false")
	}
	if len(o.Security) != len(httpx.SessionOrBearer) {
		t.Fatalf("Security = %v, want SessionOrBearer", o.Security)
	}

	get := op.Koro(huma.Operation{OperationID: "listClubs", Method: http.MethodGet, Path: "/clubs", Tags: []string{"directory"}},
		op.Meta{Action: "directory.club.list", Surface: op.SurfacePublic})
	if get.Security != nil || get.Extensions[op.ExtRateLimit] != "read_default" {
		t.Fatalf("public GET: Security = %v, rate = %v", get.Security, get.Extensions[op.ExtRateLimit])
	}
	if _, has := get.Extensions[op.ExtIdempotency]; has {
		t.Fatal("GET must not carry x-koro-idempotency")
	}
}

func TestKoro_InconsistentOperation_Panics(t *testing.T) {
	t.Parallel()
	base := func() huma.Operation {
		return huma.Operation{OperationID: "createThing", Method: http.MethodPost, Path: "/things", Tags: []string{"events"}}
	}
	good := op.Meta{Action: "events.thing.create", Surface: op.SurfaceSelf, Idempotency: op.IdempotencyOptional}
	tests := []struct {
		name string
		o    func() huma.Operation
		m    func() op.Meta
		want string
	}{
		{"bad operationId", func() huma.Operation { o := base(); o.OperationID = "CreateThing"; return o }, func() op.Meta { return good }, "lowerCamel"},
		{"two tags", func() huma.Operation { o := base(); o.Tags = []string{"events", "orgs"}; return o }, func() op.Meta { return good }, "exactly one tag"},
		{"bad action", base, func() op.Meta { m := good; m.Action = "events.create"; return m }, "<module>.<entity>.<verb>"},
		{"unknown surface", base, func() op.Meta { m := good; m.Surface = "internal"; return m }, "unknown surface"},
		{"public with security", func() huma.Operation { o := base(); o.Security = httpx.SessionOrBearer; return o },
			func() op.Meta { m := good; m.Surface = op.SurfacePublic; return m }, "must not declare Security"},
		{"post without idempotency", base, func() op.Meta { m := good; m.Idempotency = ""; return m }, "must declare idempotency"},
		{"get with idempotency", func() huma.Operation { o := base(); o.Method = http.MethodGet; return o }, func() op.Meta { return good }, "POST only"},
		{"unknown rate class", base, func() op.Meta { m := good; m.RateLimit = "fast"; return m }, "unknown rate limit"},
		{"global_ip is not an op class", base, func() op.Meta { m := good; m.RateLimit = op.RateGlobalIP; return m }, "unknown rate limit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				r := recover()
				msg, _ := r.(string)
				if !strings.Contains(msg, tt.want) {
					t.Fatalf("panic = %v, want message containing %q", r, tt.want)
				}
			}()
			op.Koro(tt.o(), tt.m())
		})
	}
}

func TestRateLimits_MatchDesignTable(t *testing.T) {
	t.Parallel()
	if n := len(op.RateLimits()); n != 18 {
		t.Fatalf("04 §1.9 defines 18 classes, got %d", n)
	}
}
