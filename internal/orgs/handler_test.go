package orgs_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"koro/internal/authz/authztest"
	"koro/internal/orgs"
	"koro/internal/platform/httpx"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/ratelimit"
)

// newHTTP serves the orgs operations of e with the persona resolver, the
// rate limit middleware over an in-memory limiter and the Idempotency-Key
// middleware (as cmd/koro does).
func newHTTP(t *testing.T, e *env) *httpx.App {
	t.Helper()
	app, err := httpx.New(httpx.Deps{Log: obs.Discard(), Actors: authztest.NewResolver(append(authztest.Personas(), authztest.Helpers()...)...)})
	if err != nil {
		t.Fatal(err)
	}
	app.API.UseMiddleware(ratelimit.Middleware(app.API, ratelimit.NewMem(e.clk), obs.Discard()))
	app.API.UseMiddleware(idempotency.Middleware(app.API))
	orgs.Register(app.V1, e.svc)
	return app
}

func call(t *testing.T, app *httpx.App, persona, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, path, bytes.NewBufferString(body))
	if body != "" {
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
	return rec
}

// TestSearchOrgs_RateLimited: searchOrgs is in the search class (04 §1.9:
// 60/min per actor, per IP when anonymous): the 61st search of a minute is
// 429 with Retry-After, another actor is not affected.
func TestSearchOrgs_RateLimited(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if _, err := e.svc.Create(t.Context(), orgs.CreateOrg{Kind: orgs.KindClub, Name: "Fotoğraf Kulübü"}); err != nil {
		t.Fatal(err)
	}
	app := newHTTP(t, e)
	for i := range 60 {
		if rec := call(t, app, "student", http.MethodGet, "/api/v1/orgs?q=foto", "", nil); rec.Code != http.StatusOK {
			t.Fatalf("search %d = %d %s", i+1, rec.Code, rec.Body)
		}
	}
	rec := call(t, app, "student", http.MethodGet, "/api/v1/orgs?q=foto", "", nil)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" ||
		!strings.Contains(rec.Body.String(), "rate_limited") {
		t.Fatalf("61st search = %d %s (Retry-After %q)", rec.Code, rec.Body, rec.Header().Get("Retry-After"))
	}
	if rec := call(t, app, "company_member", http.MethodGet, "/api/v1/orgs?q=foto", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("another actor's search = %d", rec.Code)
	}
	// The profile read is not in the search class.
	if rec := call(t, app, "student", http.MethodGet, "/api/v1/orgs/by-slug/fotograf-kulubu", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("getOrgBySlug after the search limit = %d %s", rec.Code, rec.Body)
	}
}

// TestHTTP_SearchOrgs_ResultAndCursor: the search finds an active org by a
// part of its name (Turkish-insensitive) and pages with an opaque cursor.
func TestHTTP_SearchOrgs_ResultAndCursor(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, n := range []string{"Işık Kulübü", "Işıklı Topluluk", "Başka Kulüp"} {
		if _, err := e.svc.Create(t.Context(), orgs.CreateOrg{Kind: orgs.KindClub, Name: n}); err != nil {
			t.Fatal(err)
		}
	}
	app := newHTTP(t, e)
	rec := call(t, app, "anonymous", http.MethodGet, "/api/v1/orgs?q=isik&sort=name&limit=1", "", nil)
	var page struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
		Next string `json:"next_cursor"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &page) != nil || len(page.Items) != 1 || page.Next == "" {
		t.Fatalf("first page = %d %s", rec.Code, rec.Body)
	}
	first := page.Items[0].Name
	rec = call(t, app, "anonymous", http.MethodGet, "/api/v1/orgs?q=isik&sort=name&limit=1&cursor="+page.Next, "", nil)
	page.Items, page.Next = nil, ""
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &page) != nil || len(page.Items) != 1 ||
		page.Items[0].Name == first || page.Next != "" {
		t.Fatalf("second page = %d %s", rec.Code, rec.Body)
	}
	// The cursor is bound to its filters.
	if rec := call(t, app, "anonymous", http.MethodGet, "/api/v1/orgs?q=kulup&sort=name&limit=1&cursor="+page.Next+"x", "", nil); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("foreign cursor = %d", rec.Code)
	}
}

// TestHTTP_UpdateOrg_ETagRoundTrip: getOrg's ETag header is updateOrg's
// If-Match (04 §1.13); the PATCH answers with the new ETag.
func TestHTTP_UpdateOrg_ETagRoundTrip(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := authztest.ID("company_a")
	if _, _, err := e.store.InsertOrg(t.Context(), orgs.Org{ID: id, Kind: orgs.KindCompany, Name: "Matris A", Slug: "company-a", Status: orgs.StatusActive}); err != nil {
		t.Fatal(err)
	}
	app := newHTTP(t, e)
	path := "/api/v1/orgs/" + id.String()
	rec := call(t, app, "anonymous", http.MethodGet, path, "", nil)
	etag := rec.Header().Get("ETag")
	if rec.Code != http.StatusOK || !strings.HasPrefix(etag, `"`) || !strings.Contains(rec.Body.String(), `"follower_count":null`) {
		t.Fatalf("getOrg = %d %s (ETag %q)", rec.Code, rec.Body, etag)
	}
	body := `{"description": "Yeni açıklama", "website_url": "https://matris.example"}`
	if rec := call(t, app, "company_admin", http.MethodPatch, path, body, nil); rec.Code != http.StatusPreconditionRequired {
		t.Fatalf("PATCH without If-Match = %d %s", rec.Code, rec.Body)
	}
	rec = call(t, app, "company_admin", http.MethodPatch, path, body, map[string]string{"If-Match": etag})
	if rec.Code != http.StatusOK || rec.Header().Get("ETag") == etag || !strings.Contains(rec.Body.String(), "Yeni açıklama") {
		t.Fatalf("PATCH = %d %s", rec.Code, rec.Body)
	}
	if rec := call(t, app, "company_admin", http.MethodPatch, path, body, map[string]string{"If-Match": etag}); rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("PATCH with the old ETag = %d", rec.Code)
	}
	// Another company's admin: the management surface does not exist for it.
	if rec := call(t, app, "other_company_member", http.MethodPatch, path, body, map[string]string{"If-Match": etag}); rec.Code != http.StatusNotFound {
		t.Fatalf("PATCH by another company = %d", rec.Code)
	}
}
