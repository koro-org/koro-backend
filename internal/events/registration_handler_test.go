package events_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"koro/internal/authz/authztest"
	"koro/internal/events"
	"koro/internal/platform/httpx"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
)

// newHTTP serves the events operations with the persona resolver and the
// Idempotency-Key middleware (as cmd/koro does).
func newHTTP(t *testing.T, e *env) *httpx.App {
	t.Helper()
	app, err := httpx.New(httpx.Deps{Log: obs.Discard(), Actors: authztest.NewResolver(append(authztest.Personas(), authztest.Helpers()...)...)})
	if err != nil {
		t.Fatal(err)
	}
	app.API.UseMiddleware(idempotency.Middleware(app.API))
	events.Register(app.V1, e.svc)
	return app
}

func call(t *testing.T, app *httpx.App, persona, method, path, body string, hdr map[string]string) (*httptest.ResponseRecorder, map[string]any) {
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
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

// TestHTTP_CreateRegistration: the Idempotency-Key is required (422
// without), a retry replays the 201 with Idempotent-Replayed (04 §1.8,
// §E.2); the DTOs follow 04 §E.2 and §V.6.
func TestHTTP_CreateRegistration(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.regEvent(t, capacity(1, true), motivationField)
	app := newHTTP(t, e)
	path := "/api/v1/events/" + ev.Event.ID.String() + "/registrations"
	body := `{"answers":{"motivation":"Veri bilimi"}}`
	rec, out := call(t, app, "student", http.MethodPost, path, body, nil)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Idempotency-Key") {
		t.Fatalf("without a key: %d %v", rec.Code, out)
	}
	key := map[string]string{"Idempotency-Key": "reg-http-00000001", "X-Koro-Client": "mobile"}
	rec, out = call(t, app, "student", http.MethodPost, path, body, key)
	if rec.Code != http.StatusCreated || out["status"] != "confirmed" || out["ticket_available"] != true ||
		out["waitlist_position"] != nil || out["event"].(map[string]any)["display_timezone"] != "Europe/Istanbul" {
		t.Fatalf("first: %d %v", rec.Code, out)
	}
	if _, has := out["waitlist_position"]; !has {
		t.Fatalf("waitlist_position is not in the body (04 §E.2 shows null): %v", out)
	}
	id := out["registration_id"].(string)
	rec, out = call(t, app, "student", http.MethodPost, path, body, key)
	if rec.Code != http.StatusCreated || out["registration_id"] != id || rec.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %d %v %v", rec.Code, out, rec.Header())
	}
	rec, out = call(t, app, "club_member", http.MethodPost, path, body, map[string]string{"Idempotency-Key": "reg-http-00000002"})
	if rec.Code != http.StatusCreated || out["status"] != "waitlisted" || out["waitlist_position"] != 1.0 {
		t.Fatalf("waitlisted: %d %v", rec.Code, out)
	}
	rec, out = call(t, app, "user_unverified", http.MethodPost, path, body, map[string]string{"Idempotency-Key": "reg-http-00000003"})
	if rec.Code != http.StatusForbidden || out["code"] != "students.verification_required" {
		t.Fatalf("unverified: %d %v", rec.Code, out)
	}
	bad := `{"answers":{"motivation":"x"},"consents":[{"purpose_code":"event_sponsor_share","notice_version_id":"` + id +
		`","text_sha256":"zz"}]}`
	rec, _ = call(t, app, "other_club_admin", http.MethodPost, path, bad, map[string]string{"Idempotency-Key": "reg-http-00000004"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad digest: %d", rec.Code)
	}
	// The own view, the organiser's view and the break-glass read.
	rec, out = call(t, app, "student", http.MethodGet, "/api/v1/me/registrations/"+id, "", nil)
	if rec.Code != http.StatusOK || out["answers"].(map[string]any)["motivation"] != "Veri bilimi" {
		t.Fatalf("own: %d %v", rec.Code, out)
	}
	rec, out = call(t, app, "club_admin", http.MethodGet, path+"?sort=-created_at&limit=1", "", nil)
	items, _ := out["items"].([]any)
	if rec.Code != http.StatusOK || len(items) != 1 || out["next_cursor"] == nil {
		t.Fatalf("org list: %d %v", rec.Code, out)
	}
	first := items[0].(map[string]any)
	user := first["user"].(map[string]any)
	if first["status"] != "waitlisted" || first["source"] != "web" || user["last_name"] != "Yılmaz" || first["answers"] == nil {
		t.Fatalf("org item: %v", first)
	}
	rec, out = call(t, app, "club_admin", http.MethodGet, path+"?sort=-created_at&limit=1&cursor="+out["next_cursor"].(string), "", nil)
	if items, _ := out["items"].([]any); rec.Code != http.StatusOK || len(items) != 1 ||
		items[0].(map[string]any)["registration_id"] != id || items[0].(map[string]any)["source"] != "mobile" {
		t.Fatalf("org page 2: %d %v", rec.Code, out)
	}
	rec, out = call(t, app, "support", http.MethodGet, path+"/"+id, "", nil)
	if rec.Code != http.StatusForbidden || out["code"] != "authz.break_glass_reason_required" {
		t.Fatalf("support without a reason: %d %v", rec.Code, out)
	}
	rec, out = call(t, app, "support", http.MethodGet, path+"/"+id, "", map[string]string{
		"X-Koro-Access-Reason": "Destek talebi 4711: kayıt sorunu inceleniyor", "X-Koro-Ticket-Ref": "4711"})
	if rec.Code != http.StatusOK || out["registration_id"] != id {
		t.Fatalf("support with a reason: %d %v", rec.Code, out)
	}
	// The registrant cancels; the organiser decides nothing on an auto event.
	rec, out = call(t, app, "student", http.MethodPost, "/api/v1/me/registrations/"+id+"/cancel", `{"reason":"Program değişti"}`, nil)
	if rec.Code != http.StatusOK || out["status"] != "cancelled" {
		t.Fatalf("cancel: %d %v", rec.Code, out)
	}
	rec, out = call(t, app, "club_admin", http.MethodPost, path+"/"+id+"/decision", `{"decision":"confirmed"}`, nil)
	if rec.Code != http.StatusConflict || out["code"] != "events.invalid_state" {
		t.Fatalf("decision on an auto event: %d %v", rec.Code, out)
	}
	rec, out = call(t, app, "club_member", http.MethodGet, "/api/v1/me/registrations?upcoming=true", "", nil)
	if items, _ := out["items"].([]any); rec.Code != http.StatusOK || len(items) != 1 ||
		items[0].(map[string]any)["status"] != "confirmed" {
		t.Fatalf("my list after the promotion: %d %v", rec.Code, out)
	}
}
