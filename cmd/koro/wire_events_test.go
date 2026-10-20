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
	"koro/internal/platform/queue"
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
	if want := []string{"events.waitlist_expire */15 * * * * default", "events.reconcile_counts 0 3 * * * low",
		"events.auto_finalize_attendance 20 * * * * default", "events.complete_ended 40 * * * * default",
		"events.purge_exports 25 * * * * low"}; !slices.Equal(jobs, want) {
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

// TestWire_EventsCheckinTasks pins what BE-031 adds: the ticket mails
// (critical), the schedule update, the cancellation .ics and the no-shows
// as outbox subscriptions, the resend task, and the students hook of
// T-241 answering from the events store.
func TestWire_EventsCheckinTasks(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	for _, x := range []struct{ topic, name, queue string }{
		{domainevent.TopicRegistrationConfirmed, events.SendTicketEmailTaskName, queue.Critical},
		{domainevent.TopicTicketReissued, events.SendReissuedTicketTaskName, queue.Critical},
		{domainevent.TopicEventScheduleChanged, events.SendScheduleUpdateTaskName, queue.Default},
		{domainevent.TopicRegistrationCancelled, events.SendCancellationEmailTaskName, queue.Default},
		{domainevent.TopicAttendanceFinalized, events.MarkNoShowsTaskName, queue.Default},
	} {
		if !slices.ContainsFunc(c.infra.subs.For(x.topic), func(s outbox.Subscription) bool {
			return s.Name == x.name && s.Queue == x.queue
		}) {
			t.Errorf("%s has no subscription %s on %s", x.topic, x.name, x.queue)
		}
	}
	if !slices.ContainsFunc(c.infra.jobs.Tasks(), func(tk queue.Task) bool { return tk.Name == events.ResendTicketTaskName }) {
		t.Fatal("events.resend_ticket_email has no handler")
	}
	if c.events.keys == nil {
		t.Fatal("no ticket key ring")
	}
	// T-241: the students reader is bound and answers from the events store.
	if c.students.registrations == nil {
		t.Fatal("c.students.registrations is not bound")
	}
	if n, err := c.students.registrations.CountInOrg(t.Context(), uuid.NewV7(), uuid.NewV7()); err != nil || n != 0 {
		t.Fatalf("CountInOrg = %d, %v", n, err)
	}
	if rows, err := c.students.registrations.SummaryOf(t.Context(), uuid.NewV7()); err != nil || len(rows) != 0 {
		t.Fatalf("SummaryOf = %+v, %v", rows, err)
	}
}

// TestWire_EventsExports pins what BE-050 adds: the build task, the purge
// job of 01 §7, the files service kept for the export fixtures, and the
// three operations registered with their Koro metadata (R + export class
// on createEventExport).
func TestWire_EventsExports(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(c.infra.jobs.Tasks(), func(tk queue.Task) bool { return tk.Name == events.BuildExportTaskName }) {
		t.Fatal("events.build_export has no handler")
	}
	if !slices.ContainsFunc(c.infra.jobs.Jobs(), func(j queue.Job) bool {
		return j.Name == events.PurgeExportsJobName && j.Cron == "25 * * * *" && j.Queue == queue.Low
	}) {
		t.Fatal("events.purge_exports is not scheduled hourly at :25 on low")
	}
	if c.events.files == nil {
		t.Fatal("events has no files service (export files)")
	}
	if !slices.ContainsFunc(c.privacy, func(p privacy.Provider) bool { return p.Name() == "events" }) ||
		!slices.ContainsFunc(c.sweepers, func(s privacy.RetentionSweeper) bool {
			return slices.Contains(s.PolicyCodes(), events.PolicyExportLog)
		}) {
		t.Fatal("the export log has no provider or sweeper")
	}
}
