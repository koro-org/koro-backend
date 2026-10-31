package events

import (
	"context"
	"time"
	"uuid"

	"koro/internal/platform/db"
)

// EventsOf implements Store.
//
// cross-org: the IDs come from talent's ledger (participation figures,
// BE-085); only the event of each row leaves.
func (s gormStore) EventsOf(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]uuid.UUID, error) {
	out := make(map[uuid.UUID]uuid.UUID, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	var rows []struct {
		ID      uuid.UUID `gorm:"column:id"`
		EventID uuid.UUID `gorm:"column:event_id"`
	}
	arr := db.UUIDArray(ids)
	err := db.Conn(ctx, s.orm).Raw(`SELECT id, event_id FROM event_registrations WHERE id = ANY( ?)
		UNION ALL SELECT id, event_id FROM event_checkins WHERE id = ANY( ?)`, arr, arr).Scan(&rows).Error
	if err != nil {
		return nil, wrap("events of", err)
	}
	for _, r := range rows {
		out[r.ID] = r.EventID
	}
	return out, nil
}

// ParticipationFacts implements Store.
//
// cross-org: the event IDs come from talent (participation figures,
// BE-085); only the organiser org and two counts leave. A registration is
// joined through its event and the event's org (06 H-38).
func (s gormStore) ParticipationFacts(ctx context.Context, eventIDs []uuid.UUID) (map[uuid.UUID]ParticipationFacts, error) {
	out := make(map[uuid.UUID]ParticipationFacts, len(eventIDs))
	if len(eventIDs) == 0 {
		return out, nil
	}
	var rows []struct {
		ID               uuid.UUID `gorm:"column:id"`
		OrgID            uuid.UUID `gorm:"column:org_id"`
		Confirmed        int       `gorm:"column:confirmed"`
		DistinctCheckins int       `gorm:"column:distinct_checkins"`
	}
	err := db.Conn(ctx, s.orm).Raw(`SELECT e.id, e.org_id,
		 count(r.id) FILTER (WHERE r.status IN ('confirmed','checked_in','no_show'))::int AS confirmed,
		 count(DISTINCT r.user_id) FILTER (WHERE r.status = 'checked_in')::int AS distinct_checkins
		FROM events e LEFT JOIN event_registrations r ON r.event_id = e.id AND r.org_id = e.org_id
		WHERE e.id = ANY( ?) GROUP BY e.id, e.org_id`, db.UUIDArray(eventIDs)).Scan(&rows).Error
	if err != nil {
		return nil, wrap("participation facts", err)
	}
	for _, r := range rows {
		out[r.ID] = ParticipationFacts{OrgID: r.OrgID, Confirmed: r.Confirmed, DistinctCheckins: r.DistinctCheckins}
	}
	return out, nil
}

// Undone implements Store.
//
// cross-org: the IDs come from talent's ledger (reconciliation of a closed
// period, BE-090); only the IDs of the undone rows leave.
func (s gormStore) Undone(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	out := make(map[uuid.UUID]bool, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	var rows []uuid.UUID
	arr := db.UUIDArray(ids)
	err := db.Conn(ctx, s.orm).Raw(`SELECT id FROM event_registrations WHERE id = ANY( ?) AND status = 'cancelled'
		UNION ALL SELECT id FROM event_checkins WHERE id = ANY( ?) AND voided_at IS NOT NULL`, arr, arr).Scan(&rows).Error
	if err != nil {
		return nil, wrap("undone", err)
	}
	for _, id := range rows {
		out[id] = true
	}
	return out, nil
}

// TargetableParticipations implements Store.
//
// cross-org: the user IDs come from students' audience refresh (BE-161);
// only the event, its organiser and whether the registration is checked
// in leave. A registration is joined through its event and the event's
// org (06 H-38).
func (s gormStore) TargetableParticipations(ctx context.Context, userIDs []uuid.UUID, since time.Time) ([]TargetableRow, error) {
	if len(userIDs) == 0 {
		return []TargetableRow{}, nil
	}
	var rows []struct {
		UserID   uuid.UUID `gorm:"column:user_id"`
		EventID  uuid.UUID `gorm:"column:event_id"`
		OrgID    uuid.UUID `gorm:"column:org_id"`
		Attended bool      `gorm:"column:attended"`
	}
	err := db.Conn(ctx, s.orm).Raw(`SELECT r.user_id, e.id AS event_id, e.org_id, r.status = 'checked_in' AS attended
		FROM event_registrations r JOIN events e ON e.id = r.event_id AND e.org_id = r.org_id
		WHERE r.user_id = ANY( ?) AND r.status IN ('confirmed','checked_in','no_show')
		  AND e.status IN ('published','completed') AND e.visibility = 'public' AND NOT e.is_sensitive_category
		  AND e.starts_at >= ?
		ORDER BY r.user_id, e.starts_at DESC, e.id`, db.UUIDArray(userIDs), since).Scan(&rows).Error
	if err != nil {
		return nil, wrap("targetable participations", err)
	}
	out := make([]TargetableRow, len(rows))
	for i, r := range rows {
		out[i] = TargetableRow{UserID: r.UserID, EventID: r.EventID, OrgID: r.OrgID, Attended: r.Attended}
	}
	return out, nil
}
