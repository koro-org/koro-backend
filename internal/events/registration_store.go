package events

import (
	"context"
	"time"
	"uuid"
)

// RegistrationStore is the persistence of event_registrations and
// registration_tickets (02 §11.6–11.7; BE-028), part of Store.
//
// Org scope (06 H-38): a registration is addressed through its event; the
// service passes the org it read from the events row, and the store copies
// event_id/org_id of a new registration from that row (and event_id of a
// ticket from the registration row), never from the caller. The only
// cross-org reads are the data subject's own rows (GetUserRegistration,
// ListUserRegistrations, RegistrationsOfUser) and the system jobs.
//
// Capacity (02 §11.1): ClaimSeat is the conditional counter UPDATE, the
// only way a seat is taken; it is atomic in PostgreSQL (row lock) and in
// memory (mutex), so no concurrency exceeds the capacity. The CHECK
// events_capacity_fill_chk is the second layer.
type RegistrationStore interface {
	// ClaimSeat takes one seat of a published event of orgID: confirmed_count
	// + 1 WHERE capacity IS NULL OR confirmed_count < capacity. false when
	// the event is full or not published (nothing changed).
	ClaimSeat(ctx context.Context, orgID, eventID uuid.UUID) (bool, error)
	// ReleaseSeat gives one seat back (confirmed_count - 1, never below 0).
	ReleaseSeat(ctx context.Context, orgID, eventID uuid.UUID) error
	// ShareLockEvent reads the event of orgID FOR SHARE: a concurrent
	// cancellation (FOR UPDATE) waits for this transaction, so a
	// registration written under the lock is seen by the event.cancelled
	// consumer.
	ShareLockEvent(ctx context.Context, orgID, eventID uuid.UUID) (Event, error)
	// GetEvents returns the events of ids that exist (cross-org: the caller's
	// own registrations name them).
	GetEvents(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Event, error)

	// InsertRegistration writes r for the event r.EventID of r.OrgID (event_id
	// and org_id from the events row) and returns the stored row.
	// ErrAlreadyRegistered when the user has a live registration of the event
	// (event_registrations_active_uq); ErrNotFound when the event is not
	// r.OrgID's.
	InsertRegistration(ctx context.Context, r Registration) (Registration, error)
	// GetRegistration returns registration id of event eventID of orgID or
	// ErrNotFound.
	GetRegistration(ctx context.Context, orgID, eventID, id uuid.UUID) (Registration, error)
	// LockRegistration is GetRegistration with the row locked.
	LockRegistration(ctx context.Context, orgID, eventID, id uuid.UUID) (Registration, error)
	// GetUserRegistration returns registration id of userID or ErrNotFound
	// (the data subject's own row, any org).
	GetUserRegistration(ctx context.Context, userID, id uuid.UUID) (Registration, error)
	// ActiveRegistration returns userID's live registration of the event.
	ActiveRegistration(ctx context.Context, orgID, eventID, userID uuid.UUID) (Registration, bool, error)
	// UpdateRegistration writes the state columns of r (status, waitlisted_at,
	// confirmed_at, approved_by, approved_at, decision_note, cancelled_at,
	// cancelled_by_kind, cancel_reason) scoped to r.OrgID and r.EventID and
	// returns the row; ErrNotFound when it is gone.
	UpdateRegistration(ctx context.Context, r Registration) (Registration, error)
	// NextWaitlisted returns the first waitlisted registration of the event
	// (FIFO: waitlisted_at, id) locked FOR UPDATE SKIP LOCKED; false when
	// there is none (or every one is locked by another promotion).
	NextWaitlisted(ctx context.Context, orgID, eventID uuid.UUID) (Registration, bool, error)
	// WaitlistPosition returns the 1-based place of a waitlisted registration
	// (0 when it is not waitlisted).
	WaitlistPosition(ctx context.Context, orgID, eventID, id uuid.UUID) (int, error)
	// ListEventRegistrations returns up to limit registrations of the event
	// matching f after the position after (created_at, id), by created_at
	// then id (f.Desc: newest first).
	ListEventRegistrations(ctx context.Context, orgID, eventID uuid.UUID, f RegFilter, after *After, limit int) ([]Registration, error)
	// ListUserRegistrations returns up to limit registrations of userID
	// matching f after the position after (the event's starts_at, the
	// registration id), in that order.
	ListUserRegistrations(ctx context.Context, userID uuid.UUID, f MyRegFilter, after *After, limit int) ([]Registration, error)
	// LockActiveRegistrations returns up to limit pending, confirmed or
	// waitlisted registrations of the event, oldest first, locked (the
	// event.cancelled consumer cancels them in batches).
	LockActiveRegistrations(ctx context.Context, orgID, eventID uuid.UUID, limit int) ([]Registration, error)
	// ExpireWaitlisted turns up to limit waitlisted registrations of events
	// that started at or before now into expired (skipping rows another
	// transaction holds) and returns them (events.waitlist_expire; cross-org
	// system job).
	ExpireWaitlisted(ctx context.Context, now time.Time, limit int) ([]Registration, error)
	// SeatCounts returns, for up to limit non-draft events ending after
	// endsAfter with an id after after (uuid.Nil: from the start), the stored
	// confirmed_count and the real count of confirmed and checked-in
	// registrations (events.reconcile_counts; cross-org system job).
	SeatCounts(ctx context.Context, endsAfter time.Time, after uuid.UUID, limit int) ([]SeatCount, error)
	// CountSeats returns the confirmed and checked-in registrations of the
	// event (run it under LockEvent for a consistent count).
	CountSeats(ctx context.Context, orgID, eventID uuid.UUID) (int, error)
	// SetConfirmedCount stores n as the event's confirmed_count.
	SetConfirmedCount(ctx context.Context, orgID, eventID uuid.UUID, n int) error
	// ShiftPurgeDates moves answers_purge_at (and sensitive_purge_at where it
	// is set) of the event's unpurged registrations when the event's end
	// moves (02 §11.6 "bitiş değişirse güncellenir").
	ShiftPurgeDates(ctx context.Context, orgID, eventID uuid.UUID, answersAt, sensitiveAt time.Time) error
	// CountUserRegistrationsInOrg returns userID's live and attended
	// registrations of the events of orgID (students' member card, T-241).
	CountUserRegistrationsInOrg(ctx context.Context, orgID, userID uuid.UUID) (int, error)

	// InsertTicket writes a ticket of registration t.RegistrationID of orgID
	// (event_id from the registration row). It reports false and writes
	// nothing when the manual code is taken in the event (the caller draws
	// another one); ErrNotFound when the registration is not orgID's.
	InsertTicket(ctx context.Context, orgID uuid.UUID, t Ticket) (Ticket, bool, error)
	// ActiveTicket returns the active ticket of a registration of orgID.
	ActiveTicket(ctx context.Context, orgID, registrationID uuid.UUID) (Ticket, bool, error)
	// MaxTicketVersion returns the highest ticket version of a registration
	// of orgID (0 without a ticket).
	MaxTicketVersion(ctx context.Context, orgID, registrationID uuid.UUID) (int, error)
	// RevokeTickets revokes the active ticket of a registration of orgID and
	// reports how many it revoked.
	RevokeTickets(ctx context.Context, orgID, registrationID uuid.UUID, reason RevokeReason, at time.Time) (int, error)

	// RegistrationsOfUser returns every registration of userID, oldest first
	// (KVKK export and erasure; cross-org).
	RegistrationsOfUser(ctx context.Context, userID uuid.UUID) ([]Registration, error)
	// TicketsOfRegistration returns the tickets of a registration of orgID.
	TicketsOfRegistration(ctx context.Context, orgID, registrationID uuid.UUID) ([]Ticket, error)
	// DeleteRegistration deletes a registration of orgID with its tickets
	// (ON DELETE CASCADE); ErrNotFound when it is gone.
	DeleteRegistration(ctx context.Context, orgID, id uuid.UUID) error
}

// RegFilter narrows ListEventRegistrations; empty Statuses lists every one.
type RegFilter struct {
	Statuses []RegStatus
	Desc     bool // newest first (sort -created_at)
}

// MyRegFilter narrows ListUserRegistrations.
type MyRegFilter struct {
	Statuses []RegStatus
	// EndsAfter keeps registrations of events ending after it (upcoming).
	EndsAfter *time.Time
}

// SeatCount is one row of SeatCounts.
type SeatCount struct {
	EventID uuid.UUID
	OrgID   uuid.UUID
	Stored  int
	Actual  int
}
