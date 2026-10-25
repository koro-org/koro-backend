package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"uuid"

	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/orgs"
	"koro/internal/platform/applink"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/queue"
)

// The ticket e-mails (BE-031; ADR-0013, R-003 §2.5.6, 01 §6.2, §8): the
// worker sends them, never the request (notify refuses a request context).
//
//   - registration.confirmed (a registration, a manual approval or a
//     promotion from the waiting list) → the ticket: the QR as an inline
//     CID PNG (never a data: URI), the manual code as text, the .ics
//     (REQUEST); dedupe key ticket:<registration_id>:v<version>, so an
//     outbox retry never sends a second mail;
//   - ticket.reissued → the new ticket (new version, new key);
//   - resendMyTicket → the same token re-derived (mechanism A task; its own
//     dedupe key per request, rate limited by ticket_resend);
//   - event.schedule_changed → every seat holder gets the update with the
//     .ics (REQUEST, larger SEQUENCE);
//   - registration.cancelled → the .ics CANCEL (only when the registration
//     ever had a ticket, i.e. a calendar entry).
//
// What a mail carries: the event's title, time (Europe/Istanbul), venue and
// organiser, the registrant's first name, the manual code and a link to
// the ticket page. No marketing content (6563), no token in a URL, the .ics
// or a log line; the recipient address is never logged (notify masks it).
// A registration cancelled, superseded or erased before the worker runs
// gets no ticket mail (the handler re-reads the rows).

// Worker names of BE-031 (01 §8: <module>.<verb>_<object>).
const (
	// SendTicketEmailTaskName consumes registration.confirmed (critical).
	SendTicketEmailTaskName = "events.send_ticket_email"
	// SendReissuedTicketTaskName consumes ticket.reissued (critical).
	SendReissuedTicketTaskName = "events.send_reissued_ticket_email"
	// ResendTicketTaskName is the task resendMyTicket enqueues (critical).
	ResendTicketTaskName = "events.resend_ticket_email"
	// SendScheduleUpdateTaskName consumes event.schedule_changed.
	SendScheduleUpdateTaskName = "events.send_schedule_update"
	// SendCancellationEmailTaskName consumes registration.cancelled.
	SendCancellationEmailTaskName = "events.send_cancellation_email"
)

// E-mail templates (internal/notify/templates/events).
const (
	templateTicketIssued    = "events.ticket_issued"
	templateTicketReissued  = "events.ticket_reissued"
	templateScheduleChanged = "events.schedule_changed"
	templateCancelledEmail  = "events.cancelled"
)

// Parts of a ticket mail.
const (
	qrCID       = "ticket-qr"
	icsFilename = "etkinlik.ics"
	// scheduleTimeout bounds one attempt of the schedule update (one mail
	// per seat holder; a retry skips the sent ones by their dedupe key).
	scheduleTimeout = 15 * time.Minute
)

// ticketMail is one ticket e-mail to send.
type ticketMail struct {
	template string
	regID    uuid.UUID
	eventID  uuid.UUID
	orgID    *uuid.UUID // the org of the outbox event, when it names one
	version  int        // the ticket version the mail is for
	promoted bool
	dedupe   string
}

// mailReady fails a mail task (retried) while its dependencies are not wired.
func (s *service) mailReady() error {
	switch {
	case s.d.Sender == nil:
		return errors.New("events: notify is not wired")
	case s.d.Contacts == nil:
		return errors.New("events: identity contacts are not wired")
	case s.d.QR == nil:
		return errors.New("events: the QR renderer is not wired")
	}
	return nil
}

// handleConfirmed is events.send_ticket_email (registration.confirmed).
func (s *service) handleConfirmed(ctx context.Context, env domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.RegistrationConfirmed](env)
	if err != nil {
		return queue.Permanent(err)
	}
	org := p.OrgID
	return s.sendTicket(ctx, ticketMail{template: templateTicketIssued, regID: p.RegistrationID, eventID: p.EventID,
		orgID: &org, version: p.TicketVersion, promoted: p.Promoted, dedupe: ticketDedupe(p.RegistrationID, p.TicketVersion)})
}

// handleReissued is events.send_reissued_ticket_email (ticket.reissued).
// The event names the ticket; its registration is read through the
// ticket's event and that event's org (06 H-38).
func (s *service) handleReissued(ctx context.Context, env domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.TicketReissued](env)
	if err != nil {
		return queue.Permanent(err)
	}
	t, err := s.d.Store.GetTicket(ctx, p.TicketID)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil // erased with its registration
	case err != nil:
		return err
	case t.RegistrationID != p.RegistrationID || t.Version != p.TicketVersion:
		return queue.Permanent(errors.New("events: ticket.reissued does not match its ticket"))
	}
	return s.sendTicket(ctx, ticketMail{template: templateTicketReissued, regID: t.RegistrationID, eventID: t.EventID,
		version: t.Version, dedupe: ticketDedupe(t.RegistrationID, t.Version)})
}

// resendTask is the payload of events.resend_ticket_email: IDs only.
type resendTask struct {
	RegistrationID uuid.UUID `json:"registration_id"`
	EventID        uuid.UUID `json:"event_id"`
	OrgID          uuid.UUID `json:"org_id"`
	TicketVersion  int       `json:"ticket_version"`
	Ref            uuid.UUID `json:"ref"` // one per request: the dedupe key of its mail
}

// handleResend is events.resend_ticket_email: the same ticket again (the
// token re-derived, R-003 §2.5.4), deduplicated per request.
func (s *service) handleResend(ctx context.Context, payload []byte) error {
	var t resendTask
	if err := json.Unmarshal(payload, &t); err != nil || t.RegistrationID == uuid.Nil() || t.Ref == uuid.Nil() {
		return queue.Permanent(fmt.Errorf("events: resend payload: %w", errors.Join(err, errors.New("missing ids"))))
	}
	org := t.OrgID
	return s.sendTicket(ctx, ticketMail{template: templateTicketIssued, regID: t.RegistrationID, eventID: t.EventID,
		orgID: &org, version: t.TicketVersion,
		dedupe: ticketDedupe(t.RegistrationID, t.TicketVersion) + ":resend:" + t.Ref.String()})
}

// ticketDedupe is the dedupe key of a ticket mail (notify.Message).
func ticketDedupe(registrationID uuid.UUID, version int) string {
	return fmt.Sprintf("ticket:%s:v%d", registrationID, version)
}

// sendTicket sends the ticket mail m: the registration must still hold a
// seat and its active ticket be version m.version, else nothing is sent
// (a later mail covers the newer state).
func (s *service) sendTicket(ctx context.Context, m ticketMail) error {
	if err := s.mailReady(); err != nil {
		return err
	}
	e, r, ok, err := s.mailRegistration(ctx, m.eventID, m.orgID, m.regID)
	if err != nil || !ok || !r.Status.Counted() {
		return err
	}
	t, ok, err := s.d.Store.ActiveTicket(ctx, e.OrgID, r.ID)
	if err != nil || !ok || t.Version != m.version {
		return err
	}
	token, err := s.ticketToken(t)
	if errors.Is(err, errTicketKey) {
		s.d.Log.LogAttrs(ctx, slog.LevelError, "ticket key retired; the ticket mail is not sent",
			slog.String("registration_id", r.ID.String()), slog.String("key_id", t.KeyID), slog.Bool("alarm", true))
		return nil
	}
	if err != nil {
		return err
	}
	png, err := s.d.QR.PNG(TicketPayload(token))
	if err != nil {
		return err
	}
	c, ok, err := s.contact(ctx, r.UserID)
	if err != nil || !ok {
		return err
	}
	o, err := s.org(ctx, e.OrgID)
	if err != nil {
		return err
	}
	data := s.mailData(e, o, r, c)
	data["manual_code"], data["promoted"] = displayManualCode(t.ManualCode), m.promoted
	cal := buildICS(s.icsOf(e, o, r, c, icsMethodRQ, icsSequence(e)))
	return s.email(ctx, r, c, m.template, data, []notify.InlineAttachment{{CID: qrCID, ContentType: "image/png", Data: png}},
		notify.Attachment{Filename: icsFilename, ContentType: icsContentType(icsMethodRQ), Data: cal}, m.dedupe)
}

// handleScheduleChanged is events.send_schedule_update
// (event.schedule_changed): every seat holder gets the new time and venue
// with the .ics (REQUEST, larger SEQUENCE), batch by batch; the dedupe key
// is per outbox event and registration, so a retry skips the sent ones.
func (s *service) handleScheduleChanged(ctx context.Context, env domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.EventScheduleChanged](env)
	if err != nil {
		return queue.Permanent(err)
	}
	if err := s.mailReady(); err != nil {
		return err
	}
	e, err := s.d.Store.GetEvent(ctx, p.EventID)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil
	case err != nil:
		return err
	case e.OrgID != p.OrgID || e.Status != StatusPublished:
		return nil // another org's event, or cancelled since (the cancellation mail follows)
	}
	o, err := s.org(ctx, e.OrgID)
	if err != nil {
		return err
	}
	seq, sent := icsSequence(e), 0
	for after := uuid.Nil(); ; {
		rows, err := s.d.Store.SeatHolders(ctx, e.OrgID, e.ID, after, s.batch)
		if err != nil {
			return err
		}
		for _, r := range rows {
			c, ok, err := s.contact(ctx, r.UserID)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			cal := buildICS(s.icsOf(e, o, r, c, icsMethodRQ, seq))
			if err := s.email(ctx, r, c, templateScheduleChanged, s.mailData(e, o, r, c), nil,
				notify.Attachment{Filename: icsFilename, ContentType: icsContentType(icsMethodRQ), Data: cal},
				"schedule:"+env.ID.String()+":"+r.ID.String()); err != nil {
				return err
			}
			sent++
		}
		if len(rows) < s.batch {
			break
		}
		after = rows[len(rows)-1].ID
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "schedule update sent", slog.String("event_id", e.ID.String()),
		slog.Int("count", sent))
	return nil
}

// handleCancelledEmail is events.send_cancellation_email
// (registration.cancelled): a registration that had a ticket (a calendar
// entry) gets the .ics CANCEL with the reason kind (own, organiser, event
// cancelled). Its dedupe key is per registration (cancelled once).
func (s *service) handleCancelledEmail(ctx context.Context, env domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.RegistrationCancelled](env)
	if err != nil {
		return queue.Permanent(err)
	}
	if err := s.mailReady(); err != nil {
		return err
	}
	e, r, ok, err := s.mailRegistration(ctx, p.EventID, nil, p.RegistrationID)
	if err != nil || !ok || r.Status != RegCancelled {
		return err
	}
	tickets, err := s.d.Store.TicketsOfRegistration(ctx, e.OrgID, r.ID)
	if err != nil || len(tickets) == 0 {
		return err // never confirmed: no calendar entry to remove
	}
	c, ok, err := s.contact(ctx, r.UserID)
	if err != nil || !ok {
		return err
	}
	o, err := s.org(ctx, e.OrgID)
	if err != nil {
		return err
	}
	data := s.mailData(e, o, r, c)
	data["by"] = p.By
	cal := buildICS(s.icsOf(e, o, r, c, icsMethodCX, icsSequence(e)+1))
	return s.email(ctx, r, c, templateCancelledEmail, data, nil,
		notify.Attachment{Filename: icsFilename, ContentType: icsContentType(icsMethodCX), Data: cal},
		"cancel:"+r.ID.String())
}

// mailRegistration reads the event and the registration of a mail; false
// when either is gone (deleted, erased) or the event is not orgID's.
func (s *service) mailRegistration(ctx context.Context, eventID uuid.UUID, orgID *uuid.UUID, regID uuid.UUID) (Event, Registration, bool, error) {
	e, err := s.d.Store.GetEvent(ctx, eventID)
	switch {
	case errors.Is(err, ErrNotFound):
		return Event{}, Registration{}, false, nil
	case err != nil:
		return Event{}, Registration{}, false, err
	case orgID != nil && *orgID != e.OrgID:
		return Event{}, Registration{}, false, nil
	}
	r, err := s.d.Store.GetRegistration(ctx, e.OrgID, e.ID, regID)
	switch {
	case errors.Is(err, ErrNotFound):
		return Event{}, Registration{}, false, nil
	case err != nil:
		return Event{}, Registration{}, false, err
	}
	return e, r, true, nil
}

// contact is the registrant's address; false for an erased or unknown
// account (nothing to send).
func (s *service) contact(ctx context.Context, userID uuid.UUID) (identity.UserContact, bool, error) {
	c, err := s.d.Contacts.Contact(ctx, userID)
	switch {
	case errors.Is(err, identity.ErrNotFound):
		return identity.UserContact{}, false, nil
	case err != nil:
		return identity.UserContact{}, false, err
	}
	return c, true, nil
}

// mailData is the template data shared by the event mails.
func (s *service) mailData(e Event, o orgs.OrgSummary, r Registration, c identity.UserContact) map[string]any {
	return map[string]any{
		"name": c.FirstName, "event_title": e.Title, "org_name": o.Name,
		"when": whenText(e), "location": locationText(e),
		"ticket_url": s.ticketURL(r),
	}
}

// ticketURL is the e-mail link of the registrant's own registration page
// (the catalog form /me/registrations/{registration_id}).
func (s *service) ticketURL(r Registration) string {
	return applink.Page(s.d.LinkBaseURL, notify.MyRegistrationLink(r.ID).String())
}

// icsOf is the calendar entry of registration r.
func (s *service) icsOf(e Event, o orgs.OrgSummary, r Registration, c identity.UserContact, method string, seq int) icsEvent {
	return icsEvent{
		Method: method, EventID: e.ID, RegistrationID: r.ID, Sequence: seq, Stamp: s.now(),
		Start: e.StartsAt, End: e.EndsAt, Summary: e.Title, Location: locationText(e),
		Description: o.Name, URL: s.ticketURL(r),
		OrganizerName: o.Name, OrganizerEmail: s.d.ICSOrganizer, AttendeeEmail: c.Email,
	}
}

// email sends one transactional e-mail of the registration's owner. A
// permanent failure (invalid message, rejected address) is logged and
// dropped; anything else is retried. The address is never logged.
func (s *service) email(ctx context.Context, r Registration, c identity.UserContact, tmpl string, data map[string]any,
	inline []notify.InlineAttachment, cal notify.Attachment, dedupe string) error {
	to := r.UserID
	_, err := s.d.Sender.Send(ctx, notify.Message{
		UserID: &to, ToEmail: c.Email, Channel: notify.ChannelEmail, Category: notify.CategoryTransactional,
		Template: tmpl, Locale: c.Locale, Data: data, Inline: inline, Attachments: []notify.Attachment{cal},
		DedupeKey: dedupe,
	})
	if err != nil && notify.IsPermanent(err) {
		s.d.Log.LogAttrs(ctx, slog.LevelError, "event mail not deliverable", slog.String("template", tmpl),
			slog.String("registration_id", r.ID.String()), slog.String("err", err.Error()))
		return nil
	}
	return err
}

// whenText is the event's time in Europe/Istanbul ("20.11.2026 18:00 –
// 20:00"; another end day is written in full).
func whenText(e Event) string {
	loc := istanbul()
	from, to := e.StartsAt.In(loc), e.EndsAt.In(loc)
	end := to.Format("15:04")
	if from.Format("20060102") != to.Format("20060102") {
		end = to.Format("02.01.2006 15:04")
	}
	return from.Format("02.01.2006 15:04") + " – " + end
}

// locationText is the venue line: name, address and city of an in-person
// or hybrid event, "online" for an online one (the link is in Koro only).
func locationText(e Event) string {
	if e.Format == FormatOnline {
		return "Online"
	}
	var parts []string
	for _, p := range []*string{e.LocationName, e.Address, e.City} {
		if p != nil && strings.TrimSpace(*p) != "" {
			parts = append(parts, strings.TrimSpace(*p))
		}
	}
	return strings.Join(parts, ", ")
}
