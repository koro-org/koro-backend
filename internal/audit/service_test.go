package audit_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/platform/httpx"
	"koro/internal/platform/obs"
)

// newHTTP serves the audit operations of e with the persona resolver.
func newHTTP(t *testing.T, e *env) *httpx.App {
	t.Helper()
	app, err := httpx.New(httpx.Deps{Log: obs.Discard(), Actors: authztest.NewResolver(append(authztest.Personas(), authztest.Helpers()...)...)})
	if err != nil {
		t.Fatal(err)
	}
	audit.Register(app.V1, e.svc)
	return app
}

func get(t *testing.T, app *httpx.App, persona, path string, q url.Values) (int, map[string]any) {
	t.Helper()
	return serve(t, app, persona, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path+"?"+q.Encode(), nil))
}

// search posts the criteria q to the POST …/search operation at path
// (BE-128: criteria travel in the body; BE-133 removed the GET lists).
func search(t *testing.T, app *httpx.App, persona, path string, q url.Values) (int, map[string]any) {
	t.Helper()
	criteria := make(map[string]string, len(q))
	for k := range q {
		criteria[k] = q.Get(k)
	}
	raw, err := json.Marshal(criteria)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path+"/search", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	return serve(t, app, persona, req)
}

func serve(t *testing.T, app *httpx.App, persona string, req *http.Request) (int, map[string]any) {
	t.Helper()
	if p, _ := authztest.Get(persona); p.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.Token)
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

func items(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, _ := body["items"].([]any)
	out := make([]map[string]any, len(raw))
	for i, x := range raw {
		out[i], _ = x.(map[string]any)
	}
	return out
}

// queryOrgAuditEvents returns ip and user_agent to dpo and platform_admin
// only (04 §2.21); club_admin sees the same events without them.
func TestListOrgAuditEvents_IPOnlyForDPOAndPAD(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	org := authztest.ID("club_a")
	ctx := httpx.WithUserAgent(httpx.WithClientIP(t.Context(), "203.0.113.9"), "Mozilla/5.0 Firefox/141.0")
	e.logN(t, ctx, 2, "events.event.create", authztest.ID("club_admin"), &org)
	app := newHTTP(t, e)
	q := url.Values{"occurred_from": {"2026-10-01"}, "occurred_to": {"2026-10-03"}}

	for persona, wantNetwork := range map[string]bool{"club_admin": false, "dpo": true, "platform_admin": true} {
		code, body := search(t, app, persona, "/api/v1/orgs/"+org.String()+"/audit-events", q)
		if code != http.StatusOK {
			t.Fatalf("%s: status %d (%v)", persona, code, body)
		}
		its := items(t, body)
		if len(its) != 2 {
			t.Fatalf("%s: %d items, want 2", persona, len(its))
		}
		for _, it := range its {
			ip, hasIP := it["ip"]
			ua, hasUA := it["user_agent"]
			if hasIP != wantNetwork || hasUA != wantNetwork {
				t.Fatalf("%s: ip/user_agent present = %v/%v, want %v (%v)", persona, hasIP, hasUA, wantNetwork, it)
			}
			if wantNetwork && (ip != "203.0.113.9" || ua != "Mozilla/5.0 Firefox/141.0") {
				t.Fatalf("%s: ip %v, user_agent %v", persona, ip, ua)
			}
			if _, has := it["metadata"]; has {
				t.Fatalf("%s: metadata belongs to adminQueryAuditEvents only", persona)
			}
			actor, _ := it["actor"].(map[string]any)
			if it["action"] != "events.event.create" || actor["user_id"] != authztest.ID("club_admin").String() {
				t.Fatalf("%s: item %v", persona, it)
			}
		}
	}
	// The other club's admin does not see the org at all; a member gets 403.
	if code, _ := search(t, app, "other_club_admin", "/api/v1/orgs/"+org.String()+"/audit-events", q); code != http.StatusNotFound {
		t.Fatalf("other_club_admin: %d, want 404", code)
	}
	if code, _ := search(t, app, "club_member", "/api/v1/orgs/"+org.String()+"/audit-events", q); code != http.StatusForbidden {
		t.Fatalf("club_member: %d, want 403", code)
	}
	if code, _ := search(t, app, "dpo", "/api/v1/orgs/"+org.String()+"/audit-events", url.Values{"occurred_from": {"x"}, "occurred_to": {"2026-10-03"}}); code != http.StatusUnprocessableEntity {
		t.Fatalf("bad occurred_from: %d, want 422", code)
	}
}

// The admin search is written to the log in the same transaction: who
// searched, with which filters (03 §6.5). A denied search writes nothing.
func TestAdminListAuditEvents_SearchIsAudited(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	ctx := t.Context()
	target := authztest.ID("student")
	e.logN(t, ctx, 3, "identity.session.create", target, nil)
	from, to := window()

	dpo := persona(t, "dpo")
	q := audit.AdminQuery{PageQuery: audit.PageQuery{Limit: 10}, From: from, To: to, ActorUserID: &target, BreakGlass: ptr(false)}
	p, err := e.svc.AdminList(ctx, dpo, q)
	if err != nil || len(p.Items) != 3 {
		t.Fatalf("AdminList() = %d items, %v; want the 3 events of the student", len(p.Items), err)
	}
	for _, v := range p.Items {
		if v.Metadata == nil {
			t.Fatal("admin search returns metadata")
		}
	}
	if _, err := e.svc.AdminList(ctx, persona(t, "support"), q); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("support: %v, want ErrForbidden", err)
	}

	searches, err := e.svc.AdminList(ctx, dpo, audit.AdminQuery{PageQuery: audit.PageQuery{Limit: 10}, From: from, To: to,
		Action: string(authz.AuditEventList)})
	if err != nil {
		t.Fatal(err)
	}
	// Two successful searches so far (the second one is listed by itself).
	if len(searches.Items) != 2 {
		t.Fatalf("%d logged searches, want 2 (the denied one writes nothing)", len(searches.Items))
	}
	first := searches.Items[1]
	var meta map[string]any
	if err := json.Unmarshal(first.Metadata, &meta); err != nil {
		t.Fatal(err)
	}
	if *first.ActorUserID != dpo.UserID || first.Outcome != audit.Success ||
		meta["filter_actor_user_id"] != target.String() || meta["filter_break_glass"] != false || meta["occurred_from"] == nil {
		t.Fatalf("logged search = %+v, metadata %v", first, meta)
	}
}

// listMySecurityEvents: the caller's identity and consent events with a
// coarse network view, plus break-glass accesses to the caller's data
// without the staff member's network facts.
func TestListMySecurityEvents_OwnSecurityEventsOnly(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	me, other := persona(t, "student"), persona(t, "student_b")
	ctx := httpx.WithUserAgent(httpx.WithClientIP(t.Context(), "198.51.100.77"), "Mozilla/5.0 (Macintosh) Version/19 Safari/605.1.15")
	support := persona(t, "support")
	for _, ev := range []audit.Event{
		audit.Event{Action: "identity.session.create", Outcome: audit.Success}.By(me),
		audit.Event{Action: "consent.record.create", Outcome: audit.Success}.By(me),
		audit.Event{Action: "events.registration.create", Outcome: audit.Success}.By(me), // not a security event
		audit.Event{Action: "identity.session.create", Outcome: audit.Success}.By(other), // someone else
		audit.Event{Action: "students.profile.read_admin", Outcome: audit.Success, Reason: "ticket SUP-1 investigation",
			Metadata: map[string]any{audit.MetaBreakGlass: true, audit.MetaSubjectUserID: me.UserID.String()}}.By(support),
		audit.Event{Action: "students.profile.read_admin", Outcome: audit.Success, Reason: "ticket SUP-2 investigation",
			Metadata: map[string]any{audit.MetaBreakGlass: true, audit.MetaSubjectUserID: other.UserID.String()}}.By(support),
	} {
		if err := e.svc.Log(ctx, ev); err != nil {
			t.Fatal(err)
		}
		e.clk.Advance(time.Second)
	}
	p, err := e.svc.ListOwn(t.Context(), me, audit.OwnQuery{PageQuery: audit.PageQuery{Limit: 10}})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]audit.SecurityEvent{}
	for _, it := range p.Items {
		got[it.Action] = it
	}
	if len(p.Items) != 3 || got["identity.session.create"].IPPrefix != "198.51.100.0/24" ||
		got["identity.session.create"].UserAgentFamily != "Safari" || got["consent.record.create"].IPPrefix == "" {
		t.Fatalf("own events = %+v", p.Items)
	}
	if bg := got["students.profile.read_admin"]; bg.IPPrefix != "" || bg.UserAgentFamily != "" {
		t.Fatalf("break-glass access shows the staff network: %+v", bg)
	}
}

// queryOrgAuditEvents: sensitive club records go to that club's club_admin
// only; talent.* events show only that they happened (03 §5.21).
func TestListOrgAuditEvents_SensitiveAndTalentRules(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	ctx := t.Context()
	org := authztest.ID("club_sensitive")
	adminID := authztest.ID("club_admin_sensitive")
	for _, ev := range []audit.Event{
		{Action: "orgs.member.list", Outcome: audit.Success, ActorUserID: &adminID, OrgID: &org,
			Metadata: map[string]any{audit.MetaSensitive: true}},
		{Action: "talent.subject.read", Outcome: audit.Success, ActorUserID: ptr(authztest.ID("talent_auditor")), OrgID: &org,
			ResourceType: "talent_subject", ResourceID: ptr(uuid.NewV7()), Reason: "quarterly audit sample",
			Metadata: map[string]any{"score_band": "b"}},
	} {
		if err := e.svc.Log(httpx.WithClientIP(ctx, "192.0.2.1"), ev); err != nil {
			t.Fatal(err)
		}
	}
	from, to := window()
	q := audit.OrgQuery{PageQuery: audit.PageQuery{Limit: 10}, From: from, To: to}

	clubAdmin, err := e.svc.ListOrg(ctx, persona(t, "club_admin_sensitive"), org, q)
	if err != nil || len(clubAdmin.Items) != 2 {
		t.Fatalf("club_admin of the club: %d items, %v; want both", len(clubAdmin.Items), err)
	}
	dpo, err := e.svc.ListOrg(ctx, persona(t, "dpo"), org, q)
	if err != nil || len(dpo.Items) != 1 || dpo.Items[0].Action != "talent.subject.read" {
		t.Fatalf("dpo: %+v, %v; want only the talent event", dpo.Items, err)
	}
	if v := dpo.Items[0]; v.ResourceType != "" || v.ResourceID != nil || v.IP != "" || v.Metadata != nil ||
		v.Reason != "quarterly audit sample" || v.ActorUserID == nil {
		t.Fatalf("talent event shows more than its existence: %+v", v)
	}
}

// Lists page with an opaque keyset cursor: 30 events → 25 + 5.
func TestLists_KeysetPages(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	org := authztest.ID("club_a")
	e.logN(t, t.Context(), 30, "events.event.update", authztest.ID("club_admin"), &org)
	app := newHTTP(t, e)
	q := url.Values{"occurred_from": {"2026-10-03"}, "occurred_to": {"2026-10-03"}}
	seen := map[string]bool{}
	pages := 0
	for {
		code, body := search(t, app, "club_admin", "/api/v1/orgs/"+org.String()+"/audit-events", q)
		if code != http.StatusOK {
			t.Fatalf("status %d: %v", code, body)
		}
		pages++
		for _, it := range items(t, body) {
			key, _ := it["occurred_at"].(string)
			if seen[key] {
				t.Fatalf("event at %s returned twice", key)
			}
			seen[key] = true
		}
		next, _ := body["next_cursor"].(string)
		if next == "" {
			break
		}
		q.Set("cursor", next)
	}
	if pages != 2 || len(seen) != 30 {
		t.Fatalf("%d pages, %d events; want 2 pages of 30 events", pages, len(seen))
	}
	// A cursor of one filter set is no good for another.
	q.Set("occurred_from", "2026-10-02")
	if code, body := search(t, app, "club_admin", "/api/v1/orgs/"+org.String()+"/audit-events", q); code != http.StatusUnprocessableEntity || body["code"] != "page.invalid_cursor" {
		t.Fatalf("foreign cursor: %d %v", code, body)
	}
}

func TestVerify_RangeAndAuthz(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	ctx := t.Context()
	pad := persona(t, "platform_admin")
	for name, r := range map[string][2]int{
		"reversed":     {0, -1},
		"future":       {0, 1},
		"over 31 days": {-31, 0},
	} {
		_, err := e.svc.Verify(ctx, pad, day0.AddDays(r[0]), day0.AddDays(r[1]))
		if !errors.Is(err, audit.ErrInvalidRange) {
			t.Errorf("%s: %v, want ErrInvalidRange", name, err)
		}
	}
	if _, err := e.svc.Verify(ctx, persona(t, "finance_admin"), day0, day0); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("finance_admin: %v", err)
	}
	res, err := e.svc.Verify(ctx, pad, day0.AddDays(-30), day0)
	if err != nil || !res.OK {
		t.Fatalf("Verify() = %+v, %v", res, err)
	}
	e.chain(t)
	rows := e.chained(t)
	if len(rows) != 1 || rows[0].Action != "audit.chain.verify" || *rows[0].ActorUserID != pad.UserID {
		t.Fatalf("the verification is not logged: %+v", rows)
	}
}

// BE-091 (T-326): over HTTP the break-glass item of listMySecurityEvents
// carries break_glass and the justification as reason; no network facts,
// no ticket_ref or other metadata.
func TestListMySecurityEvents_HTTPBreakGlassReason(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	me, support := persona(t, "student"), persona(t, "support")
	staff := httpx.WithUserAgent(httpx.WithClientIP(t.Context(), "203.0.113.9"), "curl/8.9.1")
	for _, ev := range []audit.Event{
		audit.Event{Action: "identity.session.create", Outcome: audit.Success}.By(me),
		audit.Event{Action: "students.profile.read_admin", Outcome: audit.Success, Reason: "Rıza kaydı itirazı incelendi",
			Metadata: map[string]any{audit.MetaBreakGlass: true, audit.MetaSubjectUserID: me.UserID.String(),
				audit.MetaTicketRef: "SUP-42"}}.By(support),
	} {
		if err := e.svc.Log(staff, ev); err != nil {
			t.Fatal(err)
		}
		e.clk.Advance(time.Second)
	}
	code, body := get(t, newHTTP(t, e), "student", "/api/v1/me/security-events", nil)
	if code != http.StatusOK {
		t.Fatalf("status %d: %v", code, body)
	}
	got := items(t, body)
	if len(got) != 2 {
		t.Fatalf("items = %v", got)
	}
	bg, own := got[0], got[1]
	want := map[string]any{"action": "students.profile.read_admin", "outcome": "success", "break_glass": true,
		"reason": "Rıza kaydı itirazı incelendi", "occurred_at": bg["occurred_at"]}
	if len(bg) != len(want) {
		t.Fatalf("break-glass item = %v, want exactly the keys of %v", bg, want)
	}
	for k, v := range want {
		if bg[k] != v {
			t.Fatalf("break-glass item %s = %v, want %v (item %v)", k, bg[k], v, bg)
		}
	}
	if _, has := own["break_glass"]; has || own["ip_prefix"] != "203.0.113.0/24" {
		t.Fatalf("own item = %v, want no break_glass and the caller's network", own)
	}
}
