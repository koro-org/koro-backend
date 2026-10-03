package main

import (
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/events"
	"koro/internal/platform/db"
	"koro/internal/platform/privacy"
)

// useMemStore installs the in-memory events stores used by service,
// wiring and authz matrix tests (05 §13.2). First owner: BE-024.
// Only tickets of the events module edit this file (BE-001 R4).
func (w *eventsWiring) useMemStore() {
	w.tx = db.NopTx{}
	w.newStore = func(c *container) events.Store { return events.NewMemStore(c.infra.clock.Now) }
}

// TestWire_EventsProvidesServiceAndPrivacy pins what events binds in build:
// its service (events.Events for later layers, answering from the built
// store) and its KVKK provider.
func TestWire_EventsProvidesServiceAndPrivacy(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	if c.events.svc == nil || c.events.built == nil {
		t.Fatal("events service not built")
	}
	if !slices.ContainsFunc(c.privacy, func(p privacy.Provider) bool { return p.Name() == "events" }) {
		t.Fatal("events privacy provider not registered")
	}
	ctx := t.Context()
	if _, err := c.events.svc.Get(ctx, uuid.NewV7()); err == nil {
		t.Fatal("Get(unknown) found an event")
	}
	mem := c.events.built.(*events.MemStore)
	start := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	e := events.Event{
		ID: uuid.NewV7(), OrgID: uuid.NewV7(), CreatedBy: uuid.NewV7(), Title: "Kanca Etkinliği", Slug: "kanca",
		Category: events.CategoryTalk, Format: events.FormatInPerson, StartsAt: start, EndsAt: start.Add(time.Hour),
		Visibility: events.VisibilityPublic, Status: events.StatusDraft, RequiresRegistration: true,
		RegistrationMode: events.ModeAuto, CreatedAt: start, UpdatedAt: start,
	}
	if err := mem.PutEvent(e); err != nil {
		t.Fatal(err)
	}
	got, err := c.events.svc.Get(ctx, e.ID)
	if err != nil || got.ID != e.ID || got.OrgID != e.OrgID || got.Sensitive {
		t.Fatalf("Get = %+v, %v", got, err)
	}
}
