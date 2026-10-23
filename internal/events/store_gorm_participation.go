package events

import (
	"context"
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
