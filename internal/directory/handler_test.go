package directory_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
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

// TestHTTP_AdminCatalogLists (BE-128): the managers' lists show what the
// public catalog hides — inactive universities and skills (both skill
// names), every e-mail domain of a university — and a faculty can be
// added so that a department can name it. Only platform_admin manages.
func TestHTTP_AdminCatalogLists(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	app := newHTTP(t, e)
	ege := e.university(t, "U-1", "Ege Üniversitesi", "İzmir")
	gone := e.university(t, "U-2", "Kapanan Üniversite", "Bursa")
	off := false
	if _, err := e.svc.AdminUpdateUniversity(t.Context(), pad(t), gone.ID, directory.UniversityPatch{IsActive: &off}); err != nil {
		t.Fatal(err)
	}
	type list struct {
		Items []map[string]any `json:"items"`
	}
	get := func(persona, path string) (int, list) {
		t.Helper()
		rec := call(t, app, persona, http.MethodGet, path, "", nil)
		var l list
		_ = json.Unmarshal(rec.Body.Bytes(), &l)
		return rec.Code, l
	}
	field := func(l list, k string) []any {
		var out []any
		for _, it := range l.Items {
			out = append(out, it[k])
		}
		return out
	}

	// Universities: all by default, then by state; the public list hides the closed one.
	if code, l := get("platform_admin", "/api/v1/admin/universities?sort=name"); code != http.StatusOK ||
		fmt.Sprint(field(l, "name")) != "[Ege Üniversitesi Kapanan Üniversite]" || fmt.Sprint(field(l, "is_active")) != "[true false]" ||
		l.Items[0]["yok_code"] != "U-1" {
		t.Fatalf("admin universities = %d %v", code, l)
	}
	if code, l := get("platform_admin", "/api/v1/admin/universities?state=inactive"); code != http.StatusOK || len(l.Items) != 1 ||
		l.Items[0]["university_id"] != gone.ID.String() {
		t.Fatalf("inactive universities = %d %v", code, l)
	}
	if _, l := get("anonymous", "/api/v1/universities"); len(l.Items) != 1 {
		t.Fatalf("public universities = %v", l)
	}

	// E-mail domains: every one, active or not.
	for d, active := range map[string]bool{"ege.edu.tr": true, "mail.ege.edu.tr": false} {
		body := `{"domain_kind":"mixed","is_active":` + strconv.FormatBool(active) + `}`
		if rec := call(t, app, "platform_admin", http.MethodPut, "/api/v1/admin/universities/"+ege.ID.String()+"/email-domains/"+d, body, nil); rec.Code != http.StatusOK {
			t.Fatalf("put domain %s = %d %s", d, rec.Code, rec.Body)
		}
	}
	if code, l := get("platform_admin", "/api/v1/admin/universities/"+ege.ID.String()+"/email-domains"); code != http.StatusOK ||
		fmt.Sprint(field(l, "domain")) != "[ege.edu.tr mail.ege.edu.tr]" || fmt.Sprint(field(l, "is_active")) != "[true false]" {
		t.Fatalf("domains = %d %v", code, l)
	}
	if code, _ := get("platform_admin", "/api/v1/admin/universities/00000000-0000-7000-8000-0000000000aa/email-domains"); code != http.StatusNotFound {
		t.Fatalf("domains of an unknown university = %d", code)
	}

	// Faculties: created active, named by a department; a second of that name is 409.
	path := "/api/v1/admin/universities/" + ege.ID.String() + "/faculties"
	rec := call(t, app, "platform_admin", http.MethodPost, path, `{"name":"Mühendislik Fakültesi","yok_code":"F-1"}`, nil)
	var fac map[string]any
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &fac) != nil || fac["name"] != "Mühendislik Fakültesi" || fac["yok_code"] != "F-1" {
		t.Fatalf("create faculty = %d %s", rec.Code, rec.Body)
	}
	if rec := call(t, app, "platform_admin", http.MethodPost, path, `{"name":"Mühendislik Fakültesi"}`, nil); rec.Code != http.StatusConflict ||
		!bytes.Contains(rec.Body.Bytes(), []byte("directory.faculty_exists")) {
		t.Fatalf("duplicate faculty = %d %s", rec.Code, rec.Body)
	}
	dep := `{"faculty_id":"` + fac["faculty_id"].(string) + `","name":"Bilgisayar Mühendisliği","degree_level":"bachelor"}`
	if rec := call(t, app, "platform_admin", http.MethodPost, "/api/v1/admin/universities/"+ege.ID.String()+"/departments", dep, nil); rec.Code != http.StatusCreated {
		t.Fatalf("department in the new faculty = %d %s", rec.Code, rec.Body)
	}
	if rec := call(t, app, "anonymous", http.MethodGet, "/api/v1/universities/"+ege.ID.String(), "", nil); !bytes.Contains(rec.Body.Bytes(), []byte(`"Mühendislik Fakültesi"`)) {
		t.Fatalf("getUniversity faculties = %s", rec.Body)
	}
	if rec := call(t, app, "platform_admin", http.MethodPost, "/api/v1/admin/universities/00000000-0000-7000-8000-0000000000aa/faculties",
		`{"name":"Hukuk Fakültesi"}`, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("faculty of an unknown university = %d", rec.Code)
	}

	// Skills: both names and the inactive ones.
	if _, err := e.svc.ImportReference(t.Context(), directory.ReferenceData{Skills: []directory.RefSkill{
		{Slug: "teamwork", NameTR: "Takım çalışması", NameEN: "Teamwork", Category: "soft"},
		{Slug: "cobol", NameTR: "COBOL", Category: "technical"},
	}}); err != nil {
		t.Fatal(err)
	}
	_, l := get("platform_admin", "/api/v1/admin/skills?state=all")
	var cobol string
	for _, it := range l.Items {
		if it["slug"] == "cobol" {
			cobol = it["skill_id"].(string)
		}
	}
	if rec := call(t, app, "platform_admin", http.MethodPatch, "/api/v1/admin/skills/"+cobol, `{"is_active":false}`, nil); rec.Code != http.StatusOK {
		t.Fatalf("deactivate = %d %s", rec.Code, rec.Body)
	}
	if code, l := get("platform_admin", "/api/v1/admin/skills"); code != http.StatusOK || len(l.Items) != 2 {
		t.Fatalf("admin skills = %d %v", code, l)
	}
	if code, l := get("platform_admin", "/api/v1/admin/skills?state=inactive"); code != http.StatusOK || len(l.Items) != 1 ||
		l.Items[0]["slug"] != "cobol" || l.Items[0]["is_active"] != false {
		t.Fatalf("inactive skills = %d %v", code, l)
	}
	if _, l := get("platform_admin", "/api/v1/admin/skills?state=active&q=takim"); len(l.Items) != 1 || l.Items[0]["name_tr"] != "Takım çalışması" ||
		l.Items[0]["name_en"] != "Teamwork" {
		t.Fatalf("active skills by name = %v", l)
	}
	if _, l := get("anonymous", "/api/v1/skills"); len(l.Items) != 1 {
		t.Fatalf("public skills = %v", l)
	}

	// Only platform_admin manages the catalog (03 §5.3).
	for _, p := range []string{"support", "moderator", "university_rep", "student"} {
		for _, path := range []string{"/api/v1/admin/universities", "/api/v1/admin/skills", "/api/v1/admin/universities/" + ege.ID.String() + "/email-domains"} {
			if code, _ := get(p, path); code != http.StatusForbidden {
				t.Errorf("%s %s = %d, want 403", p, path, code)
			}
		}
		if rec := call(t, app, p, http.MethodPost, path, `{"name":"Tıp Fakültesi"}`, nil); rec.Code != http.StatusForbidden {
			t.Errorf("%s create faculty = %d, want 403", p, rec.Code)
		}
	}
}
