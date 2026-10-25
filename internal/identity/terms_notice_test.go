package identity_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/platform/queue"
)

// task returns identity's task handler named typ.
func (e *env) task(t *testing.T, typ string) queue.Task {
	t.Helper()
	for _, task := range e.mod.Tasks() {
		if task.Name == typ {
			return task
		}
	}
	t.Fatalf("identity has no task %s", typ)
	return queue.Task{}
}

// runTasks runs the queued tasks of type typ (the worker) and returns how
// many it ran.
func (e *env) runTasks(t *testing.T, typ string) int {
	t.Helper()
	task, n := e.task(t, typ), 0
	for _, x := range e.queued(typ) {
		if err := task.Handle(workerCtx(t), x.Payload); err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
		n++
	}
	return n
}

// queued returns the queued tasks of type typ.
func (e *env) queued(typ string) []queue.Enqueued {
	var out []queue.Enqueued
	for _, x := range e.q.Tasks() {
		if x.Type == typ {
			out = append(out, x)
		}
	}
	return out
}

// TestTermsNotice_TellsTheReadersOfTheVersion (BE-124): a published terms
// of use version queues the fan-out after the commit; the fan-out queues
// one notice per active account the version is in force for (here: the
// student audience in tr-TR), deduplicated per (version, reader); each
// notice is an in-app notification (fixed text, the version's ID only,
// the acceptance link) and a transactional e-mail. A reader who accepted
// meanwhile gets nothing; a retry sends nothing twice.
func TestTermsNotice_TellsTheReadersOfTheVersion(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	ada := e.activeUser(t, "ada@example.com")
	bea := e.activeUser(t, "bea@example.com")
	// Other readers: a company account, an English-speaking student and a
	// student who never confirmed the address.
	for _, u := range []identity.User{
		{Persona: identity.PersonaCompany, Locale: identity.LocaleTR, Status: identity.StatusActive},
		{Persona: identity.PersonaStudent, Locale: identity.LocaleEN, Status: identity.StatusActive},
		{Persona: identity.PersonaStudent, Locale: identity.LocaleTR, Status: identity.StatusPendingEmail},
	} {
		u.ID = uuid.NewV7()
		u.Email, u.EmailNormalized = u.ID.String()+"@example.com", u.ID.String()+"@example.com"
		u.FirstName, u.LastName = "Cem", "Ak"
		if u.Persona == identity.PersonaStudent {
			u.BirthDate = ada.BirthDate
		}
		if err := e.store.InsertUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	e.terms.inForceFor = func(k consent.NoticeKey) bool {
		return k.Audience == consent.AudienceStudent && k.Locale == consent.LocaleTR
	}
	v2 := newTerms("2026.11.01")
	e.terms.publish(v2)
	before := len(e.mail.sent())

	e.mod.TermsPublished(ctx, v2.ID)
	if fan := e.queued(identity.NotifyTermsPublishedTaskName); len(fan) != 1 || fan[0].ID != "identity.notify_terms_published:"+v2.ID.String() {
		t.Fatalf("fan-out tasks = %+v", fan)
	}
	if n := e.runTasks(t, identity.NotifyTermsPublishedTaskName); n != 1 {
		t.Fatalf("ran %d fan-out tasks", n)
	}
	var readers []uuid.UUID
	for _, x := range e.queued(identity.SendTermsNoticeTaskName) {
		var p struct {
			UserID   uuid.UUID `json:"user_id"`
			NoticeID uuid.UUID `json:"notice_id"`
		}
		if err := json.Unmarshal(x.Payload, &p); err != nil || p.NoticeID != v2.ID ||
			x.ID != "identity.terms_notice:"+v2.ID.String()+":"+p.UserID.String() {
			t.Fatalf("notice task %+v (%s)", x, x.Payload)
		}
		readers = append(readers, p.UserID)
	}
	slices.SortFunc(readers, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	want := []uuid.UUID{ada.ID, bea.ID}
	slices.SortFunc(want, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	if !slices.Equal(readers, want) {
		t.Fatalf("notified %v, want the two active tr-TR students %v (not the company, the en-US student or the unconfirmed account)", readers, want)
	}
	// A retry of the fan-out queues nothing new (task IDs).
	if e.runTasks(t, identity.NotifyTermsPublishedTaskName); len(e.queued(identity.SendTermsNoticeTaskName)) != 2 {
		t.Fatal("a retried fan-out queued the notices twice")
	}

	// bea accepts before her notice is delivered.
	beaActor := authz.Actor{Kind: authz.KindUser, UserID: bea.ID}
	if _, err := e.svc.AcceptTerms(ctx, beaActor, identity.TermsAcceptanceInput{NoticeVersionID: v2.ID, Client: identity.ClientWeb}); err != nil {
		t.Fatal(err)
	}
	if n := e.runTasks(t, identity.SendTermsNoticeTaskName); n != 2 {
		t.Fatalf("ran %d notice tasks", n)
	}
	msgs := e.mail.sent()[before:]
	if len(msgs) != 2 {
		t.Fatalf("messages = %+v, want ada's in-app notification and e-mail", msgs)
	}
	for _, m := range msgs {
		if m.UserID == nil || *m.UserID != ada.ID || m.Template != "identity.terms_updated" || m.Category != notify.CategoryTransactional ||
			m.Locale != identity.LocaleTR || !strings.HasPrefix(m.DedupeKey, "identity.terms_notice:"+v2.ID.String()+":"+ada.ID.String()+":") {
			t.Fatalf("message %+v", m)
		}
		switch m.Channel {
		case notify.ChannelInApp:
			if m.ToEmail != "" || m.LinkPath != notify.MyTermsLink() || len(m.Data) != 1 || m.Data["notice_version_id"] != v2.ID.String() {
				t.Fatalf("in-app notification %+v", m)
			}
		case notify.ChannelEmail:
			if m.ToEmail != ada.Email || m.Data["name"] != ada.FirstName || m.Data["version"] != "2026.11.01" ||
				m.Data["link"] != "https://app.koro.test/me/terms" {
				t.Fatalf("e-mail %+v", m)
			}
		default:
			t.Fatalf("channel %s", m.Channel)
		}
	}

	// A superseded version tells nobody: the next one is in force.
	v3 := newTerms("2026.12.01")
	e.terms.publish(v3)
	e.q.Reset()
	fanOut := e.task(t, identity.NotifyTermsPublishedTaskName)
	if err := fanOut.Handle(workerCtx(t), []byte(`{"notice_id":"`+v2.ID.String()+`"}`)); err != nil {
		t.Fatal(err)
	}
	if err := fanOut.Handle(workerCtx(t), []byte(`{}`)); !queue.IsPermanent(err) {
		t.Fatalf("payload without a notice = %v, want a permanent error", err)
	}
	if q := e.queued(identity.SendTermsNoticeTaskName); len(q) != 0 {
		t.Fatalf("a superseded version queued %d notices", len(q))
	}
}
