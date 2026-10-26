package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz/authztest"
	"koro/internal/platform/clock"
	"koro/internal/platform/httpx"
	"koro/internal/platform/obs"
)

// specApp is the full API as `koro openapi` registers it (nil services).
func specApp(t *testing.T) *huma.OpenAPI {
	t.Helper()
	app, err := httpx.New(httpx.Deps{Log: obs.Discard(), Version: apiVersion})
	if err != nil {
		t.Fatal(err)
	}
	c := newContainer(infra{log: obs.Discard(), clock: clock.Real(), version: version})
	c.registerRoutes(app.V1)
	return app.API.OpenAPI()
}

// TestOpenAPI_NoPersonalDataInQueryStrings (BE-128, KVKK): no operation of
// the full API documents a query parameter that names a person (an e-mail
// address, a name, free text, a user ID…) outside the reasoned allowlist
// of httpx — registration would already panic — and every allowlist row
// still names a registered operation and parameter.
func TestOpenAPI_NoPersonalDataInQueryStrings(t *testing.T) {
	t.Parallel()
	oapi := specApp(t)
	if v := httpx.PersonalQueryViolations(oapi); len(v) > 0 {
		t.Fatalf("personal data in query strings:\n%s", strings.Join(v, "\n"))
	}
	if stale := httpx.PersonalQueryAllowedUnregistered(oapi); len(stale) > 0 {
		t.Fatalf("allowlist rows without their operation or parameter: %v", stale)
	}
}

// movedCriteria are the lists whose personal criteria moved from the query
// string to the body of a POST …/search twin (BE-128): the deprecated GET,
// the criterion, the twin and a caller the action allows.
var movedCriteria = []struct {
	get, path, param, post, persona string
}{
	{"adminSearchUsers", "/api/v1/admin/users", "q", "adminQueryUsers", "platform_admin"},
	{"adminListEmailSuppressions", "/api/v1/admin/email-suppressions", "email", "adminQueryEmailSuppressions", "platform_admin"},
	{"adminListAuditEvents", "/api/v1/admin/audit-events", "actor_user_id", "adminQueryAuditEvents", "dpo"},
	{"listOrgAuditEvents", "/api/v1/orgs/{org_id}/audit-events", "actor_user_id", "queryOrgAuditEvents", "club_admin"},
	{"listCompanyGrantedStudents", "/api/v1/orgs/{org_id}/students", "q", "queryCompanyGrantedStudents", "company_admin"},
	{"adminListIncomingTransfers", "/api/v1/admin/incoming-transfers", "q", "adminQueryIncomingTransfers", "finance_admin"},
	{"adminListLegalParties", "/api/v1/admin/legal-parties", "q", "adminQueryLegalParties", "finance_admin"},
}

// TestOpenAPI_SearchCriteriaInBody (BE-128): each moved list is
// deprecated and no longer documents the criterion; its POST …/search
// twin reads under the same action and rate class family, takes the
// criterion in its body, is exempt from the terms gate as a read and
// declares no 202; and the deprecated GET answers a client that still
// sends the criterion in the URL with 422 at query.<param>, naming the
// twin, instead of an unfiltered list.
func TestOpenAPI_SearchCriteriaInBody(t *testing.T) {
	t.Parallel()
	oapi := specApp(t)
	c := newTestContainer(t)
	c.actors = authztest.NewResolver(append(authztest.Personas(), authztest.Helpers()...)...)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	app, err := httpx.New(httpx.Deps{Log: obs.Discard(), Version: apiVersion, Actors: c.actors, RequestTimeout: matrixRequestTimeout})
	if err != nil {
		t.Fatal(err)
	}
	c.apiMiddleware(app.API, false)
	c.registerRoutes(app.V1)

	for _, m := range movedCriteria {
		get := oapi.Paths[m.path]
		if get == nil || get.Get == nil || get.Get.OperationID != m.get {
			t.Errorf("%s: no GET %s", m.get, m.path)
			continue
		}
		g := get.Get
		if !g.Deprecated {
			t.Errorf("%s is not deprecated", m.get)
		}
		for _, p := range g.Parameters {
			if p.In == "query" && p.Name == m.param {
				t.Errorf("%s still documents query parameter %s", m.get, m.param)
			}
		}
		item := oapi.Paths[m.path+"/search"]
		if item == nil || item.Post == nil || item.Post.OperationID != m.post {
			t.Errorf("%s: no POST %s/search", m.post, m.path)
			continue
		}
		p := item.Post
		if p.Deprecated || p.Extensions["x-koro-action"] != g.Extensions["x-koro-action"] ||
			p.Extensions[httpx.ExtTermsGate] != httpx.TermsGateExempt || p.Responses["202"] != nil {
			t.Errorf("%s: deprecated %v, action %v (GET %v), terms gate %v", m.post, p.Deprecated,
				p.Extensions["x-koro-action"], g.Extensions["x-koro-action"], p.Extensions[httpx.ExtTermsGate])
		}
		body := oapi.Components.Schemas.SchemaFromRef(p.RequestBody.Content["application/json"].Schema.Ref)
		if body == nil || body.Properties[m.param] == nil || body.Properties["cursor"] == nil || body.Properties["limit"] == nil {
			t.Errorf("%s: the body lacks %s, cursor or limit", m.post, m.param)
		}

		// The audit lists' required window rides along (others ignore it).
		url := strings.NewReplacer("{org_id}", orgOf(m.persona)).Replace(m.path) + "?" + m.param +
			"=ada%40example.test&occurred_from=2026-09-01&occurred_to=2026-10-03"
		persona, _ := authztest.Get(m.persona)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
		req.Header.Set("Authorization", "Bearer "+persona.Token)
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		var prob struct {
			Errors []struct{ Location, Message string } `json:"errors"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &prob)
		if rec.Code != http.StatusUnprocessableEntity || len(prob.Errors) != 1 || prob.Errors[0].Location != "query."+m.param ||
			!strings.Contains(prob.Errors[0].Message, m.post) {
			t.Errorf("GET %s as %s = %d %s, want 422 at query.%s naming %s", url, m.persona, rec.Code, rec.Body, m.param, m.post)
		}
	}
}

// orgOf is the org a persona of movedCriteria acts in.
func orgOf(persona string) string {
	if strings.HasPrefix(persona, "company") {
		return authztest.ID("company_a").String()
	}
	return authztest.ID("club_a").String()
}

// TestOpenAPI_FourEyesAcceptedResponses (BE-128, 06 H-47): the admin
// account operations that open a four-eyes request instead of acting
// declare their 202 with its body next to the done status and the error
// response.
func TestOpenAPI_FourEyesAcceptedResponses(t *testing.T) {
	t.Parallel()
	oapi := specApp(t)
	for path, want := range map[string]struct{ done, body202 string }{
		"/api/v1/admin/users/{user_id}/suspend":         {"200", "AdminUserDTO"},
		"/api/v1/admin/users/{user_id}/sessions/revoke": {"204", "AccountActionDTO"},
		"/api/v1/admin/users/{user_id}/mfa/reset":       {"204", "AccountActionDTO"},
	} {
		o := oapi.Paths[path].Post
		r := o.Responses["202"]
		if r == nil || r.Content["application/json"] == nil ||
			r.Content["application/json"].Schema.Ref != "#/components/schemas/"+want.body202 {
			t.Errorf("%s: 202 = %+v, want %s", o.OperationID, r, want.body202)
		}
		if o.Responses[want.done] == nil || o.Responses["default"] == nil || o.Responses["default"].Content["application/problem+json"] == nil {
			t.Errorf("%s: responses %v, want %s and the problem default", o.OperationID, keys(o.Responses), want.done)
		}
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
