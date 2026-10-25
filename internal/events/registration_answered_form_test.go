package events_test

import (
	"strings"
	"testing"

	"koro/internal/events"
)

// TestGetMyRegistration_AnsweredFormVersionLabels: getMyRegistration shows
// the labels of the form version the registrant answered, not the current
// one's, after the organiser republishes the form (BE-122); the list view
// carries no form.
func TestGetMyRegistration_AnsweredFormVersionLabels(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.regEvent(t, nil, motivationField)
	a := persona(t, "student")
	r := e.mustRegister(t, a, ev.Event.ID, motivation)

	renamed := strings.Replace(motivationField, "Neden katılmak istiyorsun?", "Beklentin nedir?", 1)
	e.putDraft(t, "club_admin", ev.Event.ID, formJSON(renamed))
	if v := e.publishForm(t, ev.Event.ID, ""); v.Version.VersionNo != 2 {
		t.Fatalf("republished version = %d, want 2", v.Version.VersionNo)
	}

	got := e.reg(t, a, r.Registration.ID)
	f := got.AnsweredForm
	if f == nil || f.VersionNo != 1 || len(f.Schema.Fields) != 1 || f.Schema.Fields[0].Label["tr"] != "Neden katılmak istiyorsun?" {
		t.Fatalf("answered form = %+v", f)
	}
	if got.Registration.FormVersionID == nil {
		t.Fatal("form_version_id missing")
	}
	// A new registrant answers (and reads) the current version.
	b := persona(t, "student_minor")
	rb := e.mustRegister(t, b, ev.Event.ID, motivation)
	if f := e.reg(t, b, rb.Registration.ID).AnsweredForm; f == nil || f.VersionNo != 2 || f.Schema.Fields[0].Label["tr"] != "Beklentin nedir?" {
		t.Fatalf("current answered form = %+v", f)
	}
	// Only getMyRegistration carries it.
	page, err := e.svc.ListMyRegistrations(t.Context(), a, events.MyRegistrationQuery{})
	if err != nil || len(page.Items) != 1 || page.Items[0].AnsweredForm != nil {
		t.Fatalf("list = %+v, %v", page.Items, err)
	}
}
