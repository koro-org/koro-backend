package events_test

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"koro/internal/events"
	"koro/internal/notify"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/queue"
)

// icsProps unfolds a calendar file (RFC 5545 §3.1) and returns its
// properties by name (the first value; parameters dropped).
func icsProps(t *testing.T, b []byte) map[string]string {
	t.Helper()
	s := string(b)
	if !strings.HasSuffix(s, "\r\n") || strings.Contains(strings.ReplaceAll(s, "\r\n", ""), "\n") {
		t.Fatalf("not CRLF lines: %q", s)
	}
	for ln := range strings.SplitSeq(s, "\r\n") {
		if len(ln) > 75 {
			t.Fatalf("line of %d octets: %q", len(ln), ln)
		}
	}
	out := map[string]string{}
	for ln := range strings.SplitSeq(strings.ReplaceAll(s, "\r\n ", ""), "\r\n") {
		name, val, ok := strings.Cut(ln, ":")
		if !ok {
			continue
		}
		name, _, _ = strings.Cut(name, ";")
		if _, seen := out[name]; !seen {
			out[name] = val
		}
	}
	return out
}

// lastMail returns the last e-mail notify handed to the transport.
func (e *env) lastMail(t *testing.T) notify.Email {
	t.Helper()
	sent := e.mailbox.Sent()
	if len(sent) == 0 {
		t.Fatal("no e-mail sent")
	}
	return sent[len(sent)-1]
}

func calendar(t *testing.T, m notify.Email) []byte {
	t.Helper()
	if len(m.Attachments) != 1 || m.Attachments[0].Filename != "etkinlik.ics" {
		t.Fatalf("attachments = %+v", m.Attachments)
	}
	return m.Attachments[0].Data
}

// TestTicketEmail_HasInlineQRAndICS: the ticket mail of a confirmed
// registration carries the QR as an inline CID PNG (referenced by the HTML,
// no data: URI) rendered from the same payload as getMyTicket, the manual
// code as text and an .ics (REQUEST) without the token; the recipient is
// the registrant's address, the dedupe key ticket:<id>:v1 (R-003 §2.5.6).
func TestTicketEmail_HasInlineQRAndICS(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.doorEvent(t)
	r, tk := e.ticketOf(t, adultStudent(t), ev.Event.ID)
	e.deliver(t, domainevent.TopicRegistrationConfirmed, 0)
	m := e.lastMail(t)
	if len(e.mailbox.Sent()) != 1 || m.To != "u"+adultStudent(t).UserID.String()[:8]+"@example.test" ||
		m.Subject != "Biletin hazır: "+ev.Event.Title {
		t.Fatalf("mail = %+v", m)
	}
	png, err := events.NewQRRenderer().PNG(tk.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Inline) != 1 || m.Inline[0].CID != "ticket-qr" || m.Inline[0].ContentType != "image/png" ||
		!bytes.Equal(m.Inline[0].Data, png) {
		t.Fatalf("inline = %+v", m.Inline)
	}
	if !strings.Contains(m.HTML, `src="cid:ticket-qr"`) || strings.Contains(m.HTML, "data:") {
		t.Fatal("the QR is not a CID image")
	}
	code := tk.Ticket.ManualCode[:4] + "-" + tk.Ticket.ManualCode[4:]
	token := strings.TrimPrefix(tk.Payload, events.TicketQRPrefix)
	if !strings.Contains(m.Text, code) || !strings.Contains(m.HTML, code) || !strings.Contains(m.Text, "Ayla") ||
		!strings.Contains(m.Text, "https://koro.test/me/registrations/"+r.Registration.ID.String()) ||
		!strings.Contains(m.Text, "20.11.2026 18:00 – 20:00") {
		t.Fatalf("text = %s", m.Text)
	}
	cal := calendar(t, m)
	for _, part := range []string{m.Text, m.HTML, m.Subject, string(cal)} {
		if strings.Contains(part, token) {
			t.Fatal("the token is in the mail text or the calendar")
		}
	}
	if ct := m.Attachments[0].ContentType; !strings.HasPrefix(ct, "text/calendar;") || !strings.Contains(ct, "method=REQUEST") {
		t.Fatalf("content type %q", ct)
	}
	p := icsProps(t, cal)
	want := map[string]string{
		"METHOD": "REQUEST", "UID": ev.Event.ID.String() + "." + r.Registration.ID.String() + "@koro",
		"DTSTART": "20261120T150000Z", "DTEND": "20261120T170000Z", "SUMMARY": ev.Event.Title, "STATUS": "CONFIRMED",
		"ORGANIZER": "mailto:noreply@koro.test", "ATTENDEE": "mailto:" + m.To,
	}
	for k, v := range want {
		if p[k] != v {
			t.Fatalf("%s = %q, want %q (%q)", k, p[k], v, cal)
		}
	}
	if _, err := strconv.Atoi(p["SEQUENCE"]); err != nil {
		t.Fatalf("SEQUENCE %q", p["SEQUENCE"])
	}
	msgs := e.sender.emails("events.ticket_issued")
	if len(msgs) != 1 || msgs[0].DedupeKey != "ticket:"+r.Registration.ID.String()+":v1" || *msgs[0].UserID != adultStudent(t).UserID ||
		msgs[0].Category != notify.CategoryTransactional {
		t.Fatalf("message = %+v", msgs)
	}
}

// TestTicketEmail_DedupeAcrossRetries: the outbox delivers at least once;
// a second delivery of registration.confirmed sends no second mail (dedupe
// key per ticket version). A reissue mails the new version once; a
// registration cancelled (or erased) before the worker runs gets none; a
// promotion from the waiting list gets its ticket.
func TestTicketEmail_DedupeAcrossRetries(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.doorEvent(t, capacity(2, true))
	r, v1 := e.ticketOf(t, adultStudent(t), ev.Event.ID)
	for range 3 {
		e.deliver(t, domainevent.TopicRegistrationConfirmed, 0)
	}
	if n := len(e.mailbox.Sent()); n != 1 {
		t.Fatalf("%d mails after three deliveries", n)
	}
	v2, err := e.svc.ReissueMyTicket(t.Context(), adultStudent(t), r.Registration.ID)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		e.deliver(t, domainevent.TopicTicketReissued, 0)
	}
	sent := e.mailbox.Sent()
	if len(sent) != 2 || !strings.HasPrefix(sent[1].Subject, "Biletin yenilendi") {
		t.Fatalf("mails = %d", len(sent))
	}
	png, _ := events.NewQRRenderer().PNG(v2.Payload)
	old, _ := events.NewQRRenderer().PNG(v1.Payload)
	if !bytes.Equal(sent[1].Inline[0].Data, png) || bytes.Equal(png, old) {
		t.Fatal("the reissue mail does not carry the new QR")
	}
	// Cancelled before the worker: no ticket mail (the cancellation mail
	// follows from registration.cancelled).
	c := e.mustRegister(t, persona(t, "club_member"), ev.Event.ID, motivation)
	w := e.mustRegister(t, persona(t, "other_club_admin"), ev.Event.ID, motivation)
	if w.Registration.Status != events.RegWaitlisted {
		t.Fatalf("status %s", w.Registration.Status)
	}
	if _, err := e.svc.CancelMyRegistration(t.Context(), persona(t, "club_member"), c.Registration.ID, ""); err != nil {
		t.Fatal(err)
	}
	e.deliver(t, domainevent.TopicRegistrationConfirmed, 1)
	issued := e.sender.emails("events.ticket_issued")
	if len(issued) != 2 || *issued[1].UserID != persona(t, "other_club_admin").UserID {
		t.Fatalf("ticket mails = %+v", issued)
	}
	if !strings.Contains(e.lastMail(t).Text, "Bekleme listesinden") {
		t.Fatal("the promotion is not mentioned")
	}
	// An erased account gets nothing (and the task does not fail).
	e2 := newEnv(t)
	ev2 := e2.doorEvent(t)
	s := newStudent()
	e2.mustRegister(t, s, ev2.Event.ID, motivation)
	e2.users.erased[s.UserID] = true
	e2.deliver(t, domainevent.TopicRegistrationConfirmed, 0)
	if n := len(e2.mailbox.Sent()); n != 0 {
		t.Fatalf("%d mails to an erased account", n)
	}
}

// TestResendTicket_SameTokenRederived_RateLimited: a resend mails the same
// ticket again (the token re-derived: the same QR image) under its own
// dedupe key, through a queued task; only a seat holder resends (409), a
// stranger's registration is 404, a queue outage 503. (The ticket_resend
// limit of 3 per hour is the HTTP test's.)
func TestResendTicket_SameTokenRederived_RateLimited(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.doorEvent(t, capacity(1, true))
	r, tk := e.ticketOf(t, adultStudent(t), ev.Event.ID)
	e.deliver(t, domainevent.TopicRegistrationConfirmed, 0)
	for range 2 {
		if err := e.svc.ResendMyTicket(t.Context(), adultStudent(t), r.Registration.ID); err != nil {
			t.Fatal(err)
		}
	}
	tasks := e.queue.Tasks()
	if len(tasks) != 2 || tasks[0].Type != events.ResendTicketTaskName || tasks[0].Queue != queue.Critical ||
		tasks[0].ID == tasks[1].ID || bytes.Contains(tasks[0].Payload, []byte(strings.TrimPrefix(tk.Payload, "KORO1:"))) {
		t.Fatalf("tasks = %+v", tasks)
	}
	var handle queue.HandlerFunc
	for _, x := range e.mod.Tasks() {
		if x.Name == events.ResendTicketTaskName {
			handle = x.Handle
		}
	}
	for _, task := range tasks {
		for range 2 { // a task retry sends nothing twice
			if err := handle(t.Context(), task.Payload); err != nil {
				t.Fatal(err)
			}
		}
	}
	sent := e.mailbox.Sent()
	if len(sent) != 3 {
		t.Fatalf("%d mails, want the ticket and two resends", len(sent))
	}
	for _, m := range sent[1:] {
		if !bytes.Equal(m.Inline[0].Data, sent[0].Inline[0].Data) {
			t.Fatal("a resend carries another QR")
		}
	}
	keys := map[string]bool{}
	for _, m := range e.sender.emails("events.ticket_issued") {
		keys[m.DedupeKey] = true
	}
	if len(keys) != 3 {
		t.Fatalf("dedupe keys = %v", keys)
	}
	if n := len(e.audit.of("events.ticket.resend")); n != 2 {
		t.Fatalf("%d resend audits", n)
	}
	w := e.mustRegister(t, persona(t, "club_member"), ev.Event.ID, motivation)
	err := e.svc.ResendMyTicket(t.Context(), persona(t, "club_member"), w.Registration.ID)
	wantCode(t, err, 409, "events.invalid_state")
	err = e.svc.ResendMyTicket(t.Context(), persona(t, "club_member"), r.Registration.ID)
	wantNotFound(t, err)
	e.queue.Fail(errors.New("redis down"))
	err = e.svc.ResendMyTicket(t.Context(), adultStudent(t), r.Registration.ID)
	wantCode(t, err, 503, "unavailable")
}

// TestScheduleChanged_SendsICSWithIncrementedSequence: a schedule change
// of a published event (event.schedule_changed) mails every seat holder
// the new time with an .ics of the same UID and a larger SEQUENCE
// (REQUEST); a waitlisted registration gets nothing; a retry of the same
// event sends nothing twice. A cancellation then sends METHOD:CANCEL with
// a larger SEQUENCE still.
func TestScheduleChanged_SendsICSWithIncrementedSequence(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ev := e.doorEvent(t, capacity(2, true))
	r, _ := e.ticketOf(t, adultStudent(t), ev.Event.ID)
	e.ticketOf(t, persona(t, "club_member"), ev.Event.ID)
	w := e.mustRegister(t, persona(t, "other_club_admin"), ev.Event.ID, motivation)
	if w.Registration.Status != events.RegWaitlisted {
		t.Fatalf("status %s", w.Registration.Status)
	}
	e.deliver(t, domainevent.TopicRegistrationConfirmed, 0)
	seq1, _ := strconv.Atoi(icsProps(t, calendar(t, e.mailbox.Sent()[0]))["SEQUENCE"])

	e.clk.Advance(time.Hour)
	cur, err := e.svc.GetEvent(t.Context(), persona(t, "club_admin"), ev.Event.ID)
	if err != nil {
		t.Fatal(err)
	}
	ns, ne := start.Add(24*time.Hour), start.Add(27*time.Hour)
	if _, err := e.svc.UpdateEvent(t.Context(), persona(t, "club_admin"), ev.Event.ID, cur.ETag,
		events.EventPatch{StartsAt: &ns, EndsAt: &ne}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		e.deliver(t, domainevent.TopicEventScheduleChanged, 0)
	}
	upd := e.sender.emails("events.schedule_changed")
	if len(upd) != 2 {
		t.Fatalf("%d schedule mails, want the two seat holders once", len(upd))
	}
	sent := e.mailbox.Sent()
	m := sent[len(sent)-1]
	p := icsProps(t, calendar(t, m))
	seq2, _ := strconv.Atoi(p["SEQUENCE"])
	if seq2 <= seq1 || p["METHOD"] != "REQUEST" || p["DTSTART"] != "20261121T150000Z" || p["DTEND"] != "20261121T180000Z" ||
		!strings.HasPrefix(p["UID"], ev.Event.ID.String()+".") || len(m.Inline) != 0 ||
		!strings.Contains(m.Text, "21.11.2026 18:00 – 21:00") {
		t.Fatalf("SEQUENCE %d → %d, props %v", seq1, seq2, p)
	}
	for _, x := range upd {
		if *x.UserID == persona(t, "other_club_admin").UserID {
			t.Fatal("a waitlisted registration got the schedule mail")
		}
	}
	// The registrant cancels: the calendar entry is cancelled.
	if _, err := e.svc.CancelMyRegistration(t.Context(), adultStudent(t), r.Registration.ID, ""); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		e.deliver(t, domainevent.TopicRegistrationCancelled, 0)
	}
	cx := e.sender.emails("events.cancelled")
	if len(cx) != 1 || *cx[0].UserID != adultStudent(t).UserID {
		t.Fatalf("cancellation mails = %+v", cx)
	}
	sent = e.mailbox.Sent()
	m = sent[len(sent)-1]
	p = icsProps(t, calendar(t, m))
	seq3, _ := strconv.Atoi(p["SEQUENCE"])
	if p["METHOD"] != "CANCEL" || p["STATUS"] != "CANCELLED" || seq3 <= seq2 ||
		p["UID"] != ev.Event.ID.String()+"."+r.Registration.ID.String()+"@koro" ||
		!strings.Contains(m.Attachments[0].ContentType, "method=CANCEL") ||
		!strings.Contains(m.Text, "Kaydını iptal ettin") {
		t.Fatalf("cancel = %v, %q", p, m.Text)
	}
	// An organiser's cancellation of a pending application (no ticket ever)
	// sends no calendar mail.
	man := e.regEvent(t, func(in *events.EventInput) { in.RegistrationMode = events.ModeManual }, motivationField)
	pa := e.mustRegister(t, persona(t, "club_member"), man.Event.ID, motivation)
	if _, err := e.svc.CancelEventRegistration(t.Context(), persona(t, "club_admin"), man.Event.ID, pa.Registration.ID,
		"Kontenjan doldu"); err != nil {
		t.Fatal(err)
	}
	e.deliver(t, domainevent.TopicRegistrationCancelled, 1)
	if n := len(e.sender.emails("events.cancelled")); n != 1 {
		t.Fatalf("%d cancellation mails", n)
	}
}

// TestICS_EscapingAndFolding: TEXT values are escaped (RFC 5545 §3.3.11),
// long lines folded at 75 octets without splitting a UTF-8 sequence, and
// the sequence follows the event row (seconds since its creation).
func TestICS_EscapingAndFolding(t *testing.T) {
	t.Parallel()
	ev := events.Event{
		ID: [16]byte{1}, Title: "Veri; Bilimi, Atölyesi\\ " + strings.Repeat("çğıöşü", 20) + "\r\nBCC:x@y", Format: events.FormatInPerson,
		StartsAt: start, EndsAt: start.Add(time.Hour), CreatedAt: t0, UpdatedAt: t0.Add(90 * time.Second),
	}
	b := events.ICS("REQUEST", ev, [16]byte{2}, events.ICSSequence(ev), "a@example.test")
	p := icsProps(t, b)
	if !strings.HasPrefix(p["SUMMARY"], `Veri\; Bilimi\, Atölyesi\\ çğıöşü`) || strings.Contains(string(b), "\nBCC") ||
		!strings.HasSuffix(p["SUMMARY"], `\nBCC:x@y`) || p["SEQUENCE"] != "90" {
		t.Fatalf("props = %v", p)
	}
	if !bytes.HasPrefix(b, []byte("BEGIN:VCALENDAR\r\n")) || !bytes.HasSuffix(b, []byte("END:VCALENDAR\r\n")) {
		t.Fatalf("frame = %q", b)
	}
	// An address that would break the line is left out.
	if bad := events.ICS("CANCEL", ev, [16]byte{2}, 1, "x@y.test\r\nATTENDEE:mailto:z@w"); strings.Contains(string(bad), "z@w") {
		t.Fatalf("injected attendee: %q", bad)
	}
}

// TestTicketTemplates_Exist: every BE-031 mail renders in both locales
// through notify with the data the service sends.
func TestTicketTemplates_Exist(t *testing.T) {
	t.Parallel()
	tpl, err := embeddedTemplates()
	if err != nil {
		t.Fatal(err)
	}
	data := map[string]any{"name": "Ayla", "event_title": "Veri Bilimi", "org_name": "Kulüp", "when": "20.11.2026 18:00 – 20:00",
		"location": "A-101", "ticket_url": "https://koro.test/me/registrations/x", "manual_code": "7KQ2-M9TX", "promoted": false,
		"by": "org"}
	for _, id := range events.TicketTemplates {
		meta, ok := tpl.Meta(id)
		if !ok || !meta.Supports(notify.ChannelEmail) || meta.Category != notify.CategoryTransactional {
			t.Fatalf("%s: meta %+v, %v", id, meta, ok)
		}
		for _, loc := range []string{"tr", "en"} {
			r, err := tpl.Render(id, loc, notify.RenderInput{Data: data})
			if err != nil || strings.Contains(r.HTML, "data:") || strings.Contains(r.Text, "<no value>") {
				t.Fatalf("%s/%s: %v", id, loc, err)
			}
		}
	}
}
