package events_test

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/events"
	"koro/internal/orgs"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/problem/problemtest"
)

func TestCreateEvent_CreatesDraftForm(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	v := e.create(t, "club_admin", "club_a", input("Go Atölyesi"))
	ev := v.Event
	if ev.Status != events.StatusDraft || ev.OrgID != orgID("club_a") || ev.Slug != "go-atolyesi" ||
		ev.CreatedBy != persona(t, "club_admin").UserID || !ev.RequiresRegistration || ev.Visibility != events.VisibilityPublic ||
		ev.RegistrationMode != events.ModeAuto || ev.PublishedAt != nil || !v.Manager || v.ETag == "" {
		t.Fatalf("created = %+v", v)
	}
	f, ok := e.mem.Form(ev.ID)
	if !ok || f.OrgID != ev.OrgID || f.EventID != ev.ID || f.CurrentVersionID != nil ||
		string(f.DraftSchema) != `{"schema_version":1,"fields":[]}` {
		t.Fatalf("form draft = %+v, %v", f, ok)
	}
	if got := e.audit.Of(authz.EventsEventCreate); len(got) != 1 || *got[0].ResourceID != ev.ID || *got[0].OrgID != ev.OrgID {
		t.Fatalf("audit = %+v", got)
	}
	// The same title again takes the next free slug; a requested slug that is
	// taken is 409.
	if again := e.create(t, "club_admin", "club_a", input("Go Atölyesi")); again.Event.Slug != "go-atolyesi-2" {
		t.Fatalf("second slug = %q", again.Event.Slug)
	}
	in := input("Başka Bir Atölye")
	in.Slug = "go-atolyesi"
	_, err := e.svc.CreateEvent(ctx, persona(t, "club_admin"), orgID("club_a"), in)
	problemtest.WantProblem(t, err, 409, "events.slug_taken")
	// university_rep organises for its university (MFA session), not for a club of it.
	if u := e.create(t, "university_rep", "uni_a", input("Kariyer Günü")); u.Event.OrgID != orgID("uni_a") {
		t.Fatalf("university event org = %s", u.Event.OrgID)
	}
	_, err = e.svc.CreateEvent(ctx, persona(t, "university_rep"), orgID("club_a"), input("Kulüp Adına"))
	wantNotFound(t, err) // not a member of club_a: its management surface is invisible
	// A club member has no create grant (403); an outsider sees no surface (404).
	_, err = e.svc.CreateEvent(ctx, persona(t, "club_member"), orgID("club_a"), input("Üye Etkinliği"))
	wantForbidden(t, err)
	_, err = e.svc.CreateEvent(ctx, persona(t, "other_club_admin"), orgID("club_a"), input("Yabancı Etkinlik"))
	wantNotFound(t, err)
	// Body rules: dates and the university of a university-only event.
	bad := input("Ters Tarihli")
	bad.EndsAt = bad.StartsAt
	_, err = e.svc.CreateEvent(ctx, persona(t, "club_admin"), orgID("club_a"), bad)
	problemtest.WantProblem(t, err, 422, "validation_failed")
	uniOnly := input("Yalnız Üniversite")
	uniOnly.Visibility = events.VisibilityUniversity
	other := authzUniversity("uni_b")
	uniOnly.VisibilityUniversityID = &other
	_, err = e.svc.CreateEvent(ctx, persona(t, "club_admin"), orgID("club_a"), uniOnly)
	problemtest.WantProblem(t, err, 422, "validation_failed") // club_a belongs to uni_a
	own := authzUniversity("uni_a")
	uniOnly.VisibilityUniversityID = &own
	if v := e.create(t, "club_admin", "club_a", uniOnly); *v.Event.VisibilityUniversityID != own {
		t.Fatalf("university visibility = %+v", v.Event)
	}
	// A suspended org's admin cannot create (03 §2.3).
	e.orgs.setStatus(orgID("club_a"), orgs.StatusSuspended)
	_, err = e.svc.CreateEvent(ctx, persona(t, "club_admin"), orgID("club_a"), input("Askıda"))
	problemtest.WantProblem(t, err, 403, "orgs.org_suspended")
}

func TestCreateEvent_CompanyOrg_403(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	// T-17 / k13: no company role creates events in v1.
	for _, p := range []string{"company_admin", "company_member", "platform_admin"} {
		_, err := e.svc.CreateEvent(ctx, persona(t, p), orgID("company_a"), input("Şirket Etkinliği"))
		wantForbidden(t, err)
	}
	// Another company's admin does not see company_a's surface.
	_, err := e.svc.CreateEvent(ctx, persona(t, "other_company_member"), orgID("company_a"), input("Şirket Etkinliği"))
	wantNotFound(t, err)
	if n := len(e.audit.Of(authz.EventsEventCreate)); n != 0 {
		t.Fatalf("%d audit records of refused creations", n)
	}
}

func TestPublishEvent_RequiresRegistrationWithoutForm_422(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	admin := persona(t, "club_admin")
	v := e.create(t, "club_admin", "club_a", input("Formsuz Etkinlik"))
	_, err := e.svc.PublishEvent(ctx, admin, v.Event.ID)
	problemtest.WantProblem(t, err, 422, "events.form_required")
	if got, _ := e.mem.GetEvent(ctx, v.Event.ID); got.Status != events.StatusDraft {
		t.Fatalf("status after the refused publication = %s", got.Status)
	}
	// With a published form version (BE-026) the draft is published.
	if err := e.mem.SetFormVersion(v.Event.ID, uuid.NewV7()); err != nil {
		t.Fatal(err)
	}
	p, err := e.svc.PublishEvent(ctx, admin, v.Event.ID)
	if err != nil || p.Event.Status != events.StatusPublished || p.Event.PublishedAt == nil {
		t.Fatalf("publish = %+v, %v", p.Event, err)
	}
	_, err = e.svc.PublishEvent(ctx, admin, v.Event.ID)
	problemtest.WantProblem(t, err, 409, "events.invalid_state")
	// Without registration no form is needed; an online event needs its link.
	in := input("Kayıtsız Çevrim İçi")
	in.RequiresRegistration, in.Format, in.City, in.LocationName = new(false), events.FormatOnline, "", ""
	o := e.create(t, "club_admin", "club_a", in)
	_, err = e.svc.PublishEvent(ctx, admin, o.Event.ID)
	problemtest.WantProblem(t, err, 422, "validation_failed")
	_, err = e.svc.UpdateEvent(ctx, admin, o.Event.ID, o.ETag, events.EventPatch{OnlineURL: new("https://meet.example.test/oda")})
	if err != nil {
		t.Fatal(err)
	}
	if p, err := e.svc.PublishEvent(ctx, admin, o.Event.ID); err != nil || p.Event.Status != events.StatusPublished {
		t.Fatalf("publish without registration = %+v, %v", p.Event, err)
	}
	// A past event cannot be published.
	old := e.create(t, "club_admin", "club_a", input("Geçmiş Etkinlik"))
	_ = e.mem.SetFormVersion(old.Event.ID, uuid.NewV7())
	e.clk.Set(start.Add(3 * time.Hour))
	_, err = e.svc.PublishEvent(ctx, admin, old.Event.ID)
	problemtest.WantProblem(t, err, 422, "validation_failed")
}

func TestUpdateEvent_StaleETag_412(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	admin := persona(t, "club_admin")
	v := e.create(t, "club_admin", "club_a", input("Sürüm Etkinliği"))
	title := new("Yeni Başlık")
	_, err := e.svc.UpdateEvent(ctx, admin, v.Event.ID, "", events.EventPatch{Title: title})
	problemtest.WantProblem(t, err, 428, "precondition_required")
	_, err = e.svc.UpdateEvent(ctx, admin, v.Event.ID, `"stale"`, events.EventPatch{Title: title})
	problemtest.WantProblem(t, err, 412, "precondition_failed")
	u, err := e.svc.UpdateEvent(ctx, admin, v.Event.ID, v.ETag, events.EventPatch{Title: title})
	if err != nil || u.Event.Title != "Yeni Başlık" || u.ETag == v.ETag {
		t.Fatalf("update = %+v (etag %s → %s), %v", u.Event, v.ETag, u.ETag, err)
	}
	// The first ETag is stale now.
	_, err = e.svc.UpdateEvent(ctx, admin, v.Event.ID, v.ETag, events.EventPatch{Title: new("Üçüncü")})
	problemtest.WantProblem(t, err, 412, "precondition_failed")
	// An empty patch, a field both set and cleared.
	_, err = e.svc.UpdateEvent(ctx, admin, v.Event.ID, u.ETag, events.EventPatch{})
	problemtest.WantProblem(t, err, 422, "events.empty_patch")
	_, err = e.svc.UpdateEvent(ctx, admin, v.Event.ID, u.ETag, events.EventPatch{City: new("Ankara"), Clear: []string{"city"}})
	problemtest.WantProblem(t, err, 422, "validation_failed")
	// Clearing works; audit names the fields, never values.
	c, err := e.svc.UpdateEvent(ctx, admin, v.Event.ID, u.ETag, events.EventPatch{Clear: []string{"city"}})
	if err != nil || c.Event.City != nil {
		t.Fatalf("clear city = %+v, %v", c.Event, err)
	}
	if got := e.audit.Of(authz.EventsEventUpdate); len(got) != 2 || !slices.Equal(got[1].Metadata["fields"].([]string), []string{"city"}) {
		t.Fatalf("audit = %+v", got)
	}
	// Another club's admin does not see the draft (06 H-38: 404, nothing written).
	_, err = e.svc.UpdateEvent(ctx, persona(t, "other_club_admin"), v.Event.ID, c.ETag, events.EventPatch{Title: title})
	wantNotFound(t, err)
}

func TestUpdateEvent_ScheduleChange_EmitsEvent(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	admin := persona(t, "club_admin")
	// A draft's schedule change emits nothing.
	d := e.create(t, "club_admin", "club_a", input("Taslak Program"))
	if _, err := e.svc.UpdateEvent(ctx, admin, d.Event.ID, d.ETag, events.EventPatch{StartsAt: new(start.Add(time.Hour))}); err != nil {
		t.Fatal(err)
	}
	if topics := e.out.Topics(); len(topics) != 0 {
		t.Fatalf("a draft's edit emitted %v", topics)
	}
	p := e.published(t, input("Yayımlı Program"))
	// A title change of a published event emits no schedule change, only
	// event.updated for the web (BE-106).
	p, err := e.svc.UpdateEvent(ctx, admin, p.Event.ID, p.ETag, events.EventPatch{Title: new("Yeni Ad")})
	if err != nil {
		t.Fatal(err)
	}
	if topics := e.out.Topics(); !slices.Equal(topics, []string{domainevent.TopicEventPublished, domainevent.TopicEventUpdated}) {
		t.Fatalf("events before the schedule change: %v", topics)
	}
	before := len(e.out.Events())
	moved := start.Add(24 * time.Hour)
	if _, err = e.svc.UpdateEvent(ctx, admin, p.Event.ID, p.ETag, events.EventPatch{
		StartsAt: &moved, EndsAt: new(moved.Add(2 * time.Hour)), Summary: new("Tarih değişti"),
	}); err != nil {
		t.Fatal(err)
	}
	evs := e.out.Events()[before:]
	if len(evs) != 2 || evs[0].Topic != domainevent.TopicEventScheduleChanged || evs[0].AggregateID != p.Event.ID ||
		evs[1].Topic != domainevent.TopicEventUpdated {
		t.Fatalf("events = %+v", evs)
	}
	var payload domainevent.EventScheduleChanged
	if err := json.Unmarshal(evs[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.EventID != p.Event.ID || payload.OrgID != orgID("club_a") || !slices.Equal(payload.Fields, []string{"starts_at", "ends_at"}) {
		t.Fatalf("payload = %+v", payload)
	}
}

// TestPublishAndUpdate_EmitWebRevalidationEvents: publishing writes
// event.published and every edit of a published event event.updated, in
// the transaction of the write; a slug change carries the old slug so the
// web revalidates the old page too (BE-106).
func TestPublishAndUpdate_EmitWebRevalidationEvents(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	admin := persona(t, "club_admin")
	updatesBefore := len(e.store.updates)
	p := e.published(t, input("Web Etkinliği"))
	pub := e.eventsOf(domainevent.TopicEventPublished)
	if len(pub) != 1 || pub[0].AggregateID != p.Event.ID {
		t.Fatalf("event.published = %+v", pub)
	}
	got, err := domainevent.Decode[domainevent.EventPublished](pub[0])
	if err != nil || got.EventID != p.Event.ID || got.OrgID != orgID("club_a") {
		t.Fatalf("payload = %+v, %v", got, err)
	}
	if adds, updates := e.out.txs[domainevent.TopicEventPublished], e.store.updates[updatesBefore:]; len(adds) != 1 ||
		adds[0] == 0 || len(updates) != 1 || adds[0] != updates[0] {
		t.Fatalf("outbox tx %v, update tx %v", adds, updates)
	}
	oldSlug := p.Event.Slug
	if _, err := e.svc.UpdateEvent(ctx, admin, p.Event.ID, p.ETag, events.EventPatch{Slug: new("web-etkinligi-yeni")}); err != nil {
		t.Fatal(err)
	}
	upd := e.eventsOf(domainevent.TopicEventUpdated)
	if len(upd) != 1 {
		t.Fatalf("event.updated = %+v", upd)
	}
	u, err := domainevent.Decode[domainevent.EventUpdated](upd[0])
	if err != nil || u.EventID != p.Event.ID || u.OrgID != orgID("club_a") || u.PreviousSlug != oldSlug ||
		!slices.Equal(u.Fields, []string{"slug"}) {
		t.Fatalf("payload = %+v, %v (old slug %q)", u, err, oldSlug)
	}
	if s, err := e.svc.Get(ctx, p.Event.ID); err != nil || s.Slug != "web-etkinligi-yeni" {
		t.Fatalf("summary slug = %q, %v", s.Slug, err)
	}
}

func TestCancelEvent_EmitsEventInSameTx(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	admin := persona(t, "club_admin")
	d := e.create(t, "club_admin", "club_a", input("İptal Taslağı"))
	_, err := e.svc.CancelEvent(ctx, admin, d.Event.ID, "Hava koşulları")
	problemtest.WantProblem(t, err, 409, "events.invalid_state") // a draft is deleted, not cancelled
	p := e.published(t, input("İptal Edilecek"))
	_, err = e.svc.CancelEvent(ctx, admin, p.Event.ID, " ")
	problemtest.WantProblem(t, err, 422, "validation_failed")
	// A failing audit write fails the transaction: no event leaves (the
	// in-memory store has no rollback, so that event is not reused).
	failed := e.published(t, input("Başarısız İptal"))
	e.audit.Fail(errors.New("audit down"))
	if _, err := e.svc.CancelEvent(ctx, admin, failed.Event.ID, "Hava koşulları"); err == nil {
		t.Fatal("cancel with a failing audit succeeded")
	}
	if len(e.eventsOf(domainevent.TopicEventCancelled)) != 0 {
		t.Fatalf("events after the rollback: %v", e.out.Topics())
	}
	e.audit.Fail(nil)
	updatesBefore := len(e.store.updates)
	c, err := e.svc.CancelEvent(ctx, admin, p.Event.ID, "Hava koşulları")
	if err != nil || c.Event.Status != events.StatusCancelled || c.Event.CancelledAt == nil || *c.Event.CancelReason != "Hava koşulları" {
		t.Fatalf("cancel = %+v, %v", c.Event, err)
	}
	evs := e.eventsOf(domainevent.TopicEventCancelled)
	if len(evs) != 1 || evs[0].Topic != domainevent.TopicEventCancelled || evs[0].AggregateID != p.Event.ID {
		t.Fatalf("events = %+v", evs)
	}
	// The event is written to the outbox in the transaction of the status change.
	adds := e.out.txs[domainevent.TopicEventCancelled]
	updates := e.store.updates[updatesBefore:]
	if len(updates) != 1 || len(adds) != 2 || adds[1] == 0 || adds[1] != updates[0] {
		t.Fatalf("outbox tx %v, update tx %v", adds, updates)
	}
	_, err = e.svc.CancelEvent(ctx, admin, p.Event.ID, "Tekrar")
	problemtest.WantProblem(t, err, 409, "events.invalid_state")
	// A cancelled event takes no edit.
	_, err = e.svc.UpdateEvent(ctx, admin, p.Event.ID, c.ETag, events.EventPatch{Title: new("Sonradan")})
	problemtest.WantProblem(t, err, 409, "events.invalid_state")
}

func TestDeleteEvent_OnlyDraft(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	admin := persona(t, "club_admin")
	d := e.create(t, "club_admin", "club_a", input("Silinecek Taslak"))
	if _, err := e.svc.AddStaff(ctx, admin, d.Event.ID, "student@example.test", events.StaffScanner); err != nil {
		t.Fatal(err)
	}
	// Another org's admin: 404 (the draft is invisible to it, 06 H-38).
	wantNotFound(t, e.svc.DeleteEventDraft(ctx, persona(t, "other_club_admin"), d.Event.ID))
	wantForbidden(t, e.svc.DeleteEventDraft(ctx, persona(t, "platform_admin"), d.Event.ID)) // k-po
	if err := e.svc.DeleteEventDraft(ctx, admin, d.Event.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.mem.GetEvent(ctx, d.Event.ID); !errors.Is(err, events.ErrNotFound) {
		t.Fatalf("deleted draft: %v", err)
	}
	if _, ok := e.mem.Form(d.Event.ID); ok {
		t.Fatal("the form draft survived its event")
	}
	if rows, _ := e.mem.StaffOfUser(ctx, persona(t, "student").UserID); len(rows) != 0 {
		t.Fatalf("staff rows survived their event: %+v", rows)
	}
	wantNotFound(t, e.svc.DeleteEventDraft(ctx, admin, d.Event.ID))
	// A published or cancelled event is never deleted.
	p := e.published(t, input("Yayımlı"))
	problemtest.WantProblem(t, e.svc.DeleteEventDraft(ctx, admin, p.Event.ID), 409, "events.invalid_state")
	if _, err := e.svc.CancelEvent(ctx, admin, p.Event.ID, "Vazgeçildi"); err != nil {
		t.Fatal(err)
	}
	problemtest.WantProblem(t, e.svc.DeleteEventDraft(ctx, admin, p.Event.ID), 409, "events.invalid_state")
	if got := e.audit.Of(authz.EventsEventDelete); len(got) != 1 {
		t.Fatalf("delete audit = %+v", got)
	}
}

func TestListEvents_AnonymousOnlyPublic(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	public := e.published(t, input("Herkese Açık Yapay Zekâ Söyleşisi"))
	uni := input("Üniversite Etkinliği")
	uni.Visibility, uni.VisibilityUniversityID = events.VisibilityUniversity, new(authzUniversity("uni_a"))
	uniEv := e.published(t, uni)
	mem := input("Üyelere Özel")
	mem.Visibility = events.VisibilityMembers
	memEv := e.published(t, mem)
	e.create(t, "club_admin", "club_a", input("Taslak Etkinlik"))
	staffOnly := e.published(t, mem)
	if _, err := e.svc.AddStaff(ctx, persona(t, "club_admin"), staffOnly.Event.ID, "event_staff@example.test", events.StaffScanner); err != nil {
		t.Fatal(err)
	}
	titles := func(actor authz.Actor, q events.ListQuery) []uuid.UUID {
		t.Helper()
		p, err := e.svc.ListEvents(ctx, actor, q)
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]uuid.UUID, len(p.Items))
		for i, v := range p.Items {
			ids[i] = v.Event.ID
			if v.Manager {
				t.Fatalf("a list item carries the manager view: %+v", v)
			}
		}
		slices.SortFunc(ids, func(a, b uuid.UUID) int { return compare(a, b) })
		return ids
	}
	set := func(vs ...events.EventView) []uuid.UUID {
		ids := make([]uuid.UUID, len(vs))
		for i, v := range vs {
			ids[i] = v.Event.ID
		}
		slices.SortFunc(ids, func(a, b uuid.UUID) int { return compare(a, b) })
		return ids
	}
	for _, tc := range []struct {
		who  string
		want []uuid.UUID
	}{
		{"anonymous", set(public)},
		{"user_unverified", set(public)},
		{"student", set(public, uniEv)},                       // a verified student of uni_a
		{"other_club_admin", set(public)},                     // uni_b
		{"club_member", set(public, uniEv, memEv, staffOnly)}, // club_a member
		{"event_staff", set(public, uniEv, staffOnly)},        // uni_a student + staff of staffOnly
		{"university_rep", set(public)},                       // member of uni_a only, not a student
		{"moderator", set(public, uniEv, memEv, staffOnly)},   // platform roles see every published event
		{"club_admin", set(public, uniEv, memEv, staffOnly)},  // drafts never appear in the public list
		{"company_member", set(public)},
	} {
		if got := titles(persona(t, tc.who), events.ListQuery{}); !slices.Equal(got, tc.want) {
			t.Errorf("%s sees %v, want %v", tc.who, got, tc.want)
		}
	}
	anon := authz.Anonymous()
	// Turkish search (BE-020 pattern): case and diacritics fold.
	if got := titles(anon, events.ListQuery{Q: "YAPAY ZEKA"}); !slices.Equal(got, set(public)) {
		t.Errorf("q=YAPAY ZEKA → %v", got)
	}
	if got := titles(anon, events.ListQuery{Q: "istanbul"}); len(got) != 0 {
		t.Errorf("q matched the city: %v", got)
	}
	if got := titles(anon, events.ListQuery{City: "ISTANBUL"}); !slices.Equal(got, set(public)) {
		t.Errorf("city=ISTANBUL → %v", got)
	}
	if got := titles(anon, events.ListQuery{UniversityIDs: []uuid.UUID{authzUniversity("uni_b")}}); len(got) != 0 {
		t.Errorf("university_id=uni_b → %v", got)
	}
	if got := titles(anon, events.ListQuery{OrgIDs: []uuid.UUID{orgID("club_a")}, UniversityIDs: []uuid.UUID{authzUniversity("uni_a")}}); !slices.Equal(got, set(public)) {
		t.Errorf("org_id+university_id → %v", got)
	}
	_, err := e.svc.ListEvents(ctx, anon, events.ListQuery{Q: "a"})
	problemtest.WantProblem(t, err, 422, "validation_failed")
	// Ended events leave the default list.
	e.clk.Set(start.Add(3 * time.Hour))
	if got := titles(anon, events.ListQuery{}); len(got) != 0 {
		t.Errorf("after the end: %v", got)
	}
	from := start.Add(-time.Hour)
	if got := titles(anon, events.ListQuery{StartsFrom: &from}); !slices.Equal(got, set(public)) {
		t.Errorf("starts_from: %v", got)
	}
}

func TestListEvents_Pages(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	for i := range 5 {
		in := input("Sayfa Etkinliği")
		in.StartsAt, in.EndsAt = start.Add(time.Duration(i)*time.Hour), start.Add(time.Duration(i)*time.Hour+time.Minute*30)
		e.published(t, in)
	}
	var seen []uuid.UUID
	var after *events.After
	for range 3 {
		p, err := e.svc.ListEvents(ctx, authz.Anonymous(), events.ListQuery{Limit: 2, After: after})
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range p.Items {
			seen = append(seen, v.Event.ID)
		}
		if after = p.Next; after == nil {
			break
		}
	}
	if len(seen) != 5 || slices.Compact(slices.Clone(seen))[4] != seen[4] {
		t.Fatalf("paged ids = %v", seen)
	}
}

func TestGetEvent_MembersOnly_NonMember404(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	in := input("Üyelere Özel Toplantı")
	in.Visibility = events.VisibilityMembers
	v := e.published(t, in)
	if _, err := e.svc.AddStaff(ctx, persona(t, "club_admin"), v.Event.ID, "event_staff@example.test", events.StaffScanner); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"anonymous", "user_unverified", "student", "other_club_admin", "university_rep", "company_admin"} {
		_, err := e.svc.GetEvent(ctx, persona(t, p), v.Event.ID)
		if !errors.Is(err, authz.ErrNotFound) {
			t.Errorf("%s: %v, want 404", p, err)
		}
	}
	for _, p := range []string{"club_member", "club_admin", "event_staff", "moderator", "talent_auditor"} {
		if _, err := e.svc.GetEvent(ctx, persona(t, p), v.Event.ID); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	// A draft is its organiser's admins' only.
	d := e.create(t, "club_admin", "club_a", input("Gizli Taslak"))
	_, err := e.svc.GetEvent(ctx, persona(t, "club_member"), d.Event.ID)
	wantNotFound(t, err)
	_, err = e.svc.GetEvent(ctx, persona(t, "anonymous"), uuid.NewV7())
	wantNotFound(t, err)
}

func TestGetEvent_OnlineURLHiddenFromPublic(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	in := input("Çevrim İçi Söyleşi")
	in.Format, in.OnlineURL = events.FormatOnline, "https://meet.example.test/gizli-oda"
	v := e.published(t, in)
	for _, p := range []string{"anonymous", "student", "club_member", "moderator", "university_rep"} {
		got, err := e.svc.GetEvent(ctx, persona(t, p), v.Event.ID)
		if err != nil || got.Manager {
			t.Fatalf("%s: manager=%v, %v", p, got.Manager, err)
		}
		if b := eventJSON(t, got); containsAny(b, "gizli-oda", "online_url", "is_sensitive_category", "cover_file_id", "created_by") {
			t.Fatalf("%s sees %s", p, b)
		}
	}
	for _, p := range []string{"club_admin", "platform_admin"} {
		got, err := e.svc.GetEvent(ctx, persona(t, p), v.Event.ID)
		if err != nil || !got.Manager || !containsAny(eventJSON(t, got), "gizli-oda") {
			t.Fatalf("%s: manager=%v, %v", p, got.Manager, err)
		}
	}
	// The list cards never carry the link.
	p, err := e.svc.ListEvents(ctx, persona(t, "club_admin"), events.ListQuery{})
	if err != nil || len(p.Items) != 1 || containsAny(cardJSON(t, p.Items[0]), "gizli-oda", "created_by") {
		t.Fatalf("list = %+v, %v", p, err)
	}
}

func TestAddStaff_ClubAdminOnly(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	admin := persona(t, "club_admin")
	v := e.published(t, input("Kapı Görevlileri"))
	id := v.Event.ID
	// Only the organiser's club_admin (and its university's rep for its own
	// events) manages staff: a member, the staff itself, another club's
	// admin and platform staff of a club event get 403 on a public event.
	for _, p := range []string{"club_member", "event_staff", "other_club_admin", "university_rep", "platform_admin", "student"} {
		_, err := e.svc.AddStaff(ctx, persona(t, p), id, "student@example.test", events.StaffScanner)
		wantForbidden(t, err)
	}
	_, err := e.svc.AddStaff(ctx, persona(t, "anonymous"), id, "student@example.test", events.StaffScanner)
	if err == nil {
		t.Fatal("anonymous added staff")
	}
	s, err := e.svc.AddStaff(ctx, admin, id, " Student@Example.test ", events.StaffScanner)
	if err != nil || s.UserID != persona(t, "student").UserID || s.FirstName != "Ayla" || s.LastInitial != "Y." || s.Role != events.StaffScanner {
		t.Fatalf("added = %+v, %v", s, err)
	}
	_, err = e.svc.AddStaff(ctx, admin, id, "student@example.test", events.StaffManager)
	problemtest.WantProblem(t, err, 409, "events.staff_exists")
	_, err = e.svc.AddStaff(ctx, admin, id, "nobody@example.test", events.StaffScanner)
	wantNotFound(t, err) // no account: invitations are phase 2
	e.users.suspended[persona(t, "club_member").UserID] = true
	_, err = e.svc.AddStaff(ctx, admin, id, "club_member@example.test", events.StaffScanner)
	wantNotFound(t, err)
	_, err = e.svc.AddStaff(ctx, admin, id, "student@example.test", "boss")
	problemtest.WantProblem(t, err, 422, "validation_failed")
	// The staff row takes org_id from the event (06 H-38).
	rows, _ := e.mem.StaffOfUser(ctx, s.UserID)
	if len(rows) != 1 || rows[0].OrgID != orgID("club_a") || rows[0].InvitedBy != admin.UserID {
		t.Fatalf("staff rows = %+v", rows)
	}
	list, err := e.svc.ListStaff(ctx, admin, id)
	if err != nil || len(list) != 1 || list[0].LastInitial != "Y." {
		t.Fatalf("list = %+v, %v", list, err)
	}
	_, err = e.svc.ListStaff(ctx, persona(t, "student"), id)
	wantForbidden(t, err) // the staff list is the managers'
	u, err := e.svc.UpdateStaff(ctx, admin, id, s.UserID, events.StaffManager)
	if err != nil || u.Role != events.StaffManager {
		t.Fatalf("update = %+v, %v", u, err)
	}
	_, err = e.svc.UpdateStaff(ctx, admin, id, uuid.NewV7(), events.StaffManager)
	wantNotFound(t, err)
	wantForbidden(t, e.svc.RemoveStaff(ctx, persona(t, "club_member"), id, s.UserID))
	if err := e.svc.RemoveStaff(ctx, admin, id, s.UserID); err != nil {
		t.Fatal(err)
	}
	wantNotFound(t, e.svc.RemoveStaff(ctx, admin, id, s.UserID))
	if got := e.audit.Of(authz.EventsStaffManage); len(got) != 3 {
		t.Fatalf("staff audit = %+v", got)
	}
	for _, a := range e.audit.Of(authz.EventsStaffManage) {
		if b, _ := json.Marshal(a.Metadata); containsAny(string(b), "@") {
			t.Fatalf("an address in the audit metadata: %s", b)
		}
	}
	// A university event: its university_rep manages the staff.
	ue := e.create(t, "university_rep", "uni_a", input("Rektörlük Etkinliği"))
	if _, err := e.svc.AddStaff(ctx, persona(t, "university_rep"), ue.Event.ID, "student@example.test", events.StaffScanner); err != nil {
		t.Fatal(err)
	}
	// Another org's draft: 404 for every staff operation.
	d := e.create(t, "club_admin", "club_a", input("Taslak Görevliler"))
	_, err = e.svc.AddStaff(ctx, persona(t, "other_club_admin"), d.Event.ID, "student@example.test", events.StaffScanner)
	wantNotFound(t, err)
	_, err = e.svc.ListStaff(ctx, persona(t, "other_club_admin"), d.Event.ID)
	wantNotFound(t, err)
}

func TestEventsGet_SensitiveFromClub(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	plain := e.create(t, "club_admin", "club_a", input("Sıradan Etkinlik"))
	cat := input("Hassas Kategori")
	cat.IsSensitiveCategory = true
	byCategory := e.create(t, "club_admin", "club_a", cat)
	sens := e.create(t, "club_admin_sensitive", "club_sensitive", input("Hassas Kulüp Etkinliği"))
	for _, tc := range []struct {
		id   uuid.UUID
		want bool
	}{{plain.Event.ID, false}, {byCategory.Event.ID, true}, {sens.Event.ID, true}} {
		got, err := e.svc.Get(ctx, tc.id)
		if err != nil || got.Sensitive != tc.want || got.ID != tc.id || got.Status != events.StatusDraft {
			t.Fatalf("Get(%s) = %+v, %v; want sensitive=%v", tc.id, got, err, tc.want)
		}
	}
	if _, err := e.svc.Get(ctx, uuid.NewV7()); !errors.Is(err, events.ErrNotFound) {
		t.Fatalf("Get(unknown) = %v", err)
	}
	// The flag never reaches the audit record.
	for _, a := range e.audit.Of(authz.EventsEventCreate) {
		if _, ok := a.Metadata["sensitive"]; ok {
			t.Fatalf("audit carries the sensitive flag: %+v", a)
		}
	}
}

func TestListOrgEvents_DraftsForManagers(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	in := input("Çevrim İçi Taslak")
	in.Format, in.OnlineURL = events.FormatOnline, "https://meet.example.test/oda"
	d := e.create(t, "club_admin", "club_a", in)
	p := e.published(t, input("Yayımlı"))
	list := func(who string, q events.OrgListQuery) (events.EventPage, error) {
		return e.svc.ListOrgEvents(ctx, persona(t, who), orgID("club_a"), q)
	}
	got, err := list("club_admin", events.OrgListQuery{})
	if err != nil || len(got.Items) != 2 || !got.Items[0].Manager {
		t.Fatalf("club_admin = %+v, %v", got, err)
	}
	got, err = list("moderator", events.OrgListQuery{Statuses: []events.Status{events.StatusDraft}})
	if err != nil || len(got.Items) != 1 || got.Items[0].Event.ID != d.Event.ID || got.Items[0].Manager {
		t.Fatalf("moderator drafts = %+v, %v", got, err)
	}
	_, err = list("club_member", events.OrgListQuery{})
	wantForbidden(t, err)
	_, err = list("other_club_admin", events.OrgListQuery{})
	wantNotFound(t, err)
	_, err = e.svc.ListOrgEvents(ctx, persona(t, "platform_admin"), uuid.NewV7(), events.OrgListQuery{})
	wantNotFound(t, err)
	_ = p
}

func TestPrivacy_StaffExportAndErase(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	v := e.published(t, input("Gizlilik"))
	user := persona(t, "student").UserID
	if _, err := e.svc.AddStaff(ctx, persona(t, "club_admin"), v.Event.ID, "student@example.test", events.StaffManager); err != nil {
		t.Fatal(err)
	}
	if e.mod.Privacy.Name() != "events" {
		t.Fatalf("provider name %q", e.mod.Privacy.Name())
	}
	sec, err := e.mod.Privacy.Export(ctx, user)
	if err != nil || len(sec.Items) != 1 || sec.Items[0]["event_id"] != v.Event.ID.String() || sec.Items[0]["role"] != "manager" {
		t.Fatalf("export = %+v, %v", sec, err)
	}
	res, err := e.mod.Privacy.Erase(ctx, user)
	if err != nil || res.Deleted != 1 {
		t.Fatalf("erase = %+v, %v", res, err)
	}
	if res, err = e.mod.Privacy.Erase(ctx, user); err != nil || res.Deleted != 0 {
		t.Fatalf("second erase = %+v, %v", res, err)
	}
}

// TestCreateEvent_DescriptionAllowList: the description is Tiptap JSON of
// the allow-listed nodes and marks (05 §14.6); unknown attributes are
// dropped, a script link or an unknown node is 422, and the plain text
// feeds the search.
func TestCreateEvent_DescriptionAllowList(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	doc := func(js string) map[string]any {
		var m map[string]any
		if err := json.Unmarshal([]byte(js), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	in := input("Zengin Metin")
	in.Description = doc(`{"type":"doc","content":[{"type":"paragraph","attrs":{"style":"x"},"content":[
		{"type":"text","text":"Merhaba ","marks":[{"type":"bold"}]},
		{"type":"text","text":"bağlantı","marks":[{"type":"link","attrs":{"href":"https://koro.test","onclick":"x"}}]}]}]}`)
	v := e.create(t, "club_admin", "club_a", in)
	if got := string(v.Event.DescriptionJSON); containsAny(got, "style", "onclick") || !containsAny(got, "https://koro.test") {
		t.Fatalf("stored description = %s", got)
	}
	if v.Event.DescriptionText == nil || *v.Event.DescriptionText != "Merhaba bağlantı" {
		t.Fatalf("description text = %v", v.Event.DescriptionText)
	}
	for _, bad := range []string{
		`{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"x","marks":[{"type":"link","attrs":{"href":"javascript:alert(1)"}}]}]}]}`,
		`{"type":"doc","content":[{"type":"image","attrs":{"src":"https://koro.test/x.png"}}]}`,
		`{"type":"doc","content":[{"type":"heading","attrs":{"level":1},"content":[{"type":"text","text":"x"}]}]}`,
		`{"type":"paragraph"}`,
	} {
		in := input("Kötü Metin")
		in.Description = doc(bad)
		_, err := e.svc.CreateEvent(ctx, persona(t, "club_admin"), orgID("club_a"), in)
		problemtest.WantProblem(t, err, 422, "validation_failed")
	}
}
