package events_test

import (
	"testing"
	"time"
	"uuid"

	"koro/internal/events"
)

// TestCompleteEnded_CompletesEndedPublishedEvents (T-264): the hourly
// events.complete_ended turns a published event that has ended into
// completed (audited as the system), leaves an upcoming one and a draft
// alone, is idempotent, and the completed event stays in a date-range list
// and keeps its door open (check-in works for completed events).
func TestCompleteEnded_CompletesEndedPublishedEvents(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ended := e.doorEvent(t)
	_, tk := e.ticketOf(t, adultStudent(t), ended.Event.ID)
	in := input("Sonraki Atölye")
	in.StartsAt, in.EndsAt = start.Add(48*time.Hour), start.Add(50*time.Hour)
	later := e.published(t, in)
	draft := e.create(t, "club_admin", "club_a", input("Taslak Etkinlik"))

	e.clk.Set(ended.Event.EndsAt.Add(-time.Minute))
	e.job(t, events.CompleteEndedJobName)
	if got, _ := e.mem.GetEvent(t.Context(), ended.Event.ID); got.Status != events.StatusPublished {
		t.Fatalf("completed before its end: %s", got.Status)
	}

	e.clk.Set(ended.Event.EndsAt.Add(time.Minute))
	e.job(t, events.CompleteEndedJobName)
	e.job(t, events.CompleteEndedJobName) // idempotent
	for id, want := range map[uuid.UUID]events.Status{
		ended.Event.ID: events.StatusCompleted, later.Event.ID: events.StatusPublished, draft.Event.ID: events.StatusDraft,
	} {
		if got, _ := e.mem.GetEvent(t.Context(), id); got.Status != want {
			t.Fatalf("event %s = %s, want %s", id, got.Status, want)
		}
	}
	if n := len(e.audit.of("events.event.complete")); n != 1 {
		t.Fatalf("%d completion audit records, want 1", n)
	}

	from, to := start.Add(-time.Hour), start.Add(time.Hour)
	page, err := e.svc.ListEvents(t.Context(), adultStudent(t), events.ListQuery{StartsFrom: &from, StartsTo: &to})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Event.ID != ended.Event.ID || page.Items[0].Event.Status != events.StatusCompleted {
		t.Fatalf("date-range list = %+v", page.Items)
	}
	if res := e.scan(t, persona(t, "event_staff"), ended.Event.ID, tk.Payload); res.Result != events.ResultOK {
		t.Fatalf("scan of a completed event = %+v", res)
	}

	var crons []string
	for _, j := range e.mod.Jobs() {
		if j.Name == events.CompleteEndedJobName {
			crons = append(crons, j.Cron)
		}
	}
	if len(crons) != 1 || crons[0] != "40 * * * *" {
		t.Fatalf("job = %v", crons)
	}
}
