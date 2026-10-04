package main

import (
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/events"
	"koro/internal/platform/crypto/rewrap"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
	"koro/internal/platform/privacy"
)

// useMemStore installs the in-memory events stores used by service,
// wiring and authz matrix tests (05 §13.2). First owner: BE-024.
// Only tickets of the events module edit this file (BE-001 R4).
func (w *eventsWiring) useMemStore() {
	w.tx = db.NopTx{}
	w.newStore = func(c *container) events.Store { return events.NewMemStore(c.infra.clock.Now) }
	w.sealer, w.tickets = testEnvelope(), testDeriver() // the shared test key and token deriver (identity's)
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

// TestWire_EventsRegistrationTasks pins what BE-028 adds: the outbox
// subscriptions (event.cancelled and the registration notices), the jobs
// of 01 §7 and the rewrap declaration of the sensitive answers.
func TestWire_EventsRegistrationTasks(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	for topic, name := range map[string]string{
		domainevent.TopicEventCancelled:         events.CancelRegistrationsTaskName,
		domainevent.TopicRegistrationSubmitted:  events.NotifySubmittedTaskName,
		domainevent.TopicRegistrationWaitlisted: events.NotifyWaitlistedTaskName,
		domainevent.TopicRegistrationRejected:   events.NotifyRejectedTaskName,
		domainevent.TopicRegistrationCancelled:  events.NotifyCancelledTaskName,
	} {
		if !slices.ContainsFunc(c.infra.subs.For(topic), func(s outbox.Subscription) bool { return s.Name == name }) {
			t.Errorf("%s has no subscription %s", topic, name)
		}
	}
	var jobs []string
	for _, j := range c.infra.jobs.Jobs() {
		if strings.HasPrefix(j.Name, "events.") {
			if err := j.Validate(); err != nil {
				t.Fatal(err)
			}
			jobs = append(jobs, j.Name+" "+j.Cron+" "+j.Queue)
		}
	}
	if want := []string{"events.waitlist_expire */15 * * * * default", "events.reconcile_counts 0 3 * * * low"}; !slices.Equal(jobs, want) {
		t.Fatalf("events jobs = %q, want %q", jobs, want)
	}
	if !slices.ContainsFunc(c.keys.columns, func(col rewrap.Column) bool {
		return col.Table == "event_registrations" && col.Column == "answers_sensitive_enc" &&
			col.PurposePrefix == events.SensitiveAnswersPurposePrefix && col.PurposeColumn == "id"
	}) {
		t.Fatalf("rewrap columns = %+v", c.keys.columns)
	}
	// The registrations reader of students (T-241) answers from the built store.
	if n, err := c.events.svc.CountInOrg(t.Context(), uuid.NewV7(), uuid.NewV7()); err != nil || n != 0 {
		t.Fatalf("CountInOrg = %d, %v", n, err)
	}
}
