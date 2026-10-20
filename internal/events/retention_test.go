package events_test

import (
	"slices"
	"testing"
	"time"

	"koro/internal/events"
	"koro/internal/platform/privacy"
)

// TestPurgeFormAnswers_NullsColumnsKeepsRegistration (02 §11.6, §24.2;
// BE-057): after the event the special category answers go first (30
// days), then every answer (90 days): the columns are emptied, the
// registration (status, ticket) stays. Unapproved, the sweep only counts.
func TestPurgeFormAnswers_NullsColumnsKeepsRegistration(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	ev := e.regEvent(t, nil, motivationField, allergyField)
	a := persona(t, "student")
	v := e.mustRegister(t, a, ev.Event.ID, map[string]any{"motivation": "x", "allergy": "Fıstık alerjim var"})
	get := func() events.Registration {
		t.Helper()
		r, err := e.mem.GetRegistration(ctx, ev.Event.OrgID, ev.Event.ID, v.Registration.ID)
		if err != nil {
			t.Fatalf("the registration is gone: %v", err)
		}
		return r
	}
	if got := e.mod.Sweeper.PolicyCodes(); !slices.Equal(got, []string{events.PolicyExportLog, events.PolicyFormAnswers,
		events.PolicyFormAnswersSensitive, events.PolicyStaff}) {
		t.Fatalf("PolicyCodes() = %v", got)
	}
	day := 24 * time.Hour
	sens := privacy.RetentionPolicy{Code: events.PolicyFormAnswersSensitive, Retention: 30 * day}
	all := privacy.RetentionPolicy{Code: events.PolicyFormAnswers, Retention: 90 * day}

	// 31 days after the end: the dry run counts, nothing changes.
	at := ev.Event.EndsAt.Add(31 * day)
	if res, err := e.mod.Sweeper.Sweep(ctx, sens, at); err != nil || res.Count != 1 || res.Method != privacy.MethodNullColumns {
		t.Fatalf("dry run = %+v, %v", res, err)
	}
	if len(get().AnswersSensitiveEnc) == 0 {
		t.Fatal("a dry run emptied the special category answers")
	}
	sens.LegalApproved, all.LegalApproved = true, true
	if res, err := e.mod.Sweeper.Sweep(ctx, sens, at); err != nil || res.Count != 1 ||
		res.Table != "event_registrations.answers_sensitive_enc" {
		t.Fatalf("sensitive sweep = %+v, %v", res, err)
	}
	if res, err := e.mod.Sweeper.Sweep(ctx, all, at); err != nil || res.Count != 0 {
		t.Fatalf("answers sweep before 90 days = %+v, %v", res, err)
	}
	r := get()
	if r.AnswersSensitiveEnc != nil || string(r.Answers) == "{}" || r.AnswersPurgedAt != nil {
		t.Fatalf("after 31 days = answers %s, sealed %d bytes, purged %v", r.Answers, len(r.AnswersSensitiveEnc), r.AnswersPurgedAt)
	}

	// 91 days after the end: every answer goes; status and ticket stay.
	at = ev.Event.EndsAt.Add(91 * day)
	if res, err := e.mod.Sweeper.Sweep(ctx, all, at); err != nil || res.Count != 1 {
		t.Fatalf("answers sweep = %+v, %v", res, err)
	}
	r = get()
	if string(r.Answers) != "{}" || r.AnswersSensitiveEnc != nil || r.AnswersPurgedAt == nil || r.Status != events.RegConfirmed ||
		r.UserID != a.UserID {
		t.Fatalf("after 91 days = %+v", r)
	}
	if tk, ok, err := e.mem.ActiveTicket(ctx, ev.Event.OrgID, r.ID); err != nil || !ok || tk.RegistrationID != r.ID {
		t.Fatalf("the ticket did not survive the purge: %+v, %v, %v", tk, ok, err)
	}
	// Idempotent.
	if res, err := e.mod.Sweeper.Sweep(ctx, all, at); err != nil || res.Count != 0 {
		t.Fatalf("rerun = %+v, %v", res, err)
	}
}
