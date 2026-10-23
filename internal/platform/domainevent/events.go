package domainevent

import "uuid"

// Topics of the events module (01 §6.2). Owner: events tickets.
const (
	TopicEventCancelled         = "event.cancelled"
	TopicEventScheduleChanged   = "event.schedule_changed"
	TopicRegistrationSubmitted  = "registration.submitted"
	TopicRegistrationConfirmed  = "registration.confirmed"
	TopicRegistrationWaitlisted = "registration.waitlisted"
	TopicRegistrationRejected   = "registration.rejected"
	TopicRegistrationCancelled  = "registration.cancelled"
	TopicRegistrationNoShow     = "registration.no_show"
	TopicTicketReissued         = "ticket.reissued"
	TopicCheckinRecorded        = "checkin.recorded"
	TopicCheckinVoided          = "checkin.voided"
	TopicAttendanceFinalized    = "attendance.finalized"
)

var _ = register(
	spec("events", Outbox, EventCancelled{}),
	spec("events", Outbox, EventScheduleChanged{}),
	spec("events", Outbox, RegistrationSubmitted{}),
	spec("events", Outbox, RegistrationConfirmed{}),
	spec("events", Outbox, RegistrationWaitlisted{}),
	spec("events", Outbox, RegistrationRejected{}),
	spec("events", Outbox, RegistrationCancelled{}),
	spec("events", Outbox, RegistrationNoShow{}),
	spec("events", Outbox, TicketReissued{}),
	spec("events", Outbox, CheckinRecorded{}),
	spec("events", Outbox, CheckinVoided{}),
	spec("events", Outbox, AttendanceFinalized{}),
)

// EventCancelled: registrations are cancelled, linked offerings too.
type EventCancelled struct {
	EventID uuid.UUID `json:"event_id"`
	OrgID   uuid.UUID `json:"org_id"`
}

// Topic implements Payload.
func (EventCancelled) Topic() string { return TopicEventCancelled }

// EventScheduleChanged: confirmed attendees get an update (.ics SEQUENCE+1).
// Fields lists the changed field names (starts_at, ends_at, location...).
type EventScheduleChanged struct {
	EventID uuid.UUID `json:"event_id"`
	OrgID   uuid.UUID `json:"org_id"`
	Fields  []string  `json:"fields"`
}

// Topic implements Payload.
func (EventScheduleChanged) Topic() string { return TopicEventScheduleChanged }

// RegistrationSubmitted: an application was received.
type RegistrationSubmitted struct {
	RegistrationID uuid.UUID `json:"registration_id"`
	EventID        uuid.UUID `json:"event_id"`
	OrgID          uuid.UUID `json:"org_id"`
	UserID         uuid.UUID `json:"user_id"`
}

// Topic implements Payload.
func (RegistrationSubmitted) Topic() string { return TopicRegistrationSubmitted }

// RegistrationConfirmed: the ticket e-mail is sent (CID QR + .ics).
type RegistrationConfirmed struct {
	RegistrationID uuid.UUID `json:"registration_id"`
	EventID        uuid.UUID `json:"event_id"`
	OrgID          uuid.UUID `json:"org_id"`
	UserID         uuid.UUID `json:"user_id"`
	Sensitive      bool      `json:"sensitive"`
	Promoted       bool      `json:"promoted"`
	TicketVersion  int       `json:"ticket_version"`
}

// Topic implements Payload.
func (RegistrationConfirmed) Topic() string { return TopicRegistrationConfirmed }

// RegistrationWaitlisted: the registration is on the waiting list.
type RegistrationWaitlisted struct {
	RegistrationID uuid.UUID `json:"registration_id"`
	EventID        uuid.UUID `json:"event_id"`
	UserID         uuid.UUID `json:"user_id"`
}

// Topic implements Payload.
func (RegistrationWaitlisted) Topic() string { return TopicRegistrationWaitlisted }

// RegistrationRejected: the application was rejected (the reason text never
// enters the event).
type RegistrationRejected struct {
	RegistrationID uuid.UUID `json:"registration_id"`
	EventID        uuid.UUID `json:"event_id"`
	UserID         uuid.UUID `json:"user_id"`
}

// Topic implements Payload.
func (RegistrationRejected) Topic() string { return TopicRegistrationRejected }

// RegistrationCancelled: By is user | org | system.
type RegistrationCancelled struct {
	RegistrationID uuid.UUID `json:"registration_id"`
	EventID        uuid.UUID `json:"event_id"`
	UserID         uuid.UUID `json:"user_id"`
	By             string    `json:"by"`
	Sensitive      bool      `json:"sensitive"`
}

// Topic implements Payload.
func (RegistrationCancelled) Topic() string { return TopicRegistrationCancelled }

// RegistrationNoShow: a confirmed registration without check-in after
// attendance was finalised.
type RegistrationNoShow struct {
	RegistrationID uuid.UUID `json:"registration_id"`
	EventID        uuid.UUID `json:"event_id"`
	UserID         uuid.UUID `json:"user_id"`
	Sensitive      bool      `json:"sensitive"`
}

// Topic implements Payload.
func (RegistrationNoShow) Topic() string { return TopicRegistrationNoShow }

// TicketReissued: a new QR e-mail is sent.
type TicketReissued struct {
	RegistrationID uuid.UUID `json:"registration_id"`
	TicketID       uuid.UUID `json:"ticket_id"`
	TicketVersion  int       `json:"ticket_version"`
}

// Topic implements Payload.
func (TicketReissued) Topic() string { return TopicTicketReissued }

// CheckinRecorded: an attendee was checked in. OrgID is the organiser of
// the event (talent: an organiser earns nothing at their own event; BE-085).
type CheckinRecorded struct {
	CheckinID      uuid.UUID `json:"checkin_id"`
	RegistrationID uuid.UUID `json:"registration_id"`
	EventID        uuid.UUID `json:"event_id"`
	OrgID          uuid.UUID `json:"org_id"`
	UserID         uuid.UUID `json:"user_id"`
	StaffUserID    uuid.UUID `json:"staff_user_id"`
	Sensitive      bool      `json:"sensitive"`
}

// Topic implements Payload.
func (CheckinRecorded) Topic() string { return TopicCheckinRecorded }

// CheckinVoided: a check-in was voided.
type CheckinVoided struct {
	CheckinID      uuid.UUID `json:"checkin_id"`
	RegistrationID uuid.UUID `json:"registration_id"`
	UserID         uuid.UUID `json:"user_id"`
}

// Topic implements Payload.
func (CheckinVoided) Topic() string { return TopicCheckinVoided }

// AttendanceFinalized: FinalizedBy is user | system.
type AttendanceFinalized struct {
	EventID     uuid.UUID `json:"event_id"`
	FinalizedBy string    `json:"finalized_by"`
}

// Topic implements Payload.
func (AttendanceFinalized) Topic() string { return TopicAttendanceFinalized }
