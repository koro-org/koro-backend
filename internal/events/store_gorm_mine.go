package events

import (
	"context"
	"uuid"

	"koro/internal/platform/db"
)

// ListStaffEvents implements Store.
//
// cross-org: the caller's own door list spans every organiser it works
// for: the orgs of f.OrgIDs (the caller's memberships) and the events it
// is staff of (event_staff.user_id, event_staff_user_idx).
func (s gormStore) ListStaffEvents(ctx context.Context, f StaffEventFilter, after *After, limit int) ([]Event, error) {
	q := db.Conn(ctx, s.orm).Model(&Event{}).Select(eventCols).
		Where("status = ?", string(StatusPublished)).Where("ends_at > ?", f.EndsAfter)
	staff := "EXISTS (SELECT 1 FROM event_staff s WHERE s.event_id = events.id AND s.user_id = ?)"
	if len(f.OrgIDs) > 0 {
		q = q.Where("(org_id IN ? OR "+staff+")", f.OrgIDs, f.UserID)
	} else {
		q = q.Where(staff, f.UserID)
	}
	if after != nil {
		q = q.Where("(starts_at, id) > ( ?, ?)", after.At, after.ID)
	}
	var out []Event
	if err := q.Order("starts_at, id").Limit(limit).Find(&out).Error; err != nil {
		return nil, wrap("list staff events", err)
	}
	return out, nil
}

// StaffRolesOf implements Store.
//
// cross-org: the caller's own staff rows on a page of its door list.
func (s gormStore) StaffRolesOf(ctx context.Context, userID uuid.UUID, eventIDs []uuid.UUID) (map[uuid.UUID]StaffRole, error) {
	out := map[uuid.UUID]StaffRole{}
	if len(eventIDs) == 0 {
		return out, nil
	}
	var rows []Staff
	err := db.Conn(ctx, s.orm).Model(&Staff{}).Select(staffCols).
		Where("user_id = ? AND event_id IN ?", userID, eventIDs).Find(&rows).Error
	if err != nil {
		return nil, wrap("staff roles of user", err)
	}
	for _, r := range rows {
		out[r.EventID] = r.Role
	}
	return out, nil
}
