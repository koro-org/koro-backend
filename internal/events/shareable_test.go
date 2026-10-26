package events_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/events"
)

// failingCatalog is fakeCatalog whose club sensitivity cannot be read
// once fail is set.
type failingCatalog struct {
	fakeCatalog
	fail *atomic.Bool
}

func (f failingCatalog) SensitiveOrgIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	if f.fail.Load() {
		return nil, errors.New("directory unavailable")
	}
	return f.fakeCatalog.SensitiveOrgIDs(ctx, ids)
}

// TestEventSummary_ShareableBy pins the event_share rule (feed's 422
// body.event_id; BE-133 moves it into events): a published public event
// is shareable by anyone; a sensitive one by its organiser only; a draft,
// cancelled, completed or members-only event by nobody.
func TestEventSummary_ShareableBy(t *testing.T) {
	t.Parallel()
	own, other := uuid.NewV7(), uuid.NewV7()
	pub := events.EventSummary{ID: uuid.NewV7(), OrgID: own, Status: events.StatusPublished, Visibility: events.VisibilityPublic}
	for name, c := range map[string]struct {
		edit       func(*events.EventSummary)
		own, other bool
	}{
		"published public":   {func(*events.EventSummary) {}, true, true},
		"sensitive":          {func(e *events.EventSummary) { e.Sensitive = true }, true, false},
		"draft":              {func(e *events.EventSummary) { e.Status = events.StatusDraft }, false, false},
		"cancelled":          {func(e *events.EventSummary) { e.Status = events.StatusCancelled }, false, false},
		"completed":          {func(e *events.EventSummary) { e.Status = events.StatusCompleted }, false, false},
		"members only":       {func(e *events.EventSummary) { e.Visibility = events.VisibilityMembers }, false, false},
		"university only":    {func(e *events.EventSummary) { e.Visibility = events.VisibilityUniversity }, false, false},
		"sensitive, members": {func(e *events.EventSummary) { e.Sensitive, e.Visibility = true, events.VisibilityMembers }, false, false},
	} {
		e := pub
		c.edit(&e)
		if got := e.ShareableBy(own); got != c.own {
			t.Errorf("%s: ShareableBy(organiser) = %v, want %v", name, got, c.own)
		}
		if got := e.ShareableBy(other); got != c.other {
			t.Errorf("%s: ShareableBy(other org) = %v, want %v", name, got, c.other)
		}
	}
}

// TestEvents_ShareableByOthers_SameRuleAsFeed (BE-133, WEB-022): the
// event page and every card say whether another org may share the event,
// with the answer feed's createPost gives (Events.Get → ShareableBy): a
// published public event of a non-sensitive club yes; a draft, a
// members-only or sensitive-category event and a sensitive club's event
// no. The DTO carries the flag.
func TestEvents_ShareableByOthers_SameRuleAsFeed(t *testing.T) {
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
	want := map[uuid.UUID]bool{}
	for _, c := range []struct {
		v         events.EventView
		shareable bool
	}{
		{publish("club_admin", "club_a", input("Go Atölyesi")), true},
		{e.create(t, "club_admin", "club_a", input("Taslak")), false},
		{publish("club_admin", "club_a", members), false},
		{publish("club_admin", "club_a", special), false},
		{publish("club_admin_sensitive", "club_sensitive", input("Hassas Kulüp Buluşması")), false},
	} {
		want[c.v.Event.ID] = c.shareable
	}

	other := orgID("club_b")
	for id, shareable := range want {
		sum, err := e.svc.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if sum.ShareableBy(other) != shareable {
			t.Errorf("%s: Events.Get().ShareableBy(club_b) = %v, want %v", sum.Title, !shareable, shareable)
		}
		v, err := e.svc.GetEvent(ctx, persona(t, "platform_admin"), id)
		if err != nil {
			t.Fatalf("GetEvent: %v", err)
		}
		if v.ShareableByOthers != shareable || events.ToEventDTO(v).ShareableByOthers != shareable {
			t.Errorf("%s: event page shareable_by_others = %v, want %v", sum.Title, v.ShareableByOthers, shareable)
		}
	}
	page, err := e.svc.ListEvents(ctx, authz.Anonymous(), events.ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	listed := 0
	for _, v := range page.Items {
		if shareable, ok := want[v.Event.ID]; ok {
			listed++
			if d := events.ToCardDTO(v); d.ShareableByOthers != shareable {
				t.Errorf("listEvents card %q: shareable_by_others = %v, want %v", v.Event.Title, d.ShareableByOthers, shareable)
			}
		}
	}
	if listed == 0 {
		t.Fatal("no card listed")
	}

	// The organiser's sensitivity cannot be read: nobody else may share
	// (fail closed), as feed refuses the share when events.Get fails.
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
	for _, v := range fp.Items {
		if v.ShareableByOthers {
			t.Errorf("%q shareable without the organiser's sensitivity", v.Event.Title)
		}
	}
	if len(fp.Items) == 0 {
		t.Fatal("no card listed without the directory")
	}
}
