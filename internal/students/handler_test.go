package students_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/consent"
	"koro/internal/platform/httpx"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/ratelimit"
	"koro/internal/students"
)

// newHTTP serves the students operations of e with the persona resolver,
// the rate limit and Idempotency-Key middlewares (as cmd/koro does).
func newHTTP(t *testing.T, e *env) *httpx.App {
	t.Helper()
	e.resolver = authztest.NewResolver(append(authztest.Personas(), authztest.Helpers()...)...)
	app, err := httpx.New(httpx.Deps{Log: obs.Discard(), Actors: e.resolver})
	if err != nil {
		t.Fatal(err)
	}
	app.API.UseMiddleware(ratelimit.Middleware(app.API, ratelimit.NewMem(e.clk), obs.Discard()))
	app.API.UseMiddleware(idempotency.Middleware(app.API))
	students.Register(app.V1, e.svc)
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

// callAs sends a request as actor a (registered with the resolver of
// newHTTP under its own token); hdr is a list of header name, value pairs.
func (e *env) callAs(t *testing.T, app *httpx.App, a authz.Actor, method, path string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	token := "actor-" + a.UserID.String()
	e.resolver.Add(token, a)
	req := httptest.NewRequestWithContext(t.Context(), method, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	return rec
}

func code(rec *httptest.ResponseRecorder) string {
	var p struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	return p.Code
}

// TestHTTP_UpdateProfile_UniversityCannotBeSent (04 §2.4): university_id is
// not a member of the PATCH body: sending it is 422 and nothing changes;
// the profile keeps the verified university.
func TestHTTP_UpdateProfile_UniversityCannotBeSent(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	app := newHTTP(t, e)
	other := uuid.NewV7().String()
	rec := call(t, app, "student", http.MethodPatch, "/api/v1/me/student-profile",
		`{"university_id":"`+other+`","headline":"Merhaba"}`, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("PATCH with university_id = %d %s", rec.Code, rec.Body)
	}
	if _, err := e.store.GetProfile(t.Context(), authztest.ID("student")); !errors.Is(err, students.ErrNotFound) {
		t.Fatal("the refused request wrote a profile")
	}
	rec = call(t, app, "student", http.MethodPatch, "/api/v1/me/student-profile", `{"headline":"Merhaba","city":""}`, nil)
	var got students.StudentProfileSelfDTO
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil ||
		got.University.UniversityID != authztest.UniversityID("uni_a").String() || got.Headline != "Merhaba" {
		t.Fatalf("PATCH = %d %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("personal data answered without Cache-Control: no-store")
	}
}

// TestHTTP_NonStudents_403VerificationRequired (03 §2.3 k-uv): accounts
// without a student verification (a company admin of another company, a
// recruiter, an unverified user, platform staff) get 403
// students.verification_required on /me/student-profile, /me/privacy and
// /me/cvs (BE-123: documented in the contract); anonymous 401.
func TestHTTP_NonStudents_403VerificationRequired(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	app := newHTTP(t, e)
	for _, p := range []string{"user_unverified", "company_member", "company_admin", "other_company_member", "university_rep", "dpo"} {
		for _, path := range []string{"/api/v1/me/student-profile", "/api/v1/me/privacy", "/api/v1/me/cvs"} {
			rec := call(t, app, p, http.MethodGet, path, "", nil)
			if rec.Code != http.StatusForbidden || code(rec) != "students.verification_required" {
				t.Fatalf("%s %s = %d %s", p, path, rec.Code, rec.Body)
			}
		}
	}
	if rec := call(t, app, "anonymous", http.MethodGet, "/api/v1/me/student-profile", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d", rec.Code)
	}
}

// TestHTTP_Experience_OtherStudentAndCompany_IDOR: student's experience ID
// is 404 for another student (update and delete), 403 for a company
// account (no student path at all), 404 when malformed; the owner edits it.
func TestHTTP_Experience_OtherStudentAndCompany_IDOR(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	app := newHTTP(t, e)
	rec := call(t, app, "student", http.MethodPost, "/api/v1/me/student-profile/experiences",
		`{"kind":"internship","title":"Stajyer","organization_name":"Acme","start_month":"2025-07","end_month":"2025-09"}`, nil)
	var x students.StudentExperienceDTO
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &x) != nil || x.EndMonth != "2025-09" {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	path := "/api/v1/me/student-profile/experiences/" + x.ExperienceID
	for _, tc := range []struct {
		persona, method, body string
		want                  int
	}{
		{"student_b", http.MethodPatch, `{"title":"Ele geçirildi"}`, http.StatusNotFound},
		{"student_b", http.MethodDelete, "", http.StatusNotFound},
		{"other_club_admin", http.MethodDelete, "", http.StatusNotFound},
		{"company_admin", http.MethodPatch, `{"title":"Ele geçirildi"}`, http.StatusForbidden},
		{"other_company_member", http.MethodDelete, "", http.StatusForbidden},
	} {
		if rec := call(t, app, tc.persona, tc.method, path, tc.body, nil); rec.Code != tc.want {
			t.Fatalf("%s %s = %d %s", tc.persona, tc.method, rec.Code, rec.Body)
		}
	}
	if rec := call(t, app, "student", http.MethodDelete, "/api/v1/me/student-profile/experiences/not-a-uuid", "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("malformed ID = %d", rec.Code)
	}
	if rec := call(t, app, "student", http.MethodPatch, path, `{"end_month":""}`, nil); rec.Code != http.StatusOK ||
		strings.Contains(rec.Body.String(), "end_month") {
		t.Fatalf("owner clears end_month = %d %s", rec.Code, rec.Body)
	}
	if rec := call(t, app, "student", http.MethodDelete, path, "", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("owner delete = %d %s", rec.Code, rec.Body)
	}
}

// TestHTTP_Privacy_ConsentAndMinor: turning discoverable on carries the
// consent text (recorded with the X-Koro-Client channel); without it 422
// consent.proof_required; a minor 422 students.minor_restricted.
func TestHTTP_Privacy_ConsentAndMinor(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	app := newHTTP(t, e)
	sum := sha256.Sum256([]byte("metin"))
	notice := uuid.NewV7().String()
	on := `{"discoverable":true,"consent":{"discovery_profile":{"notice_version_id":"` + notice + `","text_sha256":"` +
		hex.EncodeToString(sum[:]) + `"}}}`
	rec := call(t, app, "student", http.MethodPatch, "/api/v1/me/privacy", on, map[string]string{"X-Koro-Client": "mobile"})
	var got students.StudentPrivacyDTO
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil || !got.Discoverable || got.ShowContactOnGrant {
		t.Fatalf("turn on = %d %s", rec.Code, rec.Body)
	}
	recs := e.consent.records()
	if len(recs) != 1 || recs[0].Channel != consent.ChannelMobile || recs[0].NoticeVersionID.String() != notice {
		t.Fatalf("consent = %+v", recs)
	}
	if rec := call(t, app, "student_b", http.MethodPatch, "/api/v1/me/privacy", `{"show_contact_on_grant":true}`, nil); rec.Code != http.StatusUnprocessableEntity ||
		code(rec) != "consent.proof_required" {
		t.Fatalf("without proof = %d %s", rec.Code, rec.Body)
	}
	if rec := call(t, app, "student_minor", http.MethodPatch, "/api/v1/me/privacy", on, nil); rec.Code != http.StatusUnprocessableEntity ||
		code(rec) != "students.minor_restricted" {
		t.Fatalf("minor = %d %s", rec.Code, rec.Body)
	}
	if rec := call(t, app, "student", http.MethodGet, "/api/v1/me/privacy", "", nil); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"discoverable":true`) {
		t.Fatalf("read back = %d %s", rec.Code, rec.Body)
	}
}

// TestHTTP_Sections_PutAndRead: PUT replaces the lists; the profile read
// shows them; unknown members are refused.
func TestHTTP_Sections_PutAndRead(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	app := newHTTP(t, e)
	rec := call(t, app, "student", http.MethodPut, "/api/v1/me/student-profile/skills",
		`{"items":[{"skill_id":"`+e.skill1.String()+`","level":3},{"skill_id":"`+e.skill2.String()+`"}]}`, nil)
	if rec.Code != http.StatusOK || strings.Count(rec.Body.String(), "skill_id") != 2 {
		t.Fatalf("skills = %d %s", rec.Code, rec.Body)
	}
	rec = call(t, app, "student", http.MethodPut, "/api/v1/me/student-profile/languages",
		`{"items":[{"language_code":"en","level":"C1"},{"language_code":"tr","level":"native"}]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("languages = %d %s", rec.Code, rec.Body)
	}
	rec = call(t, app, "student", http.MethodGet, "/api/v1/me/student-profile", "", nil)
	var got students.StudentProfileSelfDTO
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil || len(got.Skills) != 2 || len(got.Languages) != 2 ||
		got.Privacy.ActivityRankingChoice != "default" {
		t.Fatalf("profile = %d %s", rec.Code, rec.Body)
	}
	if rec := call(t, app, "student", http.MethodPut, "/api/v1/me/student-profile/languages",
		`{"items":[{"language_code":"en","level":"C1","talent_score":9}]}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown member = %d", rec.Code)
	}
}
