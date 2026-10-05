package events

import (
	"context"
	"time"
	"uuid"
)

// CheckinMethod is event_checkins.method.
type CheckinMethod string

// How a ticket was presented at the door (02 §11.8).
const (
	MethodQR         CheckinMethod = "qr"
	MethodManualCode CheckinMethod = "manual_code"
)

// Valid reports whether m is a method of the CHECK constraint.
func (m CheckinMethod) Valid() bool { return m == MethodQR || m == MethodManualCode }

// Checkin is one event_checkins row (02 §11.8): personal data (who entered
// when, which staff member scanned). event_id and org_id come from the
// registration row (06 H-38). A voided check-in keeps its row (voided_at,
// voided_by and void_reason together); at most one valid check-in per
// registration (event_checkins_one_active_uq). client_scan_id is unique
// over the table; the service looks it up together with the event and the
// staff member (T-137).
type Checkin struct {
	ID             uuid.UUID     `gorm:"type:uuid;column:id;primaryKey"`
	EventID        uuid.UUID     `gorm:"type:uuid;column:event_id"`
	OrgID          uuid.UUID     `gorm:"type:uuid;column:org_id"`
	RegistrationID uuid.UUID     `gorm:"type:uuid;column:registration_id"`
	TicketID       uuid.UUID     `gorm:"type:uuid;column:ticket_id"`
	StaffUserID    uuid.UUID     `gorm:"type:uuid;column:staff_user_id"`
	Method         CheckinMethod `gorm:"column:method"`
	DeviceID       *string       `gorm:"column:device_id"`
	ClientScanID   uuid.UUID     `gorm:"type:uuid;column:client_scan_id"`
	ScannedAt      time.Time     `gorm:"column:scanned_at"`
	ReceivedAt     time.Time     `gorm:"column:received_at"`
	VoidedAt       *time.Time    `gorm:"column:voided_at"`
	VoidedBy       *uuid.UUID    `gorm:"type:uuid;column:voided_by"`
	VoidReason     *string       `gorm:"column:void_reason"`
}

// TableName implements gorm's Tabler.
func (Checkin) TableName() string { return "event_checkins" }

// CheckinCounts are the attendance counters of an event (getCheckinStats).
type CheckinCounts struct {
	// Confirmed are the seat holders: confirmed and checked-in registrations
	// (events.confirmed_count, 02 §11.1).
	Confirmed  int
	CheckedIn  int
	Voided     int // voided check-ins
	Waitlisted int
	NoShow     int
}

// CheckinStore is the persistence of event_checkins and the ticket and
// attendance reads and writes of BE-031 (02 §11.1, §11.7–11.8), part of
// Store.
//
// Org scope (06 H-38): a check-in copies event_id and org_id from its
// registration row; every read and write is scoped to the event of orgID
// the service read. The cross-org reads are documented one by one: the
// ticket lookup by its token hash (the token is the credential, the
// service compares the ticket's event before anything else is read), the
// ticket of an outbox event and the system jobs.
//
// Lock order (no deadlock with the registration flows, BE-028): the events
// row first (KeyShareLockEvent / LockEvent), then registration rows, then
// check-in rows.
type CheckinStore interface {
	// KeyShareLockEvent reads the event of orgID FOR KEY SHARE: concurrent
	// check-ins share the lock, a flow that locks the event FOR UPDATE
	// (cancellation, finalisation) waits for them and they wait for it.
	KeyShareLockEvent(ctx context.Context, orgID, eventID uuid.UUID) (Event, error)

	// TicketByLookup returns the ticket whose token_lookup is lookup (any
	// status; false when none).
	TicketByLookup(ctx context.Context, lookup []byte) (Ticket, bool, error)
	// TicketByManualCode returns the ticket of event eventID of orgID with
	// the manual code (any status; false when none).
	TicketByManualCode(ctx context.Context, orgID, eventID uuid.UUID, code string) (Ticket, bool, error)
	// GetTicket returns ticket id (any status) or ErrNotFound.
	GetTicket(ctx context.Context, id uuid.UUID) (Ticket, error)

	// CheckinByClientScan returns the check-in staffUserID recorded for
	// event eventID of orgID under clientScanID (a client retry; T-137:
	// another event's or staff member's scan is never returned).
	CheckinByClientScan(ctx context.Context, orgID, eventID, staffUserID, clientScanID uuid.UUID) (Checkin, bool, error)
	// CheckIn records c for its registration of orgID in one step: the
	// conditional UPDATE confirmed → checked_in (checked_in_at = scanned_at)
	// takes the registration row, then the check-in row is written (event_id
	// and org_id from the registration). false and nothing written when the
	// registration is not confirmed (checked in meanwhile, cancelled, …).
	// ErrClientScanTaken when client_scan_id belongs to another check-in
	// (the caller's transaction must roll back).
	CheckIn(ctx context.Context, orgID uuid.UUID, c Checkin) (Checkin, bool, error)
	// ActiveCheckin returns the valid (not voided) check-in of a
	// registration of event eventID of orgID.
	ActiveCheckin(ctx context.Context, orgID, eventID, registrationID uuid.UUID) (Checkin, bool, error)
	// VoidCheckin voids check-in id of event eventID of orgID and moves its
	// registration back to confirmed (checked_in_at cleared): ErrNotFound
	// when there is no such check-in, ErrInvalidState when it is already
	// voided (the caller's transaction must roll back).
	VoidCheckin(ctx context.Context, orgID, eventID, id, by uuid.UUID, reason string, at time.Time) (Checkin, Registration, error)
	// ListCheckins returns up to limit check-ins of the event, the latest
	// scan first (scanned_at, id descending), after the position after.
	ListCheckins(ctx context.Context, orgID, eventID uuid.UUID, after *After, limit int) ([]Checkin, error)
	// CheckinCounts returns the attendance counters of the event.
	CheckinCounts(ctx context.Context, orgID, eventID uuid.UUID) (CheckinCounts, error)

	// FinalizeAttendance stores attendance_finalized_at (and _by; nil: the
	// system) of an event of orgID whose attendance is still open; false
	// when it was finalised already.
	FinalizeAttendance(ctx context.Context, orgID, eventID uuid.UUID, by *uuid.UUID, at time.Time) (bool, error)
	// MarkNoShows turns up to limit confirmed registrations of the event
	// into no_show (oldest first) and returns them; their seats leave
	// confirmed_count in the same step (no_show holds no seat, 02 §11.1).
	// Run it under LockEvent.
	MarkNoShows(ctx context.Context, orgID, eventID uuid.UUID, limit int) ([]Registration, error)
	// DueAttendance returns up to limit events with an id after after
	// (uuid.Nil: from the start) whose attendance the system closes: check-in
	// enabled, not finalised, published or completed, ended at or before
	// endedBefore, with at least one valid check-in.
	DueAttendance(ctx context.Context, endedBefore time.Time, after uuid.UUID, limit int) ([]Event, error)
	// SeatHolders returns up to limit confirmed and checked-in registrations
	// of the event with an id after after, by id (the schedule change
	// e-mails).
	SeatHolders(ctx context.Context, orgID, eventID, after uuid.UUID, limit int) ([]Registration, error)

	// CheckinsOfRegistration returns the check-ins of a registration of
	// orgID, oldest first (KVKK export).
	CheckinsOfRegistration(ctx context.Context, orgID, registrationID uuid.UUID) ([]Checkin, error)
	// CheckinsByStaff returns the check-ins userID recorded as staff, oldest
	// first (KVKK export).
	CheckinsByStaff(ctx context.Context, userID uuid.UUID) ([]Checkin, error)
}
