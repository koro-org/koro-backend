package events_test

import (
	"testing"
	"uuid"
)

// TestParticipation_OnStaff (BE-090; 00 T-341): talent's staff rule reads
// the door staff of an event in the event's org. A scanner and a manager
// are on the staff; a user without a staff row, the staff member under
// another org, an unknown event and a missing org are not.
func TestParticipation_OnStaff(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.doorEvent(t)
	org, id := ev.Event.OrgID, ev.Event.ID
	ctx := t.Context()
	for _, name := range []string{"event_staff", "event_manager_a"} {
		if ok, err := e.svc.OnStaff(ctx, org, id, persona(t, name).UserID); err != nil || !ok {
			t.Fatalf("OnStaff(%s) = %v, %v; want true", name, ok, err)
		}
	}
	staff := persona(t, "event_staff").UserID
	for name, c := range map[string]struct{ org, event, user uuid.UUID }{
		"not on the staff": {org, id, adultStudent(t).UserID},
		"another org":      {uuid.NewV7(), id, staff},
		"unknown event":    {org, uuid.NewV7(), staff},
		"no org":           {uuid.Nil(), id, staff},
	} {
		if ok, err := e.svc.OnStaff(ctx, c.org, c.event, c.user); err != nil || ok {
			t.Errorf("%s: OnStaff = %v, %v; want false", name, ok, err)
		}
	}
}
