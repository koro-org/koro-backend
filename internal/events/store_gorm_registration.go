package events

import (
	"context"
	"errors"
	"strings"
	"time"
	"uuid"

	"gorm.io/gorm/clause"

	"koro/internal/platform/db"
)

// Column lists of event_registrations and registration_tickets.
const (
	regCols = `id, event_id, org_id, user_id, status, form_version_id, answers, answers_sensitive_enc, consent_snapshot,
		source, waitlisted_at, confirmed_at, approved_by, approved_at, decision_note, cancelled_at, cancelled_by_kind,
		cancel_reason, checked_in_at, answers_purge_at, sensitive_purge_at, answers_purged_at, created_at, updated_at`
	ticketCols = `id, registration_id, event_id, version, key_id, token_lookup, manual_code, status, issued_at,
		revoked_at, revoke_reason`
	// activeStatuses are the statuses event_registrations_active_uq covers.
	activeStatuses = `('pending','confirmed','waitlisted','checked_in','no_show')`
	// seatStatuses hold a seat (events.confirmed_count, 02 §11.1).
	seatStatuses = `('confirmed','checked_in')`
)

// prefixed qualifies a column list with a table alias ("r.").
func prefixed(cols, alias string) string {
	parts := strings.Split(cols, ",")
	for i, p := range parts {
		parts[i] = alias + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}

func strPtr[T ~string](p *T) *string {
	if p == nil {
		return nil
	}
	s := string(*p)
	return &s
}

// ClaimSeat implements Store: the conditional counter UPDATE (02 §11.1).
// The row lock serialises concurrent claims; each re-checks the condition
// on the committed count, so no interleaving exceeds the capacity.
func (s gormStore) ClaimSeat(ctx context.Context, orgID, eventID uuid.UUID) (bool, error) {
	res := db.Conn(ctx, s.orm).Exec(`UPDATE events SET confirmed_count = confirmed_count + 1
		WHERE id = @id AND org_id = @org_id AND status = 'published'
		  AND (capacity IS NULL OR confirmed_count < capacity)`, map[string]any{"id": eventID, "org_id": orgID})
	if res.Error != nil {
		return false, wrap("claim seat", res.Error)
	}
	return res.RowsAffected == 1, nil
}

// ReleaseSeat implements Store.
func (s gormStore) ReleaseSeat(ctx context.Context, orgID, eventID uuid.UUID) error {
	err := db.Conn(ctx, s.orm).Exec(`UPDATE events SET confirmed_count = confirmed_count - 1
		WHERE id = @id AND org_id = @org_id AND confirmed_count > 0`, map[string]any{"id": eventID, "org_id": orgID}).Error
	if err != nil {
		return wrap("release seat", err)
	}
	return nil
}

// ShareLockEvent implements Store.
func (s gormStore) ShareLockEvent(ctx context.Context, orgID, eventID uuid.UUID) (Event, error) {
	return take[Event]("share lock event", eventCols, db.Conn(ctx, s.orm).
		Clauses(clause.Locking{Strength: clause.LockingStrengthShare}).Scopes(db.ScopeOrg(orgID)).Where("id = ?", eventID))
}

// GetEvents implements Store.
//
// cross-org: the events of the caller's own registrations (by id).
func (s gormStore) GetEvents(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Event, error) {
	out := map[uuid.UUID]Event{}
	if len(ids) == 0 {
		return out, nil
	}
	var rows []Event
	if err := db.Conn(ctx, s.orm).Model(&Event{}).Select(eventCols).Where("id IN ?", ids).Find(&rows).Error; err != nil {
		return nil, wrap("get events", err)
	}
	for _, e := range rows {
		out[e.ID] = e
	}
	return out, nil
}

// InsertRegistration implements Store. event_id and org_id come from the
// events row (06 H-38).
func (s gormStore) InsertRegistration(ctx context.Context, r Registration) (Registration, error) {
	return returning[Registration](ctx, s.orm, "insert registration", `INSERT INTO event_registrations
		(id, event_id, org_id, user_id, status, form_version_id, answers, answers_sensitive_enc, consent_snapshot, source,
		 waitlisted_at, confirmed_at, answers_purge_at, sensitive_purge_at)
		SELECT @id, e.id, e.org_id, @user_id, @status, @form_version_id, @answers, @answers_sensitive_enc, @consent_snapshot,
		 @source, @waitlisted_at, @confirmed_at, @answers_purge_at, @sensitive_purge_at
		FROM events e WHERE e.id = @event_id AND e.org_id = @org_id
		RETURNING `+regCols, map[string]any{
		"id": r.ID, "event_id": r.EventID, "org_id": r.OrgID, "user_id": r.UserID, "status": string(r.Status),
		"form_version_id": r.FormVersionID, "answers": jsonOr(r.Answers), "answers_sensitive_enc": r.AnswersSensitiveEnc,
		"consent_snapshot": jsonOr(r.ConsentSnapshot), "source": string(r.Source), "waitlisted_at": r.WaitlistedAt,
		"confirmed_at": r.ConfirmedAt, "answers_purge_at": r.AnswersPurgeAt, "sensitive_purge_at": r.SensitivePurgeAt,
	})
}

// jsonOr is j, or the empty object for a nil column value (NOT NULL).
func jsonOr(j JSON) JSON {
	if j == nil {
		return JSON(`{}`)
	}
	return j
}

// GetRegistration implements Store.
func (s gormStore) GetRegistration(ctx context.Context, orgID, eventID, id uuid.UUID) (Registration, error) {
	return take[Registration]("get registration", regCols, db.Conn(ctx, s.orm).Scopes(db.ScopeOrg(orgID)).
		Where("id = ? AND event_id = ?", id, eventID))
}

// LockRegistration implements Store.
func (s gormStore) LockRegistration(ctx context.Context, orgID, eventID, id uuid.UUID) (Registration, error) {
	return take[Registration]("lock registration", regCols, locked(db.Conn(ctx, s.orm)).Scopes(db.ScopeOrg(orgID)).
		Where("id = ? AND event_id = ?", id, eventID))
}

// GetUserRegistration implements Store.
//
// cross-org: the data subject's own registration (/me/registrations/{id}).
func (s gormStore) GetUserRegistration(ctx context.Context, userID, id uuid.UUID) (Registration, error) {
	return take[Registration]("get own registration", regCols, db.Conn(ctx, s.orm).Where("id = ? AND user_id = ?", id, userID))
}

// ActiveRegistration implements Store.
func (s gormStore) ActiveRegistration(ctx context.Context, orgID, eventID, userID uuid.UUID) (Registration, bool, error) {
	r, err := take[Registration]("active registration", regCols, db.Conn(ctx, s.orm).Scopes(db.ScopeOrg(orgID)).
		Where("event_id = ? AND user_id = ? AND status IN "+activeStatuses, eventID, userID))
	switch {
	case errors.Is(err, ErrNotFound):
		return Registration{}, false, nil
	case err != nil:
		return Registration{}, false, err
	}
	return r, true, nil
}

// UpdateRegistration implements Store.
func (s gormStore) UpdateRegistration(ctx context.Context, r Registration) (Registration, error) {
	return returning[Registration](ctx, s.orm, "update registration", `UPDATE event_registrations SET
		status = @status, waitlisted_at = @waitlisted_at, confirmed_at = @confirmed_at, approved_by = @approved_by,
		approved_at = @approved_at, decision_note = @decision_note, cancelled_at = @cancelled_at,
		cancelled_by_kind = @cancelled_by_kind, cancel_reason = @cancel_reason
		WHERE id = @id AND event_id = @event_id AND org_id = @org_id RETURNING `+regCols, map[string]any{
		"id": r.ID, "event_id": r.EventID, "org_id": r.OrgID, "status": string(r.Status), "waitlisted_at": r.WaitlistedAt,
		"confirmed_at": r.ConfirmedAt, "approved_by": r.ApprovedBy, "approved_at": r.ApprovedAt,
		"decision_note": r.DecisionNote, "cancelled_at": r.CancelledAt, "cancelled_by_kind": strPtr(r.CancelledByKind),
		"cancel_reason": r.CancelReason,
	})
}

// NextWaitlisted implements Store (FOR UPDATE SKIP LOCKED: two promotions
// never pick the same registration).
func (s gormStore) NextWaitlisted(ctx context.Context, orgID, eventID uuid.UUID) (Registration, bool, error) {
	var rows []Registration
	err := db.Conn(ctx, s.orm).Raw(`SELECT `+regCols+` FROM event_registrations
		WHERE event_id = @event_id AND org_id = @org_id AND status = 'waitlisted'
		ORDER BY waitlisted_at, id LIMIT 1 FOR UPDATE SKIP LOCKED`, map[string]any{"event_id": eventID, "org_id": orgID}).
		Scan(&rows).Error
	if err != nil {
		return Registration{}, false, wrap("next waitlisted", err)
	}
	if len(rows) == 0 {
		return Registration{}, false, nil
	}
	return rows[0], true, nil
}

// WaitlistPosition implements Store.
func (s gormStore) WaitlistPosition(ctx context.Context, orgID, eventID, id uuid.UUID) (int, error) {
	var n int
	err := db.Conn(ctx, s.orm).Raw(`SELECT count(*) FROM event_registrations r
		JOIN event_registrations w ON w.event_id = r.event_id AND w.status = 'waitlisted'
		 AND (w.waitlisted_at, w.id) <= (r.waitlisted_at, r.id)
		WHERE r.id = @id AND r.event_id = @event_id AND r.org_id = @org_id AND r.status = 'waitlisted'`,
		map[string]any{"id": id, "event_id": eventID, "org_id": orgID}).Scan(&n).Error
	if err != nil {
		return 0, wrap("waitlist position", err)
	}
	return n, nil
}

func statusTexts(st []RegStatus) []string {
	out := make([]string, len(st))
	for i, x := range st {
		out[i] = string(x)
	}
	return out
}

// ListEventRegistrations implements Store.
func (s gormStore) ListEventRegistrations(ctx context.Context, orgID, eventID uuid.UUID, f RegFilter, after *After, limit int) ([]Registration, error) {
	q := db.Conn(ctx, s.orm).Model(&Registration{}).Select(regCols).Scopes(db.ScopeOrg(orgID)).Where("event_id = ?", eventID)
	if len(f.Statuses) > 0 {
		q = q.Where("status IN ?", statusTexts(f.Statuses))
	}
	if f.Desc {
		if after != nil {
			q = q.Where("(created_at, id) < ( ?, ?)", after.At, after.ID)
		}
		q = q.Order("created_at DESC, id DESC")
	} else {
		if after != nil {
			q = q.Where("(created_at, id) > ( ?, ?)", after.At, after.ID)
		}
		q = q.Order("created_at, id")
	}
	var out []Registration
	if err := q.Limit(limit).Find(&out).Error; err != nil {
		return nil, wrap("list registrations", err)
	}
	return out, nil
}

// ListUserRegistrations implements Store.
//
// cross-org: the data subject's own registrations (/me/registrations).
func (s gormStore) ListUserRegistrations(ctx context.Context, userID uuid.UUID, f MyRegFilter, after *After, limit int) ([]Registration, error) {
	q := db.Conn(ctx, s.orm).Table("event_registrations r").Select(prefixed(regCols, "r.")).
		Joins("JOIN events e ON e.id = r.event_id").Where("r.user_id = ?", userID)
	if len(f.Statuses) > 0 {
		q = q.Where("r.status IN ?", statusTexts(f.Statuses))
	}
	if f.EndsAfter != nil {
		q = q.Where("e.ends_at > ?", *f.EndsAfter)
	}
	if after != nil {
		q = q.Where("(e.starts_at, r.id) > ( ?, ?)", after.At, after.ID)
	}
	var out []Registration
	if err := q.Order("e.starts_at, r.id").Limit(limit).Scan(&out).Error; err != nil {
		return nil, wrap("list own registrations", err)
	}
	return out, nil
}

// LockActiveRegistrations implements Store.
func (s gormStore) LockActiveRegistrations(ctx context.Context, orgID, eventID uuid.UUID, limit int) ([]Registration, error) {
	var out []Registration
	err := db.Conn(ctx, s.orm).Raw(`SELECT `+regCols+` FROM event_registrations
		WHERE event_id = @event_id AND org_id = @org_id AND status IN ('pending','confirmed','waitlisted')
		ORDER BY created_at, id LIMIT @limit FOR UPDATE`, map[string]any{"event_id": eventID, "org_id": orgID, "limit": limit}).
		Scan(&out).Error
	if err != nil {
		return nil, wrap("lock active registrations", err)
	}
	return out, nil
}

// ExpireWaitlisted implements Store.
//
// cross-org: the system job events.waitlist_expire runs over every event.
func (s gormStore) ExpireWaitlisted(ctx context.Context, now time.Time, limit int) ([]Registration, error) {
	var out []Registration
	err := db.Conn(ctx, s.orm).Raw(`UPDATE event_registrations r SET status = 'expired'
		FROM (SELECT x.id FROM event_registrations x JOIN events e ON e.id = x.event_id
		      WHERE x.status = 'waitlisted' AND e.starts_at <= @now
		      ORDER BY x.waitlisted_at, x.id LIMIT @limit FOR UPDATE OF x SKIP LOCKED) t
		WHERE r.id = t.id RETURNING `+prefixed(regCols, "r."), map[string]any{"now": now, "limit": limit}).Scan(&out).Error
	if err != nil {
		return nil, wrap("expire waitlisted", err)
	}
	return out, nil
}

// SeatCounts implements Store.
//
// cross-org: the system job events.reconcile_counts runs over every event.
func (s gormStore) SeatCounts(ctx context.Context, endsAfter time.Time, after uuid.UUID, limit int) ([]SeatCount, error) {
	var out []SeatCount
	err := db.Conn(ctx, s.orm).Raw(`SELECT e.id AS event_id, e.org_id, e.confirmed_count AS stored,
		 (SELECT count(*) FROM event_registrations r WHERE r.event_id = e.id AND r.status IN `+seatStatuses+`)::int AS actual
		FROM events e WHERE e.status <> 'draft' AND e.ends_at > @ends_after AND e.id > @after
		ORDER BY e.id LIMIT @limit`, map[string]any{"ends_after": endsAfter, "after": after, "limit": limit}).Scan(&out).Error
	if err != nil {
		return nil, wrap("seat counts", err)
	}
	return out, nil
}

// CountSeats implements Store.
func (s gormStore) CountSeats(ctx context.Context, orgID, eventID uuid.UUID) (int, error) {
	var n int64
	err := db.Conn(ctx, s.orm).Model(&Registration{}).Scopes(db.ScopeOrg(orgID)).
		Where("event_id = ? AND status IN "+seatStatuses, eventID).Count(&n).Error
	if err != nil {
		return 0, wrap("count seats", err)
	}
	return int(n), nil
}

// SetConfirmedCount implements Store.
func (s gormStore) SetConfirmedCount(ctx context.Context, orgID, eventID uuid.UUID, n int) error {
	res := db.Conn(ctx, s.orm).Exec(`UPDATE events SET confirmed_count = @n WHERE id = @id AND org_id = @org_id`,
		map[string]any{"id": eventID, "org_id": orgID, "n": n})
	if res.Error != nil {
		return wrap("set confirmed count", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// ShiftPurgeDates implements Store.
func (s gormStore) ShiftPurgeDates(ctx context.Context, orgID, eventID uuid.UUID, answersAt, sensitiveAt time.Time) error {
	err := db.Conn(ctx, s.orm).Exec(`UPDATE event_registrations SET answers_purge_at = @answers,
		sensitive_purge_at = CASE WHEN sensitive_purge_at IS NULL THEN NULL ELSE CAST( @sensitive AS timestamptz) END
		WHERE event_id = @event_id AND org_id = @org_id AND answers_purged_at IS NULL`, map[string]any{
		"answers": answersAt, "sensitive": sensitiveAt, "event_id": eventID, "org_id": orgID,
	}).Error
	if err != nil {
		return wrap("shift purge dates", err)
	}
	return nil
}

// CountUserRegistrationsInOrg implements Store.
func (s gormStore) CountUserRegistrationsInOrg(ctx context.Context, orgID, userID uuid.UUID) (int, error) {
	var n int64
	err := db.Conn(ctx, s.orm).Model(&Registration{}).Scopes(db.ScopeOrg(orgID)).
		Where("user_id = ? AND status IN "+activeStatuses, userID).Count(&n).Error
	if err != nil {
		return 0, wrap("count registrations in org", err)
	}
	return int(n), nil
}

// InsertTicket implements Store. event_id comes from the registration row
// (06 H-38); a taken manual code writes nothing (ON CONFLICT DO NOTHING:
// the transaction stays usable).
func (s gormStore) InsertTicket(ctx context.Context, orgID uuid.UUID, t Ticket) (Ticket, bool, error) {
	out, err := returning[Ticket](ctx, s.orm, "insert ticket", `INSERT INTO registration_tickets
		(id, registration_id, event_id, version, key_id, token_lookup, manual_code, status, issued_at)
		SELECT @id, r.id, r.event_id, @version, @key_id, @token_lookup, @manual_code, 'active', @issued_at
		FROM event_registrations r WHERE r.id = @registration_id AND r.org_id = @org_id
		ON CONFLICT ON CONSTRAINT registration_tickets_event_manual_code_uq DO NOTHING
		RETURNING `+ticketCols, map[string]any{
		"id": t.ID, "registration_id": t.RegistrationID, "org_id": orgID, "version": t.Version, "key_id": t.KeyID,
		"token_lookup": t.TokenLookup, "manual_code": t.ManualCode, "issued_at": t.IssuedAt,
	})
	switch {
	case errors.Is(err, ErrNotFound):
	case err != nil:
		return Ticket{}, false, err
	default:
		return out, true, nil
	}
	var exists bool
	if err := db.Conn(ctx, s.orm).Raw(`SELECT EXISTS (SELECT 1 FROM event_registrations
		WHERE id = @id AND org_id = @org_id)`, map[string]any{"id": t.RegistrationID, "org_id": orgID}).Scan(&exists).Error; err != nil {
		return Ticket{}, false, wrap("insert ticket", err)
	}
	if !exists {
		return Ticket{}, false, ErrNotFound
	}
	return Ticket{}, false, nil
}

// ActiveTicket implements Store.
func (s gormStore) ActiveTicket(ctx context.Context, orgID, registrationID uuid.UUID) (Ticket, bool, error) {
	var rows []Ticket
	err := db.Conn(ctx, s.orm).Raw(`SELECT `+prefixed(ticketCols, "t.")+` FROM registration_tickets t
		JOIN event_registrations r ON r.id = t.registration_id
		WHERE t.registration_id = @id AND r.org_id = @org_id AND t.status = 'active'`,
		map[string]any{"id": registrationID, "org_id": orgID}).Scan(&rows).Error
	if err != nil {
		return Ticket{}, false, wrap("active ticket", err)
	}
	if len(rows) == 0 {
		return Ticket{}, false, nil
	}
	return rows[0], true, nil
}

// MaxTicketVersion implements Store.
func (s gormStore) MaxTicketVersion(ctx context.Context, orgID, registrationID uuid.UUID) (int, error) {
	var n int
	err := db.Conn(ctx, s.orm).Raw(`SELECT coalesce(max(t.version), 0) FROM registration_tickets t
		JOIN event_registrations r ON r.id = t.registration_id
		WHERE t.registration_id = @id AND r.org_id = @org_id`, map[string]any{"id": registrationID, "org_id": orgID}).
		Scan(&n).Error
	if err != nil {
		return 0, wrap("max ticket version", err)
	}
	return n, nil
}

// RevokeTickets implements Store.
func (s gormStore) RevokeTickets(ctx context.Context, orgID, registrationID uuid.UUID, reason RevokeReason, at time.Time) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`UPDATE registration_tickets t SET status = 'revoked', revoked_at = @at,
		revoke_reason = @reason FROM event_registrations r
		WHERE r.id = t.registration_id AND t.registration_id = @id AND r.org_id = @org_id AND t.status = 'active'`,
		map[string]any{"id": registrationID, "org_id": orgID, "at": at, "reason": string(reason)})
	if res.Error != nil {
		return 0, wrap("revoke tickets", res.Error)
	}
	return int(res.RowsAffected), nil
}

// RegistrationsOfUser implements Store.
//
// cross-org: the data subject's own rows in every org (KVKK export,
// erasure; students' admin view).
func (s gormStore) RegistrationsOfUser(ctx context.Context, userID uuid.UUID) ([]Registration, error) {
	var out []Registration
	err := db.Conn(ctx, s.orm).Model(&Registration{}).Select(regCols).Where("user_id = ?", userID).
		Order("created_at, id").Find(&out).Error
	if err != nil {
		return nil, wrap("registrations of user", err)
	}
	return out, nil
}

// TicketsOfRegistration implements Store.
func (s gormStore) TicketsOfRegistration(ctx context.Context, orgID, registrationID uuid.UUID) ([]Ticket, error) {
	var out []Ticket
	err := db.Conn(ctx, s.orm).Raw(`SELECT `+prefixed(ticketCols, "t.")+` FROM registration_tickets t
		JOIN event_registrations r ON r.id = t.registration_id
		WHERE t.registration_id = @id AND r.org_id = @org_id ORDER BY t.version`,
		map[string]any{"id": registrationID, "org_id": orgID}).Scan(&out).Error
	if err != nil {
		return nil, wrap("tickets of registration", err)
	}
	return out, nil
}

// DeleteRegistration implements Store (tickets and check-ins cascade).
func (s gormStore) DeleteRegistration(ctx context.Context, orgID, id uuid.UUID) error {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM event_registrations WHERE id = @id AND org_id = @org_id`,
		map[string]any{"id": id, "org_id": orgID})
	if res.Error != nil {
		return wrap("delete registration", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}
