package feed

import (
	"context"
	"time"
	"uuid"

	"koro/internal/platform/db"
)

// droppedLogoCols are the columns of org_dropped_logos (BE-180).
const droppedLogoCols = `file_id, org_id, dropped_at`

// DropLogo implements Store.
//
// cross-org: org_id comes from the org row updateOrg locked (06 H-38).
func (s gormStore) DropLogo(ctx context.Context, l DroppedLogo) error {
	err := db.Conn(ctx, s.orm).Exec(`INSERT INTO org_dropped_logos (file_id, org_id, dropped_at)
		VALUES ( @file_id, @org_id, @dropped_at)`,
		map[string]any{"file_id": l.FileID, "org_id": l.OrgID, "dropped_at": l.DroppedAt}).Error
	if err != nil {
		return wrap("drop logo", err)
	}
	return nil
}

// DroppedLogosOf implements Store.
func (s gormStore) DroppedLogosOf(ctx context.Context, orgID uuid.UUID) ([]DroppedLogo, error) {
	var rows []DroppedLogo
	if err := db.Conn(ctx, s.orm).Scopes(db.ScopeOrg(orgID)).Select(droppedLogoCols).
		Order("dropped_at, file_id").Find(&rows).Error; err != nil {
		return nil, wrap("dropped logos of", err)
	}
	return rows, nil
}

// ForgetDroppedLogos implements Store.
//
// cross-org: the rows the retention sweep locked.
func (s gormStore) ForgetDroppedLogos(ctx context.Context, fileIDs []uuid.UUID) (int, error) {
	if len(fileIDs) == 0 {
		return 0, nil
	}
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM org_dropped_logos WHERE file_id = ANY ( @ids)`,
		map[string]any{"ids": db.UUIDArray(fileIDs)})
	if res.Error != nil {
		return 0, wrap("forget dropped logos", res.Error)
	}
	return int(res.RowsAffected), nil
}

// droppedLogoDueSQL selects the kept logos past their retention that no
// report still holds: a report filed before the logo left, open or in review,
// or resolved within the same period (BE-180: the logo lasts as long as the
// report it may be evidence for; a later report cannot be about it).
const droppedLogoDueSQL = `d.dropped_at < @cutoff
	AND NOT EXISTS (SELECT 1 FROM content_reports r WHERE r.target_type = 'org' AND r.target_id = d.org_id
		AND r.created_at <= d.dropped_at AND (r.status IN ('open', 'in_review') OR r.resolved_at >= @cutoff))`

// DroppedLogosDue implements Store.
//
// cross-org: retention spans every org.
func (s gormStore) DroppedLogosDue(ctx context.Context, cutoff time.Time, limit int) ([]DroppedLogo, error) {
	var rows []DroppedLogo
	err := db.Conn(ctx, s.orm).Raw(`SELECT d.file_id, d.org_id, d.dropped_at FROM org_dropped_logos d WHERE `+droppedLogoDueSQL+`
		ORDER BY d.dropped_at, d.file_id LIMIT @limit FOR UPDATE OF d SKIP LOCKED`,
		map[string]any{"cutoff": cutoff, "limit": limit}).Scan(&rows).Error
	if err != nil {
		return nil, wrap("dropped logos due", err)
	}
	return rows, nil
}

// CountDroppedLogosDue implements Store.
//
// cross-org: retention spans every org.
func (s gormStore) CountDroppedLogosDue(ctx context.Context, cutoff time.Time) (int, error) {
	var n int64
	if err := db.Conn(ctx, s.orm).Raw(`SELECT count(*) FROM org_dropped_logos d WHERE `+droppedLogoDueSQL,
		map[string]any{"cutoff": cutoff}).Scan(&n).Error; err != nil {
		return 0, wrap("count dropped logos due", err)
	}
	return int(n), nil
}
