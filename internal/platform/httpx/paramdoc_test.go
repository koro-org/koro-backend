package httpx_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/obs"
)

type paramDocInput struct {
	OrgID   string   `query:"org_id" required:"true"`
	SkillID []string `query:"skill_id,explode"`
	RefID   string   `query:"ref_id" format:"uuid"`
	Code    string   `query:"code_id" pattern:"^[a-z]+$"`
	Status  string   `query:"status" required:"true"`
	Kind    string   `query:"kind" required:"true" enum:"a,b"`
	Note    string   `query:"note"`
}

// TestParamDocs_DocumentsIDsAndRequiredStrings (BE-170): an ID query
// parameter without a format or pattern is documented as a UUID (a list:
// its items), a required string as minLength 1; a parameter that has its
// own format, pattern or enum keeps it, and a plain optional one is left
// alone. Only the document changes: a malformed ID and an empty status of
// the schema huma checks still reach the handler, which answers them
// itself (before or after authz).
func TestParamDocs_DocumentsIDsAndRequiredStrings(t *testing.T) {
	t.Parallel()
	app, err := httpx.New(httpx.Deps{Log: obs.Discard()})
	if err != nil {
		t.Fatal(err)
	}
	var got paramDocInput
	op.Register(app.V1, "testonly", huma.Operation{OperationID: "listParamDocs", Method: http.MethodGet, Path: "/param-docs"},
		op.Meta{Action: "testonly.thing.list", Surface: op.SurfacePublic},
		func(_ context.Context, in *paramDocInput) (*struct{}, error) { got = *in; return nil, nil })

	params := map[string]*huma.Param{}
	for _, p := range app.API.OpenAPI().Paths["/api/v1/param-docs"].Get.Parameters {
		params[p.Name] = p
	}
	type want struct {
		format, pattern string
		minLen          int // 0: unset
	}
	for name, w := range map[string]want{
		"org_id":   {format: "uuid", minLen: 1},
		"skill_id": {format: "uuid"},
		"ref_id":   {format: "uuid", minLen: 0},
		"code_id":  {pattern: "^[a-z]+$"},
		"status":   {minLen: 1},
		"kind":     {},
		"note":     {},
	} {
		s := params[name].Schema
		if s.Type == huma.TypeArray {
			s = s.Items
		}
		minLen := 0
		if s.MinLength != nil {
			minLen = *s.MinLength
		}
		if s.Format != w.format || s.Pattern != w.pattern || minLen != w.minLen {
			t.Errorf("%s: format %q pattern %q minLength %d, want %+v", name, s.Format, s.Pattern, minLen, w)
		}
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/api/v1/param-docs?org_id=not-a-uuid&skill_id=x&status=open&kind=a", nil)
	rec := httptest.NewRecorder()
	app.Engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent && rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s, want the handler's answer", rec.Code, rec.Body)
	}
	if got.OrgID != "not-a-uuid" || len(got.SkillID) != 1 || got.SkillID[0] != "x" {
		t.Fatalf("handler input = %+v", got)
	}
}

// TestIDParamName (BE-170): "id" and "<x>_id" are ID parameters.
func TestIDParamName(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{"id": true, "org_id": true, "university_org_id": true,
		"identity": false, "idle": false, "status": false, "ids": false} {
		if got := httpx.IDParamName(name); got != want {
			t.Errorf("IDParamName(%q) = %v, want %v", name, got, want)
		}
	}
}
