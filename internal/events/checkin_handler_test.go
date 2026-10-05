package events_test

import (
	"net/http"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz/authztest"
	"koro/internal/events"
	"koro/internal/platform/httpx"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/ratelimit"
)

// newLimitedHTTP is newHTTP plus the rate limit middleware (in-memory
// limiter on the env clock), as cmd/koro mounts it.
func newLimitedHTTP(t *testing.T, e *env) *httpx.App {
	t.Helper()
	app, err := httpx.New(httpx.Deps{Log: obs.Discard(), Actors: authztest.NewResolver(append(authztest.Personas(), authztest.Helpers()...)...)})
	if err != nil {
		t.Fatal(err)
	}
	app.API.UseMiddleware(ratelimit.Middleware(app.API, ratelimit.NewMem(e.clk), obs.Discard()))
	app.API.UseMiddleware(idempotency.Middleware(app.API))
	events.Register(app.V1, e.svc)
	return app
}

func scanBody(payload string, scan uuid.UUID, at time.Time) string {
	return `{"method":"qr","token":"` + payload + `","client_scan_id":"` + scan.String() + `","scanned_at":"` +
		at.UTC().Format(time.RFC3339) + `","device_id":"ipad-kapi-1"}`
}

// TestHTTP_TicketsAndCheckins: the DTOs of 04 §E.3 and §E.5 over HTTP:
// the ticket, the resend limit (ticket_resend: 3 per hour, then 429), the
// check-in with its required Idempotency-Key (a replay answers the same
// body), wrong_event without the attendee, the void, the list, the
// counters, the organiser's reissue and the attendance close.
func TestHTTP_TicketsAndCheckins(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.doorEvent(t)
	other := e.doorEvent(t)
	app := newLimitedHTTP(t, e)
	r, _ := e.ticketOf(t, adultStudent(t), ev.Event.ID)
	_, foreign := e.ticketOf(t, persona(t, "club_member"), other.Event.ID)
	me := "/api/v1/me/registrations/" + r.Registration.ID.String()

	rec, out := call(t, app, "student", http.MethodGet, me+"/ticket", "", nil)
	ev1, _ := out["event"].(map[string]any)
	payload, _ := out["qr_payload"].(string)
	if rec.Code != http.StatusOK || !strings.HasPrefix(payload, "KORO1:") || out["version"] != 1.0 || out["manual_code"] == nil ||
		out["issued_at"] == nil || ev1["event_id"] != ev.Event.ID.String() {
		t.Fatalf("getMyTicket: %d %v", rec.Code, out)
	}
	if rec, _ := call(t, app, "club_member", http.MethodGet, me+"/ticket", "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("another's ticket: %d", rec.Code)
	}
	for i := range 3 {
		if rec, out := call(t, app, "student", http.MethodPost, me+"/ticket/resend", "", nil); rec.Code != http.StatusAccepted {
			t.Fatalf("resend %d: %d %v", i, rec.Code, out)
		}
	}
	rec, out = call(t, app, "student", http.MethodPost, me+"/ticket/resend", "", nil)
	if rec.Code != http.StatusTooManyRequests || out["code"] != "rate_limited" || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("fourth resend: %d %v", rec.Code, out)
	}

	e.clk.Set(doorStart)
	door := "/api/v1/events/" + ev.Event.ID.String() + "/check-ins"
	body := scanBody(payload, uuid.NewV7(), doorStart)
	if rec, _ := call(t, app, "event_staff", http.MethodPost, door, body, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("without Idempotency-Key: %d", rec.Code)
	}
	key := map[string]string{"Idempotency-Key": "scan-http-0000001"}
	rec, out = call(t, app, "event_staff", http.MethodPost, door, body, key)
	att, _ := out["attendee"].(map[string]any)
	prev, hasPrev := out["previous"]
	if rec.Code != http.StatusOK || out["result"] != "ok" || out["checkin_id"] == nil || att["attendee_display"] != "Ayla Y." ||
		out["checked_in_at"] != doorStart.UTC().Format(time.RFC3339) || !hasPrev || prev != nil {
		t.Fatalf("check-in: %d %v", rec.Code, out)
	}
	checkin := out["checkin_id"].(string)
	rec, again := call(t, app, "event_staff", http.MethodPost, door, body, key)
	if rec.Code != http.StatusOK || again["checkin_id"] != checkin || rec.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %d %v", rec.Code, again)
	}
	rec, out = call(t, app, "event_staff", http.MethodPost, door, scanBody(payload, uuid.NewV7(), doorStart),
		map[string]string{"Idempotency-Key": "scan-http-0000002"})
	if prev, _ := out["previous"].(map[string]any); rec.Code != http.StatusOK || out["result"] != "already_checked_in" ||
		prev["staff_display"] != "Ayla Y." {
		t.Fatalf("second scan: %d %v", rec.Code, out)
	}
	rec, out = call(t, app, "event_staff", http.MethodPost, door, scanBody(foreign.Payload, uuid.NewV7(), doorStart),
		map[string]string{"Idempotency-Key": "scan-http-0000003"})
	if _, has := out["attendee"]; rec.Code != http.StatusOK || out["result"] != "wrong_event" || has {
		t.Fatalf("wrong event: %d %v", rec.Code, out)
	}
	if rec, out := call(t, app, "club_member", http.MethodPost, door, scanBody(payload, uuid.NewV7(), doorStart),
		map[string]string{"Idempotency-Key": "scan-http-0000004"}); rec.Code != http.StatusForbidden {
		t.Fatalf("a member scans: %d %v", rec.Code, out)
	}

	rec, out = call(t, app, "event_staff", http.MethodGet, door+"?limit=10", "", nil)
	items, _ := out["items"].([]any)
	if rec.Code != http.StatusOK || len(items) != 1 || items[0].(map[string]any)["attendee_display"] != "Ayla Y." ||
		items[0].(map[string]any)["method"] != "qr" {
		t.Fatalf("list: %d %v", rec.Code, out)
	}
	rec, out = call(t, app, "event_staff", http.MethodGet, door+"/stats", "", nil)
	if rec.Code != http.StatusOK || out["confirmed"] != 1.0 || out["checked_in"] != 1.0 || out["voided"] != 0.0 ||
		out["updated_at"] == nil {
		t.Fatalf("stats: %d %v", rec.Code, out)
	}
	void := door + "/" + checkin + "/void"
	if rec, _ := call(t, app, "event_staff", http.MethodPost, void, `{"void_reason":"Yanlış kişi okutuldu"}`, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("a scanner voids: %d", rec.Code)
	}
	rec, out = call(t, app, "event_manager_a", http.MethodPost, void, `{"void_reason":"Yanlış kişi okutuldu"}`, nil)
	if rec.Code != http.StatusOK || out["checkin_id"] != checkin || out["registration_status"] != "confirmed" || out["voided_at"] == nil {
		t.Fatalf("void: %d %v", rec.Code, out)
	}

	reissue := "/api/v1/events/" + ev.Event.ID.String() + "/registrations/" + r.Registration.ID.String() + "/ticket/reissue"
	rec, out = call(t, app, "club_admin", http.MethodPost, reissue, "", nil)
	if _, leak := out["qr_payload"]; rec.Code != http.StatusOK || out["version"] != 2.0 || leak {
		t.Fatalf("organiser reissue: %d %v", rec.Code, out)
	}
	rec, out = call(t, app, "student", http.MethodPost, me+"/ticket/reissue", "", nil)
	if rec.Code != http.StatusOK || out["version"] != 3.0 || out["qr_payload"] == payload {
		t.Fatalf("own reissue: %d %v", rec.Code, out)
	}
	finalize := "/api/v1/events/" + ev.Event.ID.String() + "/attendance/finalize"
	rec, out = call(t, app, "club_admin", http.MethodPost, finalize, "", nil)
	if rec.Code != http.StatusConflict || out["code"] != "events.invalid_state" {
		t.Fatalf("finalize before the end: %d %v", rec.Code, out)
	}
	e.clk.Set(ev.Event.EndsAt.Add(time.Hour))
	rec, out = call(t, app, "event_manager_a", http.MethodPost, finalize, "", nil)
	if rec.Code != http.StatusOK || out["no_show_count"] != 1.0 {
		t.Fatalf("finalize: %d %v", rec.Code, out)
	}
}
