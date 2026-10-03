package entitlements_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/entitlements"
	"koro/internal/platform/httpx"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
)

// newHTTP serves the entitlements operations of e with the persona
// resolver and the Idempotency-Key middleware (as cmd/koro does).
func newHTTP(t *testing.T, e *env) *httpx.App {
	t.Helper()
	app, err := httpx.New(httpx.Deps{Log: obs.Discard(), Actors: authztest.NewResolver(append(authztest.Personas(), authztest.Helpers()...)...)})
	if err != nil {
		t.Fatal(err)
	}
	app.API.UseMiddleware(idempotency.Middleware(app.API))
	entitlements.Register(app.V1, e.svc)
	return app
}

func call(t *testing.T, app *httpx.App, persona, method, path string, body any, hdr map[string]string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequestWithContext(t.Context(), method, path, &buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if p, _ := authztest.Get(persona); p.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.Token)
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func listOf(body map[string]any, key string) []map[string]any {
	raw, _ := body[key].([]any)
	out := make([]map[string]any, len(raw))
	for i, x := range raw {
		out[i], _ = x.(map[string]any)
	}
	return out
}

// TestHTTP_ListUsages_ActorOnlyForPadFin: actor_user_id is absent from the
// JSON for the company and present for platform_admin/finance_admin.
func TestHTTP_ListUsages_ActorOnlyForPadFin(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	org := authztest.ID("company_a")
	e.quota(t, org, entitlements.FeatureContactRequest, 5)
	cm := actor(t, "company_member")
	if _, err := e.svc.Consume(authz.WithActor(t.Context(), cm), org, entitlements.FeatureContactRequest, 1, contactRef()); err != nil {
		t.Fatal(err)
	}
	app := newHTTP(t, e)
	for persona, want := range map[string]bool{"company_member": false, "company_admin": false, "platform_admin": true, "finance_admin": true} {
		rec, body := call(t, app, persona, http.MethodGet, "/api/v1/orgs/"+org.String()+"/entitlement-usages", nil, nil)
		its := listOf(body, "items")
		if rec.Code != http.StatusOK || len(its) != 1 {
			t.Fatalf("%s: %d %v", persona, rec.Code, body)
		}
		got, has := its[0]["actor_user_id"]
		if has != want || (has && got != cm.UserID.String()) {
			t.Fatalf("%s: actor_user_id = %v (present %v), want present %v", persona, got, has, want)
		}
		if its[0]["feature"] != "contact_request" || its[0]["ref_type"] != "contact_request" || its[0]["quantity"] != 1.0 {
			t.Fatalf("%s: item %v", persona, its[0])
		}
	}
}

func TestHTTP_ListPagesAndAvailable(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	org := authztest.ID("company_a")
	e.quota(t, org, entitlements.FeatureTargetedPost, 2)
	e.quota(t, org, entitlements.FeatureTargetedPost, 3)
	now := e.clk.Now()
	e.grant(t, org, entitlements.FeatureDiscoveryAccess, nil, now.Add(-time.Hour), now.Add(48*time.Hour))
	app := newHTTP(t, e)
	base := "/api/v1/orgs/" + org.String() + "/entitlements"
	rec, body := call(t, app, "company_member", http.MethodGet, base+"?limit=2", nil, nil)
	cursor, _ := body["next_cursor"].(string)
	if rec.Code != http.StatusOK || len(listOf(body, "items")) != 2 || cursor == "" {
		t.Fatalf("first page: %d %v", rec.Code, body)
	}
	av := map[string]map[string]any{}
	for _, a := range listOf(body, "available") {
		av[a["feature"].(string)] = a
	}
	if av["targeted_post"]["remaining"] != 5.0 || av["contact_request"]["remaining"] != 0.0 ||
		av["discovery_access"]["access_until"] == nil || av["discovery_access"]["remaining"] != nil {
		t.Fatalf("available = %v", av)
	}
	rec, body = call(t, app, "company_member", http.MethodGet, base+"?limit=2&cursor="+cursor, nil, nil)
	if rec.Code != http.StatusOK || len(listOf(body, "items")) != 1 || body["next_cursor"] != nil {
		t.Fatalf("second page: %d %v", rec.Code, body)
	}
	// A cursor of another filter is refused (04 §1.6).
	rec, body = call(t, app, "company_member", http.MethodGet, base+"?limit=2&feature=targeted_post&cursor="+cursor, nil, nil)
	if rec.Code != http.StatusUnprocessableEntity || body["code"] != "page.invalid_cursor" {
		t.Fatalf("foreign cursor: %d %v", rec.Code, body)
	}
	// A malformed entitlement ID is 404 for members, like an unknown one.
	rec, _ = call(t, app, "company_member", http.MethodGet, base+"/not-a-uuid", nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("malformed ID: %d", rec.Code)
	}
}

func TestHTTP_AdminGrantReplayAndRevokeWarning(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	org := authztest.ID("company_b")
	app := newHTTP(t, e)
	grant := map[string]any{
		"feature": "targeted_post", "kind": "quota", "quantity_total": 3,
		"valid_from": "2026-10-01T00:00:00Z", "valid_to": "2027-10-01T00:00:00Z",
		"source_type": "admin_grant", "reason": "Pilot package sold offline",
	}
	path := "/api/v1/admin/orgs/" + org.String() + "/entitlements"
	rec, _ := call(t, app, "finance_admin", http.MethodPost, path, grant, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("grant without Idempotency-Key: %d, want 422", rec.Code)
	}
	key := map[string]string{"Idempotency-Key": "grant-http-0001"}
	rec, first := call(t, app, "finance_admin", http.MethodPost, path, grant, key)
	if rec.Code != http.StatusCreated || first["status"] != "active" || first["quantity_total"] != 3.0 {
		t.Fatalf("grant: %d %v", rec.Code, first)
	}
	rec, again := call(t, app, "finance_admin", http.MethodPost, path, grant, key)
	if rec.Code != http.StatusCreated || again["entitlement_id"] != first["entitlement_id"] || rec.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %d %v (header %q)", rec.Code, again, rec.Header().Get("Idempotent-Replayed"))
	}
	bad := map[string]any{}
	for k, v := range grant {
		bad[k] = v
	}
	bad["kind"] = "access"
	rec, body := call(t, app, "finance_admin", http.MethodPost, path, bad, map[string]string{"Idempotency-Key": "grant-http-0002"})
	if rec.Code != http.StatusUnprocessableEntity || body["code"] != "entitlements.invalid_grant" {
		t.Fatalf("kind mismatch: %d %v", rec.Code, body)
	}

	// Revoking a right bought with an order warns that no refund started.
	line := authztest.ID("line_1")
	now := e.clk.Now()
	id, err := e.svc.Grant(t.Context(), entitlements.GrantInput{
		OrgID: org, Feature: entitlements.FeatureContactRequest, Quantity: ptr(5), ValidFrom: now, ValidTo: now.Add(time.Hour),
		Status: entitlements.StatusActive, SourceType: entitlements.SourceOrderLine, SourceID: &line,
	})
	if err != nil {
		t.Fatal(err)
	}
	rec, body = call(t, app, "platform_admin", http.MethodPost, "/api/v1/admin/entitlements/"+id.String()+"/revoke",
		map[string]any{"reason": "Refund requested"}, nil)
	if rec.Code != http.StatusOK || body["status"] != "revoked" || body["warning"] != entitlements.WarnOrderRefund {
		t.Fatalf("revoke: %d %v", rec.Code, body)
	}
	rec, body = call(t, app, "platform_admin", http.MethodPost, "/api/v1/admin/entitlements/"+first["entitlement_id"].(string)+"/revoke",
		map[string]any{"reason": "Wrong org"}, nil)
	if rec.Code != http.StatusOK || body["warning"] != nil {
		t.Fatalf("revoke of an admin grant: %d %v", rec.Code, body)
	}
}
