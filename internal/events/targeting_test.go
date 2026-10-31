package events_test

import (
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/events"
)

// TestTargetableEvents_Rule (BE-161, Q-39): a company may name a published
// or completed public event that is neither of a sensitive category nor a
// sensitive club's and started within TargetingWindow (or has yet to
// start); a draft, a cancelled, a members-only or university-only, a
// sensitive or a too old event is not targetable; an unknown one is
// missing. Without the organiser's sensitivity nothing is answered (fail
// closed).
func TestTargetableEvents_Rule(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	clubA, clubSens, by := orgID("club_a"), orgID("club_sensitive"), uuid.NewV7()
	put := func(name string, edit func(*events.Event)) uuid.UUID {
		t.Helper()
		ev := row(clubA, by, name, start)
		edit(&ev)
		got, ok, err := e.mem.InsertEvent(ctx, ev)
		if err != nil || !ok {
			t.Fatalf("InsertEvent(%s): %v %v", name, ok, err)
		}
		return got.ID
	}
	at := func(d time.Time) func(*events.Event) {
		return func(ev *events.Event) { ev.StartsAt, ev.EndsAt = d, d.Add(time.Hour) }
	}
	want := map[uuid.UUID]bool{
		put("Yayında", func(*events.Event) {}): true,
		put("Tamamlandı", func(ev *events.Event) {
			ev.Status = events.StatusCompleted
			at(t0.Add(-30 * 24 * time.Hour))(ev)
		}): true,
		put("Pencerenin İçinde", at(t0.Add(-events.TargetingWindow).Add(time.Hour))):                         true,
		put("Pencerenin Dışında", at(t0.Add(-events.TargetingWindow).Add(-time.Hour))):                       false,
		put("Taslak", func(ev *events.Event) { ev.Status, ev.PublishedAt = events.StatusDraft, nil }):        false,
		put("İptal", func(ev *events.Event) { ev.Status, ev.CancelledAt = events.StatusCancelled, new(t0) }): false,
		put("Üyelere Özel", func(ev *events.Event) { ev.Visibility = events.VisibilityMembers }):             false,
		put("Üniversiteye Özel", func(ev *events.Event) {
			u := uuid.NewV7()
			ev.Visibility, ev.VisibilityUniversityID = events.VisibilityUniversity, &u
		}): false,
		put("Hassas Kategori", func(ev *events.Event) { ev.IsSensitiveCategory = true }): false,
		put("Hassas Kulüp", func(ev *events.Event) { ev.OrgID = clubSens }):              false,
	}
	ids := []uuid.UUID{uuid.NewV7()} // unknown: missing
	for id := range want {
		ids = append(ids, id)
	}
	got, err := e.svc.TargetableEvents(ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("TargetableEvents answered %d events, want %d (the unknown one missing)", len(got), len(want))
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("TargetableEvents[%s] = %v, want %v", id, got[id], w)
		}
	}
	if got, err := e.svc.TargetableEvents(ctx, nil); err != nil || len(got) != 0 {
		t.Fatalf("TargetableEvents(nil) = %v, %v", got, err)
	}

	down := &atomic.Bool{}
	fe := newEnv(t, func(d *events.Deps) { d.Catalog = failingCatalog{fail: down} })
	ev, _, err := fe.mem.InsertEvent(ctx, row(clubA, by, "Dizin Yok", start))
	if err != nil {
		t.Fatal(err)
	}
	down.Store(true)
	if got, err := fe.svc.TargetableEvents(ctx, []uuid.UUID{ev.ID}); err == nil {
		t.Fatalf("TargetableEvents without directory = %v, want an error (fail closed)", got)
	}
}

// TestTargetableParticipations_HeldPlacesOfTargetableEvents (BE-161): a
// user's confirmed, checked-in (Attended) and no-show registrations of
// targetable events come back; a cancelled or waitlisted registration, a
// members-only, sensitive-category, sensitive club's or too old event and
// another user's registration do not.
func TestTargetableParticipations_HeldPlacesOfTargetableEvents(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	clubA, by := orgID("club_a"), uuid.NewV7()
	insert := func(name string, edit func(*events.Event)) events.Event {
		t.Helper()
		ev := row(clubA, by, name, start)
		edit(&ev)
		got, ok, err := e.mem.InsertEvent(ctx, ev)
		if err != nil || !ok {
			t.Fatalf("InsertEvent(%s): %v %v", name, ok, err)
		}
		return got
	}
	register := func(ev events.Event, user uuid.UUID, st events.RegStatus) {
		t.Helper()
		r := newReg(ev, user, st)
		switch st {
		case events.RegCheckedIn, events.RegNoShow:
			r.ConfirmedAt = new(t0)
			if st == events.RegCheckedIn {
				r.CheckedInAt = new(t0)
			}
		case events.RegCancelled:
			by := events.ByUser
			r.CancelledAt, r.CancelledByKind = new(t0), &by
		}
		if _, err := e.mem.InsertRegistration(ctx, r); err != nil {
			t.Fatalf("InsertRegistration(%s, %s): %v", ev.Title, st, err)
		}
	}
	ada, can, other := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	talk := insert("Kariyer Günü", func(*events.Event) {})
	fair := insert("Staj Fuarı", func(ev *events.Event) {
		ev.Status = events.StatusCompleted
		ev.StartsAt, ev.EndsAt = t0.Add(-24*time.Hour), t0.Add(-23*time.Hour)
	})
	register(talk, ada, events.RegConfirmed)
	register(fair, ada, events.RegCheckedIn)
	register(talk, can, events.RegNoShow)
	register(fair, can, events.RegCancelled)
	register(talk, other, events.RegConfirmed) // not asked for
	for _, ev := range []events.Event{
		insert("Üyelere Özel", func(ev *events.Event) { ev.Visibility = events.VisibilityMembers }),
		insert("Hassas Kategori", func(ev *events.Event) { ev.IsSensitiveCategory = true }),
		insert("Hassas Kulüp", func(ev *events.Event) { ev.OrgID = orgID("club_sensitive") }),
		insert("Eski", func(ev *events.Event) {
			ev.StartsAt = t0.Add(-events.TargetingWindow - 24*time.Hour)
			ev.EndsAt = ev.StartsAt.Add(time.Hour)
		}),
	} {
		register(ev, ada, events.RegCheckedIn)
	}
	waiting := insert("Bekleme Listesi", func(*events.Event) {})
	register(waiting, can, events.RegWaitlisted)

	got, err := e.svc.TargetableParticipations(ctx, []uuid.UUID{ada, can, uuid.NewV7()})
	if err != nil {
		t.Fatal(err)
	}
	want := map[uuid.UUID][]events.TargetableParticipation{
		ada: {{EventID: talk.ID}, {EventID: fair.ID, Attended: true}}, // newest start first
		can: {{EventID: talk.ID}},
	}
	if len(got) != len(want) {
		t.Fatalf("TargetableParticipations = %+v, want %+v", got, want)
	}
	for u, w := range want {
		if len(got[u]) != len(w) {
			t.Fatalf("participations of %s = %+v, want %+v", u, got[u], w)
		}
		for i := range w {
			if got[u][i] != w[i] {
				t.Errorf("participation %d of %s = %+v, want %+v", i, u, got[u][i], w[i])
			}
		}
	}
	if got, err := e.svc.TargetableParticipations(ctx, nil); err != nil || len(got) != 0 {
		t.Fatalf("TargetableParticipations(nil) = %v, %v", got, err)
	}
}

// TestEvents_TargetableFlag (BE-161): the event page and every card say
// whether a company may name the event in an audience (the rule of
// TargetableEvents), so the web's event picker needs no other read: a
// published public event of a non-sensitive club yes; a members-only or
// sensitive-category event and a sensitive club's event no; false while
// the organiser's sensitivity cannot be read.
func TestEvents_TargetableFlag(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	publish := func(actor, org string, in events.EventInput) events.EventView {
		t.Helper()
		v := e.create(t, actor, org, in)
		if err := e.mem.SetFormVersion(v.Event.ID, uuid.NewV7()); err != nil {
			t.Fatal(err)
		}
		p, err := e.svc.PublishEvent(t.Context(), persona(t, actor), v.Event.ID)
		if err != nil {
			t.Fatalf("PublishEvent: %v", err)
		}
		return p
	}
	members, special := input("Üyelere Özel"), input("Hassas Kategori")
	members.Visibility = events.VisibilityMembers
	special.IsSensitiveCategory = true
	want := map[uuid.UUID]bool{
		publish("club_admin", "club_a", input("Mülakat Atölyesi")).Event.ID:                         true,
		publish("club_admin", "club_a", members).Event.ID:                                           false,
		publish("club_admin", "club_a", special).Event.ID:                                           false,
		publish("club_admin_sensitive", "club_sensitive", input("Hassas Kulüp Buluşması")).Event.ID: false,
	}
	ids := make([]uuid.UUID, 0, len(want))
	for id := range want {
		ids = append(ids, id)
	}
	rule, err := e.svc.TargetableEvents(ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	for id, w := range want {
		v, err := e.svc.GetEvent(ctx, persona(t, "platform_admin"), id)
		if err != nil {
			t.Fatalf("GetEvent: %v", err)
		}
		if v.Targetable != w || events.ToEventDTO(v).Targetable != w || rule[id] != w {
			t.Errorf("%s: targetable = %v (DTO %v, rule %v), want %v", v.Event.Title, v.Targetable,
				events.ToEventDTO(v).Targetable, rule[id], w)
		}
	}
	page, err := e.svc.ListEvents(ctx, authz.Anonymous(), events.ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	listed := 0
	for _, v := range page.Items {
		if w, ok := want[v.Event.ID]; ok {
			listed++
			if events.ToCardDTO(v).Targetable != w {
				t.Errorf("listEvents card %q: targetable = %v, want %v", v.Event.Title, !w, w)
			}
		}
	}
	if listed == 0 {
		t.Fatal("no card listed")
	}

	down := &atomic.Bool{}
	fe := newEnv(t, func(d *events.Deps) { d.Catalog = failingCatalog{fail: down} })
	fv := fe.create(t, "club_admin", "club_a", input("Dizin Yok"))
	if err := fe.mem.SetFormVersion(fv.Event.ID, uuid.NewV7()); err != nil {
		t.Fatal(err)
	}
	if _, err := fe.svc.PublishEvent(ctx, persona(t, "club_admin"), fv.Event.ID); err != nil {
		t.Fatalf("PublishEvent: %v", err)
	}
	down.Store(true)
	fp, err := fe.svc.ListEvents(ctx, authz.Anonymous(), events.ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(fp.Items) == 0 {
		t.Fatal("no card listed")
	}
	for _, v := range fp.Items {
		if v.Targetable {
			t.Fatalf("%q targetable while the organiser's sensitivity cannot be read", v.Event.Title)
		}
	}
}
