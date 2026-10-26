package students_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/httpx"
	"koro/internal/students"
)

// send is callAs with a JSON body.
func (e *env) send(t *testing.T, app *httpx.App, a authz.Actor, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	token := "actor-" + a.UserID.String()
	e.resolver.Add(token, a)
	req := httptest.NewRequestWithContext(t.Context(), method, path, bytes.NewBufferString(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	return rec
}

// TestHTTP_AccessGrants (04 §2.4): the student lists the company and
// revokes without a body (the reason is optional; 301 characters 422); the
// company's profile and CV paths answer no-store, then 404 after the
// revocation; cursors carry the next page; malformed IDs are 404 and a
// tampered cursor 422.
func TestHTTP_AccessGrants(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	app := newHTTP(t, e)
	org, rec, _ := e.company("Acme")
	s, _ := e.granted(t, org)
	e.withCV(t, s)
	second, _, _ := e.company("Beta")
	if err := e.svc.Grant(t.Context(), s.UserID, second, students.SourceContactRequest, uuid.NewV7()); err != nil {
		t.Fatal(err)
	}

	r := e.send(t, app, s, http.MethodGet, "/api/v1/me/access-grants?limit=1", "")
	var mine students.StudentAccessGrantList
	if r.Code != http.StatusOK || json.Unmarshal(r.Body.Bytes(), &mine) != nil || len(mine.Items) != 1 || mine.NextCursor == "" ||
		r.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("my grants = %d %s", r.Code, r.Body)
	}
	r = e.send(t, app, s, http.MethodGet, "/api/v1/me/access-grants?limit=1&cursor="+mine.NextCursor, "")
	var page2 students.StudentAccessGrantList
	if r.Code != http.StatusOK || json.Unmarshal(r.Body.Bytes(), &page2) != nil || len(page2.Items) != 1 || page2.NextCursor != "" ||
		page2.Items[0].Company.Name != "Acme" || page2.Items[0].Source != "contact_request" {
		t.Fatalf("my grants page 2 = %d %s", r.Code, r.Body)
	}
	if r = e.send(t, app, s, http.MethodGet, "/api/v1/me/access-grants?cursor=bm9wZQ", ""); r.Code != http.StatusUnprocessableEntity {
		t.Fatalf("tampered cursor = %d", r.Code)
	}

	base := "/api/v1/orgs/" + org.String() + "/students/" + s.UserID.String()
	r = e.send(t, app, rec, http.MethodGet, base, "")
	if r.Code != http.StatusOK || r.Header().Get("Cache-Control") != "no-store" || !strings.Contains(r.Body.String(), `"grant"`) {
		t.Fatalf("company profile = %d %s", r.Code, r.Body)
	}
	if r = e.send(t, app, rec, http.MethodGet, base+"/cv", ""); r.Code != http.StatusOK || !strings.Contains(r.Body.String(), `"url"`) {
		t.Fatalf("company CV = %d %s", r.Code, r.Body)
	}
	// BE-128: the name filter travels in the body; the deprecated GET
	// refuses it in the URL and still lists without it.
	r = e.send(t, app, rec, http.MethodPost, "/api/v1/orgs/"+org.String()+"/students/search", `{"q":"ada"}`)
	var cards students.StudentL3CardList
	if r.Code != http.StatusOK || json.Unmarshal(r.Body.Bytes(), &cards) != nil || len(cards.Items) != 1 {
		t.Fatalf("company search = %d %s", r.Code, r.Body)
	}
	if r = e.send(t, app, rec, http.MethodPost, "/api/v1/orgs/"+org.String()+"/students/search", `{"q":"zeynep"}`); r.Code != http.StatusOK ||
		!strings.Contains(r.Body.String(), `"items":[]`) {
		t.Fatalf("company search without a match = %d %s", r.Code, r.Body)
	}
	if r = e.send(t, app, rec, http.MethodGet, "/api/v1/orgs/"+org.String()+"/students?q=ada", ""); r.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(r.Body.String(), `"location":"query.q"`) || !strings.Contains(r.Body.String(), "queryCompanyGrantedStudents") {
		t.Fatalf("company list with q in the URL = %d %s", r.Code, r.Body)
	}
	r = e.send(t, app, rec, http.MethodGet, "/api/v1/orgs/"+org.String()+"/students", "")
	if r.Code != http.StatusOK || json.Unmarshal(r.Body.Bytes(), &cards) != nil || len(cards.Items) != 1 {
		t.Fatalf("company list = %d %s", r.Code, r.Body)
	}
	for _, path := range []string{"/api/v1/orgs/" + org.String() + "/students/nope", "/api/v1/orgs/nope/students/" + s.UserID.String()} {
		if r = e.send(t, app, rec, http.MethodGet, path, ""); r.Code != http.StatusNotFound {
			t.Fatalf("malformed %s = %d", path, r.Code)
		}
	}

	grant := page2.Items[0].GrantID
	if r = e.send(t, app, s, http.MethodPost, "/api/v1/me/access-grants/"+grant+"/revoke",
		`{"reason":"`+strings.Repeat("x", 301)+`"}`); r.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a 301-character reason = %d %s", r.Code, r.Body)
	}
	if r = e.send(t, app, s, http.MethodPost, "/api/v1/me/access-grants/"+grant+"/revoke", ""); r.Code != http.StatusNoContent {
		t.Fatalf("revoke without a body = %d %s", r.Code, r.Body)
	}
	for _, path := range []string{base, base + "/cv"} {
		if r = e.send(t, app, rec, http.MethodGet, path, ""); r.Code != http.StatusNotFound || code(r) != "not_found" {
			t.Fatalf("%s after the revocation = %d %s", path, r.Code, r.Body)
		}
	}
	if r = e.send(t, app, s, http.MethodPost, "/api/v1/me/access-grants/not-a-uuid/revoke", ""); r.Code != http.StatusNotFound {
		t.Fatalf("malformed grant = %d", r.Code)
	}
	if r = e.send(t, app, rec, http.MethodPost, "/api/v1/me/access-grants/"+grant+"/revoke", ""); r.Code != http.StatusNotFound ||
		code(r) != "not_found" {
		t.Fatalf("a company account on another user's grant = %d %s", r.Code, r.Body)
	}
	// The company's own path, with a reason.
	adm := e.profiled(t)
	if err := e.svc.Grant(t.Context(), adm.UserID, org, students.SourceContactRequest, uuid.NewV7()); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/orgs/" + org.String() + "/students/" + adm.UserID.String() + "/access-grant/revoke"
	if r = e.send(t, app, rec, http.MethodPost, path, `{"reason":"Pozisyon kapandı"}`); r.Code != http.StatusNoContent {
		t.Fatalf("company revoke = %d %s", r.Code, r.Body)
	}
	if r = e.send(t, app, rec, http.MethodPost, path, ""); r.Code != http.StatusNotFound {
		t.Fatalf("company revoke again = %d %s", r.Code, r.Body)
	}
}
