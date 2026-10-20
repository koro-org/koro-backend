package events_test

import (
	"net/http"
	"strings"
	"testing"
)

// TestHTTP_EventExports: createEventExport answers 202 with ExportDTO (04
// §2.6), include_sensitive is a 422 of the service (after authz), a retry
// with the same Idempotency-Key replays the export; the list carries the
// requester and the counts; the download is {url, expires_at} with
// Cache-Control no-store, and 409 events.export_not_ready while queued.
func TestHTTP_EventExports(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.exportEvent(t, "student")
	app := newHTTP(t, e)
	base := "/api/v1/events/" + ev.Event.ID.String() + "/exports"
	body := `{"reason":"` + exportWhy + `","fields":["first_name","answers.motivation"],"format":"csv","reauth":{"password":"` +
		goodPassword + `"}}`
	sensitive := `{"reason":"` + exportWhy + `","fields":["first_name"],"include_sensitive":true,"reauth":{"password":"` +
		goodPassword + `"}}`
	rec, out := call(t, app, "student", http.MethodPost, base, sensitive, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("student: %d %v (authz before the body)", rec.Code, out)
	}
	rec, out = call(t, app, "club_admin", http.MethodPost, base, sensitive, nil)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "body.include_sensitive") {
		t.Fatalf("include_sensitive: %d %v", rec.Code, out)
	}
	key := map[string]string{"Idempotency-Key": "export-http-0000001"}
	rec, out = call(t, app, "club_admin", http.MethodPost, base, body, key)
	if rec.Code != http.StatusAccepted || out["status"] != "queued" || out["format"] != "csv" || out["expires_at"] == nil ||
		out["row_count"] != nil || out["download_count"] != 0.0 {
		t.Fatalf("create: %d %v", rec.Code, out)
	}
	id := out["export_id"].(string)
	rec, out = call(t, app, "club_admin", http.MethodPost, base, body, key)
	if rec.Code != http.StatusAccepted || out["export_id"] != id || rec.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %d %v", rec.Code, out)
	}
	rec, out = call(t, app, "club_admin", http.MethodGet, base+"/"+id+"/download", "", nil)
	if rec.Code != http.StatusConflict || out["code"] != "events.export_not_ready" {
		t.Fatalf("queued download: %d %v", rec.Code, out)
	}
	e.build(t)
	rec, out = call(t, app, "dpo", http.MethodGet, base, "", nil)
	items, _ := out["items"].([]any)
	if rec.Code != http.StatusOK || len(items) != 1 {
		t.Fatalf("list: %d %v", rec.Code, out)
	}
	item := items[0].(map[string]any)
	if item["status"] != "ready" || item["row_count"] != 1.0 || item["requested_by"] == "" || item["reason"] != exportWhy {
		t.Fatalf("list item: %v", item)
	}
	rec, out = call(t, app, "club_admin", http.MethodGet, base+"/"+id+"/download", "", nil)
	if rec.Code != http.StatusOK || out["url"] == "" || out["expires_at"] == nil || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("download: %d %v %v", rec.Code, out, rec.Header())
	}
	rec, out = call(t, app, "club_admin", http.MethodGet, base+"/not-a-uuid/download", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("malformed export id: %d %v", rec.Code, out)
	}
}
