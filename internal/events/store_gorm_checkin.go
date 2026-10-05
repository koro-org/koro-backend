package events

import (
	"context"
	"errors"
	"time"
	"uuid"

	"koro/internal/platform/db"
)

// checkinCols is the column list of event_checkins.
const checkinCols = `id, event_id, org_id, registration_id, ticket_id, staff_user_id, method, device_id, client_scan_id,
		scanned_at, received_at, voided_at, voided_by, void_reason`

// found turns returning's ErrNotFound into (zero, false, nil).
func found[T any](v T, err error) (T, bool, error) {
	switch {
	case errors.Is(err, ErrNotFound):
		var zero T
		return zero, false, nil
	case err != nil:
		var zero T
		return zero, false, err
	}
	return v, true, nil
}

// KeyShareLockEvent implements Store.
func (s gormStore) KeyShareLockEvent(ctx context.Context, orgID, eventID uuid.UUID) (Event, error) {
	return returning[Event](ctx, s.orm, "key share lock event", `SELECT `+eventCols+` FROM events
		WHERE id = @id AND org_id = @org_id FOR KEY SHARE`, map[string]any{"id": eventID, "org_id": orgID})
}

// TicketByLookup implements Store.
//
// cross-org: the token hash is the credential (unique over the table); the
// service verifies the token and compares the ticket's event with the
// scanned event before anything else of the ticket is used.
func (s gormStore) TicketByLookup(ctx context.Context, lookup []byte) (Ticket, bool, error) {
	return found(returning[Ticket](ctx, s.orm, "ticket by lookup", `SELECT `+ticketCols+` FROM registration_tickets
		WHERE token_lookup = @lookup`, map[string]any{"lookup": lookup}))
}

// TicketByManualCode implements Store.
func (s gormStore) TicketByManualCode(ctx context.Context, orgID, eventID uuid.UUID, code string) (Ticket, bool, error) {
	return found(returning[Ticket](ctx, s.orm, "ticket by manual code", `SELECT `+prefixed(ticketCols, "t.")+`
		FROM registration_tickets t JOIN event_registrations r ON r.id = t.registration_id
		WHERE t.event_id = @event_id AND t.manual_code = @code AND r.org_id = @org_id`,
		map[string]any{"event_id": eventID, "code": code, "org_id": orgID}))
}

// GetTicket implements Store.
//
// cross-org: the ticket.reissued consumer reads the ticket named by its
// outbox event; the registration is then read through the ticket's event
// and that event's org (06 H-38).
func (s gormStore) GetTicket(ctx context.Context, id uuid.UUID) (Ticket, error) {
	return returning[Ticket](ctx, s.orm, "get ticket", `SELECT `+ticketCols+` FROM registration_tickets WHERE id = @id`,
		map[string]any{"id": id})
}

// CheckinByClientScan implements Store.
func (s gormStore) CheckinByClientScan(ctx context.Context, orgID, eventID, staffUserID, clientScanID uuid.UUID) (Checkin, bool, error) {
	return found(returning[Checkin](ctx, s.orm, "checkin by client scan", `SELECT `+checkinCols+` FROM event_checkins
		WHERE client_scan_id = @client_scan_id AND event_id = @event_id AND staff_user_id = @staff AND org_id = @org_id`,
		map[string]any{"client_scan_id": clientScanID, "event_id": eventID, "staff": staffUserID, "org_id": orgID}))
}

// CheckIn implements Store: a client_scan_id another scan holds is refused
// first (nothing changes; a concurrent insert of the same id still fails
// on the unique index and the caller rolls back), then the conditional
// UPDATE takes the registration row (concurrent scans of one ticket queue
// on it and re-check the status on the committed row, so exactly one
// passes) and the check-in row is written with event_id and org_id of the
// registration (06 H-38).
func (s gormStore) CheckIn(ctx context.Context, orgID uuid.UUID, c Checkin) (Checkin, bool, error) {
	var taken bool
	if err := db.Conn(ctx, s.orm).Raw(`SELECT EXISTS (SELECT 1 FROM event_checkins WHERE client_scan_id = @client_scan_id)`,
		map[string]any{"client_scan_id": c.ClientScanID}).Scan(&taken).Error; err != nil {
		return Checkin{}, false, wrap("check in", err)
	}
	if taken {
		return Checkin{}, false, ErrClientScanTaken
	}
	res := db.Conn(ctx, s.orm).Exec(`UPDATE event_registrations SET status = 'checked_in', checked_in_at = @at
		WHERE id = @id AND event_id = @event_id AND org_id = @org_id AND status = 'confirmed'`,
		map[string]any{"id": c.RegistrationID, "event_id": c.EventID, "org_id": orgID, "at": c.ScannedAt})
	if res.Error != nil {
		return Checkin{}, false, wrap("check in", res.Error)
	}
	if res.RowsAffected == 0 {
		return Checkin{}, false, nil
	}
	out, err := returning[Checkin](ctx, s.orm, "insert checkin", `INSERT INTO event_checkins
		(id, event_id, org_id, registration_id, ticket_id, staff_user_id, method, device_id, client_scan_id, scanned_at,
		 received_at)
		SELECT @id, r.event_id, r.org_id, r.id, @ticket_id, @staff, @method, @device_id, @client_scan_id, @scanned_at,
		 @received_at
		FROM event_registrations r WHERE r.id = @registration_id AND r.org_id = @org_id
		RETURNING `+checkinCols, map[string]any{
		"id": c.ID, "ticket_id": c.TicketID, "staff": c.StaffUserID, "method": string(c.Method), "device_id": c.DeviceID,
		"client_scan_id": c.ClientScanID, "scanned_at": c.ScannedAt, "received_at": c.ReceivedAt,
		"registration_id": c.RegistrationID, "org_id": orgID,
	})
	if err != nil {
		return Checkin{}, false, err
	}
	return out, true, nil
}

// ActiveCheckin implements Store.
func (s gormStore) ActiveCheckin(ctx context.Context, orgID, eventID, registrationID uuid.UUID) (Checkin, bool, error) {
	return found(returning[Checkin](ctx, s.orm, "active checkin", `SELECT `+checkinCols+` FROM event_checkins
		WHERE registration_id = @registration_id AND event_id = @event_id AND org_id = @org_id AND voided_at IS NULL`,
		map[string]any{"registration_id": registrationID, "event_id": eventID, "org_id": orgID}))
}

// VoidCheckin implements Store. Lock order as CheckIn: the registration
// row, then the check-in row (the conditional UPDATE of the check-in
// refuses a concurrent second void).
func (s gormStore) VoidCheckin(ctx context.Context, orgID, eventID, id, by uuid.UUID, reason string, at time.Time) (Checkin, Registration, error) {
	args := map[string]any{"id": id, "event_id": eventID, "org_id": orgID, "by": by, "reason": reason, "at": at}
	c, err := returning[Checkin](ctx, s.orm, "get checkin", `SELECT `+checkinCols+` FROM event_checkins
		WHERE id = @id AND event_id = @event_id AND org_id = @org_id`, args)
	if err != nil {
		return Checkin{}, Registration{}, err
	}
	if c.VoidedAt != nil {
		return Checkin{}, Registration{}, ErrInvalidState
	}
	args["registration_id"] = c.RegistrationID
	r, err := returning[Registration](ctx, s.orm, "uncheck registration", `UPDATE event_registrations
		SET status = 'confirmed', checked_in_at = NULL
		WHERE id = @registration_id AND event_id = @event_id AND org_id = @org_id AND status = 'checked_in'
		RETURNING `+regCols, args)
	if errors.Is(err, ErrNotFound) {
		return Checkin{}, Registration{}, ErrInvalidState
	}
	if err != nil {
		return Checkin{}, Registration{}, err
	}
	c, err = returning[Checkin](ctx, s.orm, "void checkin", `UPDATE event_checkins
		SET voided_at = @at, voided_by = @by, void_reason = @reason
		WHERE id = @id AND event_id = @event_id AND org_id = @org_id AND voided_at IS NULL
		RETURNING `+checkinCols, args)
	if errors.Is(err, ErrNotFound) {
		return Checkin{}, Registration{}, ErrInvalidState // voided meanwhile
	}
	if err != nil {
		return Checkin{}, Registration{}, err
	}
	return c, r, nil
}

// ListCheckins implements Store.
func (s gormStore) ListCheckins(ctx context.Context, orgID, eventID uuid.UUID, after *After, limit int) ([]Checkin, error) {
	q := db.Conn(ctx, s.orm).Model(&Checkin{}).Select(checkinCols).Scopes(db.ScopeOrg(orgID)).Where("event_id = ?", eventID)
	if after != nil {
		q = q.Where("(scanned_at, id) < ( ?, ?)", after.At, after.ID)
	}
	var out []Checkin
	if err := q.Order("scanned_at DESC, id DESC").Limit(limit).Find(&out).Error; err != nil {
		return nil, wrap("list checkins", err)
	}
	return out, nil
}

// CheckinCounts implements Store.
func (s gormStore) CheckinCounts(ctx context.Context, orgID, eventID uuid.UUID) (CheckinCounts, error) {
	var rows []CheckinCounts
	err := db.Conn(ctx, s.orm).Raw(`SELECT
		 count(*) FILTER (WHERE status IN `+seatStatuses+`)::int AS confirmed,
		 count(*) FILTER (WHERE status = 'checked_in')::int AS checked_in,
		 count(*) FILTER (WHERE status = 'waitlisted')::int AS waitlisted,
		 count(*) FILTER (WHERE status = 'no_show')::int AS no_show,
		 (SELECT count(*) FROM event_checkins c WHERE c.event_id = @event_id AND c.org_id = @org_id
		   AND c.voided_at IS NOT NULL)::int AS voided
		FROM event_registrations WHERE event_id = @event_id AND org_id = @org_id`,
		map[string]any{"event_id": eventID, "org_id": orgID}).Scan(&rows).Error
	if err != nil {
		return CheckinCounts{}, wrap("checkin counts", err)
	}
	if len(rows) == 0 {
		return CheckinCounts{}, nil
	}
	return rows[0], nil
}

// FinalizeAttendance implements Store.
func (s gormStore) FinalizeAttendance(ctx context.Context, orgID, eventID uuid.UUID, by *uuid.UUID, at time.Time) (bool, error) {
	res := db.Conn(ctx, s.orm).Exec(`UPDATE events SET attendance_finalized_at = @at, attendance_finalized_by = @by
		WHERE id = @id AND org_id = @org_id AND attendance_finalized_at IS NULL`,
		map[string]any{"id": eventID, "org_id": orgID, "by": by, "at": at})
	if res.Error != nil {
		return false, wrap("finalize attendance", res.Error)
	}
	return res.RowsAffected == 1, nil
}

// MarkNoShows implements Store.
func (s gormStore) MarkNoShows(ctx context.Context, orgID, eventID uuid.UUID, limit int) ([]Registration, error) {
	var out []Registration
	err := db.Conn(ctx, s.orm).Raw(`UPDATE event_registrations r SET status = 'no_show'
		FROM (SELECT x.id FROM event_registrations x
		      WHERE x.event_id = @event_id AND x.org_id = @org_id AND x.status = 'confirmed'
		      ORDER BY x.created_at, x.id LIMIT @limit FOR UPDATE OF x) t
		WHERE r.id = t.id RETURNING `+prefixed(regCols, "r."),
		map[string]any{"event_id": eventID, "org_id": orgID, "limit": limit}).Scan(&out).Error
	if err != nil {
		return nil, wrap("mark no-shows", err)
	}
	if len(out) == 0 {
		return out, nil
	}
	err = db.Conn(ctx, s.orm).Exec(`UPDATE events SET confirmed_count = GREATEST(confirmed_count - @n, 0)
		WHERE id = @id AND org_id = @org_id`, map[string]any{"n": len(out), "id": eventID, "org_id": orgID}).Error
	if err != nil {
		return nil, wrap("mark no-shows", err)
	}
	return out, nil
}

// DueAttendance implements Store.
//
// cross-org: the system job events.auto_finalize_attendance runs over
// every event.
func (s gormStore) DueAttendance(ctx context.Context, endedBefore time.Time, after uuid.UUID, limit int) ([]Event, error) {
	var out []Event
	err := db.Conn(ctx, s.orm).Raw(`SELECT `+eventCols+` FROM events e
		WHERE e.checkin_enabled AND e.attendance_finalized_at IS NULL AND e.status IN ('published','completed')
		  AND e.ends_at <= @ended_before AND e.id > @after
		  AND EXISTS (SELECT 1 FROM event_checkins c WHERE c.event_id = e.id AND c.voided_at IS NULL)
		ORDER BY e.id LIMIT @limit`, map[string]any{"ended_before": endedBefore, "after": after, "limit": limit}).
		Scan(&out).Error
	if err != nil {
		return nil, wrap("due attendance", err)
	}
	return out, nil
}

// SeatHolders implements Store.
func (s gormStore) SeatHolders(ctx context.Context, orgID, eventID, after uuid.UUID, limit int) ([]Registration, error) {
	var out []Registration
	err := db.Conn(ctx, s.orm).Raw(`SELECT `+regCols+` FROM event_registrations
		WHERE event_id = @event_id AND org_id = @org_id AND status IN `+seatStatuses+` AND id > @after
		ORDER BY id LIMIT @limit`, map[string]any{"event_id": eventID, "org_id": orgID, "after": after, "limit": limit}).
		Scan(&out).Error
	if err != nil {
		return nil, wrap("seat holders", err)
	}
	return out, nil
}

// CheckinsOfRegistration implements Store.
func (s gormStore) CheckinsOfRegistration(ctx context.Context, orgID, registrationID uuid.UUID) ([]Checkin, error) {
	var out []Checkin
	err := db.Conn(ctx, s.orm).Model(&Checkin{}).Select(checkinCols).Scopes(db.ScopeOrg(orgID)).
		Where("registration_id = ?", registrationID).Order("scanned_at, id").Find(&out).Error
	if err != nil {
		return nil, wrap("checkins of registration", err)
	}
	return out, nil
}

// CheckinsByStaff implements Store.
//
// cross-org: the data subject's own scans as staff, in every org (KVKK
// export).
func (s gormStore) CheckinsByStaff(ctx context.Context, userID uuid.UUID) ([]Checkin, error) {
	var out []Checkin
	err := db.Conn(ctx, s.orm).Model(&Checkin{}).Select(checkinCols).Where("staff_user_id = ?", userID).
		Order("scanned_at, id").Find(&out).Error
	if err != nil {
		return nil, wrap("checkins by staff", err)
	}
	return out, nil
}
