package directory_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"koro/internal/authz/authztest"
	"koro/internal/directory"
	"koro/internal/platform/httpx"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
)

// newHTTP serves the directory operations of e with the persona resolver
// and the Idempotency-Key middleware (as cmd/koro does).
func newHTTP(t *testing.T, e *env) *httpx.App {
	t.Helper()
	app, err := httpx.New(httpx.Deps{Log: obs.Discard(), Actors: authztest.NewResolver(append(authztest.Personas(), authztest.Helpers()...)...)})
	if err != nil {
		t.Fatal(err)
	}
	app.API.UseMiddleware(idempotency.Middleware(app.API))
	directory.Register(app.V1, e.svc)
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

// TestHTTP_ClubDTO: getClub returns the org profile, membership_private and
// a null member_count for a sensitive club, the count from 10 members for
// another, and the ETag of updateClub.
func TestHTTP_ClubDTO(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	uni := e.university(t, "U-1", "Ege Üniversitesi", "İzmir")
	open := e.club(t, uni, "Fotoğraf Kulübü", directory.CategoryArts)
	sensitive := e.club(t, uni, "İnanç Topluluğu", directory.CategoryReligious)
	e.members.set(open.Club.OrgID, 12)
	e.members.set(sensitive.Club.OrgID, 50)
	app := newHTTP(t, e)
	var dto map[string]any
	rec := call(t, app, "anonymous", http.MethodGet, "/api/v1/clubs/"+sensitive.Club.OrgID.String(), "", nil)
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &dto) != nil {
		t.Fatalf("getClub = %d %s", rec.Code, rec.Body)
	}
	if v, ok := dto["member_count"]; !ok || v != nil || dto["membership_private"] != true {
		t.Fatalf("sensitive club DTO = %s", rec.Body)
	}
	if org, _ := dto["org"].(map[string]any); org["name"] != "İnanç Topluluğu" || org["kind"] != "club" {
		t.Fatalf("org part = %v", dto["org"])
	}
	if rec.Header().Get("ETag") != sensitive.ETag {
		t.Fatalf("ETag = %q, want %q", rec.Header().Get("ETag"), sensitive.ETag)
	}
	rec = call(t, app, "anonymous", http.MethodGet, "/api/v1/clubs/"+open.Club.OrgID.String(), "", nil)
	if json.Unmarshal(rec.Body.Bytes(), &dto) != nil || dto["member_count"] != float64(12) || dto["membership_private"] != false {
		t.Fatalf("open club DTO = %s", rec.Body)
	}
	// listClubs: Turkish search through the query string, card DTO, cursor.
	rec = call(t, app, "student", http.MethodGet, "/api/v1/clubs?q="+url.QueryEscape("FOTOĞRAF")+"&sort=name&limit=1", "", nil)
	var list struct {
		Items []map[string]any `json:"items"`
		Next  string           `json:"next_cursor"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil || len(list.Items) != 1 ||
		list.Items[0]["name"] != "Fotoğraf Kulübü" || list.Next != "" {
		t.Fatalf("listClubs = %d %s", rec.Code, rec.Body)
	}
	rec = call(t, app, "student", http.MethodGet, "/api/v1/clubs?sort=name&limit=1", "", nil)
	if json.Unmarshal(rec.Body.Bytes(), &list) != nil || list.Next == "" {
		t.Fatalf("first page = %s", rec.Body)
	}
	rec = call(t, app, "student", http.MethodGet, "/api/v1/clubs?sort=name&limit=1&cursor="+list.Next, "", nil)
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil || len(list.Items) != 1 ||
		list.Items[0]["member_count"] != nil {
		t.Fatalf("second page = %d %s", rec.Code, rec.Body)
	}
	// A cursor with other filters is refused.
	rec = call(t, app, "student", http.MethodGet, "/api/v1/clubs?sort=name&limit=1&category=arts&cursor="+list.Next, "", nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("foreign cursor = %d", rec.Code)
	}
}

// TestHTTP_ListSkills_AcceptLanguage: name follows Accept-Language.
func TestHTTP_ListSkills_AcceptLanguage(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if _, err := e.svc.ImportReference(t.Context(), directory.ReferenceData{
		Skills: []directory.RefSkill{{Slug: "teamwork", NameTR: "Takım çalışması", NameEN: "Teamwork", Category: "soft"}},
	}); err != nil {
		t.Fatal(err)
	}
	app := newHTTP(t, e)
	for lang, want := range map[string]string{"": "Takım çalışması", "en-US,en;q=0.9": "Teamwork", "de": "Takım çalışması"} {
		rec := call(t, app, "anonymous", http.MethodGet, "/api/v1/skills", "", map[string]string{"Accept-Language": lang})
		var list struct {
			Items []map[string]any `json:"items"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil || len(list.Items) != 1 ||
			list.Items[0]["name"] != want {
			t.Fatalf("Accept-Language %q: %d %s", lang, rec.Code, rec.Body)
		}
	}
}
