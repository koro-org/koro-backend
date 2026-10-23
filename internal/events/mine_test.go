package events_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/events"
)

// fakeFollows is feed's follow list per user (BE-095); calls counts the
// reads, err fails them.
type fakeFollows struct {
	by    map[uuid.UUID][]uuid.UUID
	err   error
	asked []uuid.UUID
}

func (f *fakeFollows) FollowedClubIDs(_ context.Context, a authz.Actor, limit int) ([]uuid.UUID, error) {
	f.asked = append(f.asked, a.UserID)
	if f.err != nil {
		return nil, f.err
	}
	ids := f.by[a.UserID]
	return ids[:min(limit, len(ids))], nil
}

// mineWorld are the events of the BE-095 tests (all upcoming unless said
// otherwise; the matrix clock is t0, the events start at start).
type mineWorld struct {
	aPublic, aMembers, aUni, aDraft, aEnded, aCancelled, bPublic, sensPublic, sensMembers, platform events.Event
}

func putMine(t *testing.T, e *env) mineWorld {
	t.Helper()
	by := persona(t, "club_admin").UserID
	put := func(org, title string, at time.Time, mod func(*events.Event)) events.Event {
		t.Helper()
		ev := row(orgID(org), by, title, at)
		if mod != nil {
			mod(&ev)
		}
		if err := e.mem.PutEvent(ev); err != nil {
			t.Fatalf("PutEvent(%s): %v", title, err)
		}
		return ev
	}
	members := func(ev *events.Event) { ev.Visibility = events.VisibilityMembers }
	uniA := authzUniversity("uni_a")
	w := mineWorld{
		aPublic:  put("club_a", "A Açık", start, nil),
		aMembers: put("club_a", "A Üyelere", start.Add(time.Hour), members),
		aUni: put("club_a", "A Üniversiteye", start.Add(2*time.Hour), func(ev *events.Event) {
			ev.Visibility, ev.VisibilityUniversityID = events.VisibilityUniversity, &uniA
		}),
		aDraft: put("club_a", "A Taslak", start, func(ev *events.Event) { ev.Status, ev.PublishedAt = events.StatusDraft, nil }),
		aEnded: put("club_a", "A Bitmiş", t0.Add(-2*time.Hour), nil),
		aCancelled: put("club_a", "A İptal", start, func(ev *events.Event) {
			ev.Status, ev.CancelledAt = events.StatusCancelled, new(t0)
		}),
		bPublic:     put("club_b", "B Açık", start.Add(3*time.Hour), nil),
		sensPublic:  put("club_sensitive", "Hassas Açık", start.Add(4*time.Hour), nil),
		sensMembers: put("club_sensitive", "Hassas Üyelere", start.Add(5*time.Hour), members),
		platform:    put("platform_org", "Platform Etkinliği", start.Add(6*time.Hour), nil),
	}
	staff := func(ev events.Event, who string, role events.StaffRole) {
		t.Helper()
		u, _ := authztest.Get(who)
		if _, err := e.mem.InsertStaff(t.Context(), events.Staff{EventID: ev.ID, UserID: u.User(), OrgID: ev.OrgID, Role: role, InvitedBy: by}); err != nil {
			t.Fatal(err)
		}
	}
	// 03 §7.3: event_staff scans event_a, event_manager_a manages it; here
	// event_staff also scans club_b's event and an event that has ended,
	// and nobody's staff on the sensitive club's members event.
	staff(w.aPublic, "event_staff", events.StaffScanner)
	staff(w.aPublic, "event_manager_a", events.StaffManager)
	staff(w.bPublic, "event_staff", events.StaffScanner)
	staff(w.aEnded, "event_staff", events.StaffScanner)
	staff(w.aDraft, "event_staff", events.StaffScanner)
	staff(w.aCancelled, "event_staff", events.StaffScanner)
	return w
}

func helper(t *testing.T, name string) authz.Actor {
	t.Helper()
	p, ok := authztest.Get(name)
	if !ok {
		t.Fatalf("unknown helper %s", name)
	}
	return p.Actor
}

func viewIDs(vs []events.EventView) []uuid.UUID {
	out := make([]uuid.UUID, len(vs))
	for i, v := range vs {
		out[i] = v.Event.ID
	}
	return out
}

// TestListMyStaffEvents_OnlyDoorsICanWork: the door list holds the
// upcoming published events the caller may scan for (staff, the
// organiser's managers, platform_admin for the platform org); another
// user's assignment, a members event of a club the caller is only a member
// of, a sensitive club's event and drafts, cancelled or ended events never
// appear.
func TestListMyStaffEvents_OnlyDoorsICanWork(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	w := putMine(t, e)
	list := func(a authz.Actor, q events.MyEventsQuery) events.StaffEventPage {
		t.Helper()
		p, err := e.svc.ListMyStaffEvents(ctx, a, q)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	ids := func(p events.StaffEventPage) []uuid.UUID {
		out := make([]uuid.UUID, len(p.Items))
		for i, v := range p.Items {
			out[i] = v.Event.ID
		}
		return out
	}
	// event_staff: its two scanner doors, nothing of club_a's other events.
	p := list(persona(t, "event_staff"), events.MyEventsQuery{})
	if got := ids(p); !slices.Equal(got, []uuid.UUID{w.aPublic.ID, w.bPublic.ID}) || p.Next != nil {
		t.Fatalf("event_staff = %v (next %v)", got, p.Next)
	}
	for _, v := range p.Items {
		if v.StaffRole != events.StaffScanner || v.Manage || v.Org.ID != v.Event.OrgID {
			t.Fatalf("event_staff item = %+v", v)
		}
	}
	// The manager of event_a: one door, with void and finalize.
	p = list(helper(t, "event_manager_a"), events.MyEventsQuery{})
	if got := ids(p); !slices.Equal(got, []uuid.UUID{w.aPublic.ID}) || p.Items[0].StaffRole != events.StaffManager || !p.Items[0].Manage {
		t.Fatalf("event_manager_a = %+v", p.Items)
	}
	// club_admin: every upcoming published event of club_a (no staff role).
	p = list(persona(t, "club_admin"), events.MyEventsQuery{})
	if got := ids(p); !slices.Equal(got, []uuid.UUID{w.aPublic.ID, w.aMembers.ID, w.aUni.ID}) {
		t.Fatalf("club_admin = %v", got)
	}
	for _, v := range p.Items {
		if v.StaffRole != "" || !v.Manage {
			t.Fatalf("club_admin item = %+v", v)
		}
	}
	// Paging: limit 1 with the keyset of the last item.
	first := list(persona(t, "club_admin"), events.MyEventsQuery{Limit: 1})
	if got := ids(first); !slices.Equal(got, []uuid.UUID{w.aPublic.ID}) || first.Next == nil {
		t.Fatalf("first page = %v (next %v)", got, first.Next)
	}
	if got := ids(list(persona(t, "club_admin"), events.MyEventsQuery{Limit: 2, After: first.Next})); !slices.Equal(got, []uuid.UUID{w.aMembers.ID, w.aUni.ID}) {
		t.Fatalf("second page = %v", got)
	}
	// The sensitive club's admin: its own club's events only.
	if got := ids(list(helper(t, "club_admin_sensitive"), events.MyEventsQuery{})); !slices.Equal(got, []uuid.UUID{w.sensPublic.ID, w.sensMembers.ID}) {
		t.Fatalf("club_admin_sensitive = %v", got)
	}
	// club_b's admin: club_b's door only (event_staff also scans it).
	if got := ids(list(persona(t, "other_club_admin"), events.MyEventsQuery{})); !slices.Equal(got, []uuid.UUID{w.bPublic.ID}) {
		t.Fatalf("other_club_admin = %v", got)
	}
	// Members, students, the university's rep (k14: not
	// a club's events), companies and platform roles outside the platform
	// org work no door.
	for _, n := range []string{"club_member", "student", "student_minor", "user_unverified",
		"university_rep", "company_admin", "moderator", "support", "dpo", "platform_admin"} {
		if got := ids(list(persona(t, n), events.MyEventsQuery{})); len(got) != 0 {
			t.Errorf("%s works doors %v", n, got)
		}
	}
	// k-po: platform_admin of the platform org works its events' doors.
	pad := persona(t, "platform_admin")
	pad.Memberships = append(slices.Clone(pad.Memberships), authz.Membership{OrgID: orgID("platform_org"),
		OrgKind: authz.OrgPlatform, Role: authz.MemberStaff})
	if got := ids(list(pad, events.MyEventsQuery{})); !slices.Equal(got, []uuid.UUID{w.platform.ID}) {
		t.Fatalf("platform_admin of the platform org = %v", got)
	}
	// The anonymous actor has no list.
	if _, err := e.svc.ListMyStaffEvents(ctx, authz.Anonymous(), events.MyEventsQuery{}); err == nil {
		t.Fatal("anonymous listed doors")
	}
}

// TestListMyClubEvents_MembershipsAndFollows: the clubs are the caller's
// member clubs and followed clubs; the events are exactly what listEvents
// shows the caller for those clubs (03 §3.6): a followed sensitive club's
// members event never leaks, another user's follows are never read.
func TestListMyClubEvents_MembershipsAndFollows(t *testing.T) {
	t.Parallel()
	follows := &fakeFollows{by: map[uuid.UUID][]uuid.UUID{}}
	e := newEnv(t, func(d *events.Deps) { d.Follows = follows })
	ctx := t.Context()
	w := putMine(t, e)
	student := persona(t, "student")
	follows.by[student.UserID] = []uuid.UUID{orgID("club_a"), orgID("club_sensitive")}
	list := func(a authz.Actor, q events.MyEventsQuery) events.EventPage {
		t.Helper()
		p, err := e.svc.ListMyClubEvents(ctx, a, q)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, tc := range []struct {
		name  string
		actor authz.Actor
		clubs []string
		want  []uuid.UUID
	}{
		// Member of club_a and club_sensitive: their members events too.
		{"club_member", persona(t, "club_member"), []string{"club_a", "club_sensitive"},
			[]uuid.UUID{w.aPublic.ID, w.aMembers.ID, w.aUni.ID, w.sensPublic.ID, w.sensMembers.ID}},
		// Follower of club_a and the sensitive club, verified student of
		// uni_a: public and university events only; no members event.
		{"student (follows)", student, []string{"club_a", "club_sensitive"},
			[]uuid.UUID{w.aPublic.ID, w.aUni.ID, w.sensPublic.ID}},
		// club_b's admin: its own club only.
		{"other_club_admin", persona(t, "other_club_admin"), []string{"club_b"}, []uuid.UUID{w.bPublic.ID}},
		// Neither member nor follower: nothing (event_staff scans club_b's
		// event, but follows no club).
		{"event_staff", persona(t, "event_staff"), nil, []uuid.UUID{}},
		{"university_rep (no club)", persona(t, "university_rep"), nil, []uuid.UUID{}},
		{"platform_admin (no club)", persona(t, "platform_admin"), nil, []uuid.UUID{}},
	} {
		got := viewIDs(list(tc.actor, events.MyEventsQuery{}).Items)
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s = %v, want %v", tc.name, got, tc.want)
			continue
		}
		if len(tc.clubs) == 0 {
			continue
		}
		// Exactly listEvents for the same clubs (the same viewer filter).
		ids := make([]uuid.UUID, len(tc.clubs))
		for i, c := range tc.clubs {
			ids[i] = orgID(c)
		}
		ref, err := e.svc.ListEvents(ctx, tc.actor, events.ListQuery{OrgIDs: ids})
		if err != nil || !slices.Equal(viewIDs(ref.Items), tc.want) {
			t.Errorf("%s: listEvents = %v, %v; want %v", tc.name, viewIDs(ref.Items), err, tc.want)
		}
	}
	// Only the caller's own follows were read.
	for _, u := range follows.asked {
		if u == uuid.Nil() {
			t.Fatal("follows read for the nil user")
		}
	}
	// Paging by starts_at with a keyset.
	cm := persona(t, "club_member")
	first := list(cm, events.MyEventsQuery{Limit: 2})
	if got := viewIDs(first.Items); !slices.Equal(got, []uuid.UUID{w.aPublic.ID, w.aMembers.ID}) || first.Next == nil {
		t.Fatalf("first page = %v (next %v)", got, first.Next)
	}
	if got := viewIDs(list(cm, events.MyEventsQuery{Limit: 10, After: first.Next}).Items); !slices.Equal(got,
		[]uuid.UUID{w.aUni.ID, w.sensPublic.ID, w.sensMembers.ID}) {
		t.Fatalf("second page = %v", got)
	}
	// A feed failure is the request's failure, not a silent partial list.
	follows.err = errors.New("feed down")
	if _, err := e.svc.ListMyClubEvents(ctx, student, events.MyEventsQuery{}); err == nil {
		t.Fatal("feed error swallowed")
	}
	// The anonymous actor has no list.
	if _, err := e.svc.ListMyClubEvents(ctx, authz.Anonymous(), events.MyEventsQuery{}); err == nil {
		t.Fatal("anonymous listed club events")
	}
}

// TestListMyClubEvents_WithoutFeed: without the follows port only the
// member clubs are listed.
func TestListMyClubEvents_WithoutFeed(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	w := putMine(t, e)
	p, err := e.svc.ListMyClubEvents(t.Context(), persona(t, "club_admin"), events.MyEventsQuery{})
	if err != nil || !slices.Equal(viewIDs(p.Items), []uuid.UUID{w.aPublic.ID, w.aMembers.ID, w.aUni.ID}) {
		t.Fatalf("club_admin without feed = %v, %v", viewIDs(p.Items), err)
	}
	p, err = e.svc.ListMyClubEvents(t.Context(), persona(t, "student"), events.MyEventsQuery{})
	if err != nil || len(p.Items) != 0 {
		t.Fatalf("student without feed = %v, %v", viewIDs(p.Items), err)
	}
}

// TestHTTP_MyEventLists: the two /me lists over HTTP (04 §2.6): 401
// without a session, the card DTO plus the door fields, a working
// next_cursor and 422 page.invalid_cursor for a foreign cursor.
func TestHTTP_MyEventLists(t *testing.T) {
	t.Parallel()
	follows := &fakeFollows{by: map[uuid.UUID][]uuid.UUID{}}
	e := newEnv(t, func(d *events.Deps) { d.Follows = follows })
	w := putMine(t, e)
	app := newHTTP(t, e)
	for _, path := range []string{"/api/v1/me/staff-events", "/api/v1/me/club-events"} {
		if rec, _ := call(t, app, "anonymous", http.MethodGet, path, "", nil); rec.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous %s = %d", path, rec.Code)
		}
		if rec, out := call(t, app, "club_admin", http.MethodGet, path+"?cursor=bozuk", "", nil); rec.Code != 422 || out["code"] != "page.invalid_cursor" {
			t.Fatalf("bad cursor %s = %d %v", path, rec.Code, out)
		}
	}
	rec, out := call(t, app, "event_staff", http.MethodGet, "/api/v1/me/staff-events", "", nil)
	items, _ := out["items"].([]any)
	if rec.Code != http.StatusOK || len(items) != 2 {
		t.Fatalf("staff events = %d %s", rec.Code, rec.Body)
	}
	first := items[0].(map[string]any)
	if first["event_id"] != w.aPublic.ID.String() || first["staff_role"] != "scanner" || first["door_access"] != "scanner" ||
		first["checkin_enabled"] != false || first["org"].(map[string]any)["kind"] != "club" {
		t.Fatalf("staff event item = %v", first)
	}
	if _, ok := first["online_url"]; ok {
		t.Fatalf("door list carries online_url: %v", first)
	}
	// club_admin manages the door without a staff role; pages of one.
	rec, out = call(t, app, "club_admin", http.MethodGet, "/api/v1/me/staff-events?limit=1", "", nil)
	items, _ = out["items"].([]any)
	cur, _ := out["next_cursor"].(string)
	if rec.Code != http.StatusOK || len(items) != 1 || cur == "" {
		t.Fatalf("club_admin first page = %d %s", rec.Code, rec.Body)
	}
	if it := items[0].(map[string]any); it["door_access"] != "manager" || it["staff_role"] != nil {
		t.Fatalf("club_admin item = %v", it)
	}
	rec, out = call(t, app, "club_admin", http.MethodGet, "/api/v1/me/staff-events?limit=5&cursor="+cur, "", nil)
	if items, _ = out["items"].([]any); rec.Code != http.StatusOK || len(items) != 2 || out["next_cursor"] != nil {
		t.Fatalf("club_admin second page = %d %s", rec.Code, rec.Body)
	}
	// A staff-events cursor is not a club-events cursor.
	if rec, out := call(t, app, "club_admin", http.MethodGet, "/api/v1/me/club-events?cursor="+cur, "", nil); rec.Code != 422 || out["code"] != "page.invalid_cursor" {
		t.Fatalf("foreign cursor = %d %v", rec.Code, out)
	}
	// The follower of the sensitive club: never its members event.
	follows.by[persona(t, "student").UserID] = []uuid.UUID{orgID("club_sensitive")}
	rec, out = call(t, app, "student", http.MethodGet, "/api/v1/me/club-events", "", nil)
	items, _ = out["items"].([]any)
	if rec.Code != http.StatusOK || len(items) != 1 || items[0].(map[string]any)["event_id"] != w.sensPublic.ID.String() {
		t.Fatalf("student club events = %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), w.sensMembers.ID.String()) {
		t.Fatal("a followed sensitive club's members event leaked")
	}
}
