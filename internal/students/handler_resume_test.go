package students_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"koro/internal/authz"
	"koro/internal/platform/httpx"
	"koro/internal/students"
)

// postKey sends an empty POST as a with an Idempotency-Key.
func (e *env) postKey(t *testing.T, app *httpx.App, a authz.Actor, path, key string) *httptest.ResponseRecorder {
	t.Helper()
	return e.callAs(t, app, a, http.MethodPost, path, "Idempotency-Key", key)
}

// TestHTTP_ResumeMyAccessGrant (04 §2.4, BE-088): the grant list shows
// resumable_until on a suspended grant; the resume is 403
// students.verification_required without a verification (a company
// account too), 404 for another user's or a malformed grant, 200 with the
// active grant and the company's name (no resumable_until, no reason) once
// verified; the same Idempotency-Key replays the answer, a new request is
// 409 students.invalid_state; after the window 409
// students.resume_window_expired.
func TestHTTP_ResumeMyAccessGrant(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	app := newHTTP(t, e)
	org, rec, _ := e.company("Acme")
	s, _ := e.granted(t, org)
	late, _ := e.granted(t, org)
	lapsed := e.lapse(t, s)
	e.lapse(t, late)
	g := e.suspendedGrant(t, s.UserID, org)
	path := "/api/v1/me/access-grants/" + g.ID.String() + "/resume"

	r := e.send(t, app, lapsed, http.MethodGet, "/api/v1/me/access-grants", "")
	var mine students.StudentAccessGrantList
	if r.Code != http.StatusOK || json.Unmarshal(r.Body.Bytes(), &mine) != nil || len(mine.Items) != 1 ||
		mine.Items[0].Status != "suspended" || mine.Items[0].ResumableUntil == nil ||
		!mine.Items[0].ResumableUntil.Equal(g.SuspendedAt.Add(window)) {
		t.Fatalf("my grants = %d %s", r.Code, r.Body)
	}
	for name, a := range map[string]authz.Actor{"lapsed student": lapsed, "company account": rec} {
		if r = e.send(t, app, a, http.MethodPost, path, ""); r.Code != http.StatusForbidden || code(r) != "students.verification_required" {
			t.Fatalf("%s = %d %s", name, r.Code, r.Body)
		}
	}

	e.clk.Advance(time.Hour)
	e.reverify(t, s, e.uniA, false)
	other := e.profiled(t)
	if r = e.send(t, app, other, http.MethodPost, path, ""); r.Code != http.StatusNotFound || code(r) != "not_found" {
		t.Fatalf("another student = %d %s", r.Code, r.Body)
	}
	if r = e.send(t, app, s, http.MethodPost, "/api/v1/me/access-grants/nope/resume", ""); r.Code != http.StatusNotFound {
		t.Fatalf("malformed grant = %d %s", r.Code, r.Body)
	}

	r = e.postKey(t, app, s, path, "resume-key-0001")
	var got students.StudentAccessGrantDTO
	if r.Code != http.StatusOK || json.Unmarshal(r.Body.Bytes(), &got) != nil || got.GrantID != g.ID.String() ||
		got.Status != "active" || got.Company.Name != "Acme" || got.ResumableUntil != nil || got.SuspendedAt != nil ||
		strings.Contains(r.Body.String(), "reason") {
		t.Fatalf("resume = %d %s", r.Code, r.Body)
	}
	if again := e.postKey(t, app, s, path, "resume-key-0001"); again.Code != http.StatusOK || again.Body.String() != r.Body.String() {
		t.Fatalf("replay = %d %s", again.Code, again.Body)
	}
	if r = e.send(t, app, s, http.MethodPost, path, ""); r.Code != http.StatusConflict || code(r) != "students.invalid_state" {
		t.Fatalf("resume of an active grant = %d %s", r.Code, r.Body)
	}
	if r = e.send(t, app, rec, http.MethodGet, "/api/v1/orgs/"+org.String()+"/students/"+s.UserID.String(), ""); r.Code != http.StatusOK {
		t.Fatalf("the company after the resume = %d %s", r.Code, r.Body)
	}

	// The other student verifies again only after the window.
	e.clk.Set(e.suspendedGrant(t, late.UserID, org).SuspendedAt.Add(window + time.Second))
	e.reverify(t, late, e.uniA, false)
	latePath := "/api/v1/me/access-grants/" + e.suspendedGrant(t, late.UserID, org).ID.String() + "/resume"
	if r = e.send(t, app, late, http.MethodPost, latePath, ""); r.Code != http.StatusConflict || code(r) != "students.resume_window_expired" {
		t.Fatalf("resume after the window = %d %s", r.Code, r.Body)
	}
}
