package events

import (
	"context"
	"time"
	"uuid"

	"koro/internal/platform/db"
)

// exportCols is the column list of event_data_exports (the IP as text).
const exportCols = `id, event_id, org_id, requested_by, reason, fields, format, include_sensitive, status, row_count,
	file_id, file_sha256, host(ip) AS ip, expires_at, first_downloaded_at, download_count, purged_at, created_at, updated_at`

// InsertExport implements Store. event_id and org_id come from the events
// row (06 H-38); created_at and updated_at from the column defaults.
func (s gormStore) InsertExport(ctx context.Context, x Export) (Export, error) {
	return returning[Export](ctx, s.orm, "insert export", `INSERT INTO event_data_exports
		(id, event_id, org_id, requested_by, reason, fields, format, include_sensitive, status, ip, expires_at)
		SELECT @id, e.id, e.org_id, @requested_by, @reason, @fields, @format, @include_sensitive, @status, CAST(@ip AS inet), @expires_at
		FROM events e WHERE e.id = @event_id AND e.org_id = @org_id
		RETURNING `+exportCols, map[string]any{
		"id": x.ID, "event_id": x.EventID, "org_id": x.OrgID, "requested_by": x.RequestedBy, "reason": x.Reason,
		"fields": db.TextArray(x.Fields), "format": string(x.Format), "include_sensitive": x.IncludeSensitive,
		"status": string(x.Status), "ip": x.IP, "expires_at": x.ExpiresAt,
	})
}

// GetExport implements Store.
func (s gormStore) GetExport(ctx context.Context, orgID, eventID, id uuid.UUID) (Export, error) {
	return take[Export]("get export", exportCols, db.Conn(ctx, s.orm).Scopes(db.ScopeOrg(orgID)).
		Where("id = ? AND event_id = ?", id, eventID))
}

// LockExport implements Store.
func (s gormStore) LockExport(ctx context.Context, orgID, eventID, id uuid.UUID) (Export, error) {
	return take[Export]("lock export", exportCols, locked(db.Conn(ctx, s.orm)).Scopes(db.ScopeOrg(orgID)).
		Where("id = ? AND event_id = ?", id, eventID))
}

// UpdateExport implements Store.
func (s gormStore) UpdateExport(ctx context.Context, x Export) (Export, error) {
	return returning[Export](ctx, s.orm, "update export", `UPDATE event_data_exports SET
		status = @status, row_count = @row_count, file_id = @file_id, file_sha256 = @file_sha256, purged_at = @purged_at
		WHERE id = @id AND event_id = @event_id AND org_id = @org_id RETURNING `+exportCols, map[string]any{
		"id": x.ID, "event_id": x.EventID, "org_id": x.OrgID, "status": string(x.Status), "row_count": x.RowCount,
		"file_id": x.FileID, "file_sha256": x.FileSHA256, "purged_at": x.PurgedAt,
	})
}

// RecordDownload implements Store: one statement, so concurrent downloads
// never lose a count.
func (s gormStore) RecordDownload(ctx context.Context, orgID, eventID, id uuid.UUID, at time.Time) (Export, error) {
	return returning[Export](ctx, s.orm, "record export download", `UPDATE event_data_exports SET
		download_count = download_count + 1, first_downloaded_at = coalesce(first_downloaded_at, @at)
		WHERE id = @id AND event_id = @event_id AND org_id = @org_id AND status = 'ready' RETURNING `+exportCols,
		map[string]any{"id": id, "event_id": eventID, "org_id": orgID, "at": at})
}

// ListExports implements Store.
func (s gormStore) ListExports(ctx context.Context, orgID, eventID uuid.UUID, after *After, limit int) ([]Export, error) {
	q := db.Conn(ctx, s.orm).Model(&Export{}).Select(exportCols).Scopes(db.ScopeOrg(orgID)).Where("event_id = ?", eventID)
	if after != nil {
		q = q.Where("(created_at, id) < ( ?, ?)", after.At, after.ID)
	}
	var out []Export
	if err := q.Order("created_at DESC, id DESC").Limit(limit).Find(&out).Error; err != nil {
		return nil, wrap("list exports", err)
	}
	return out, nil
}

// ExportsDue implements Store.
//
// cross-org: the hourly purge applies to the exports of every org.
func (s gormStore) ExportsDue(ctx context.Context, now time.Time, after uuid.UUID, limit int) ([]Export, error) {
	var out []Export
	err := db.Conn(ctx, s.orm).Model(&Export{}).Select(exportCols).
		Where("status = 'ready' AND expires_at <= ? AND id > ?", now, after).Order("id").Limit(limit).Find(&out).Error
	if err != nil {
		return nil, wrap("exports due", err)
	}
	return out, nil
}

// QueuedExports implements Store.
//
// cross-org: the hourly repair of lost build tasks covers every org.
func (s gormStore) QueuedExports(ctx context.Context, before time.Time, after uuid.UUID, limit int) ([]Export, error) {
	var out []Export
	err := db.Conn(ctx, s.orm).Model(&Export{}).Select(exportCols).
		Where("status = 'queued' AND created_at < ? AND id > ?", before, after).Order("id").Limit(limit).Find(&out).Error
	if err != nil {
		return nil, wrap("queued exports", err)
	}
	return out, nil
}

// ExportsOfUser implements Store.
//
// cross-org: the data subject's own export requests (KVKK).
func (s gormStore) ExportsOfUser(ctx context.Context, userID uuid.UUID) ([]Export, error) {
	var out []Export
	err := db.Conn(ctx, s.orm).Model(&Export{}).Select(exportCols).Where("requested_by = ?", userID).
		Order("created_at, id").Find(&out).Error
	if err != nil {
		return nil, wrap("exports of user", err)
	}
	return out, nil
}

// ClearExportIPs implements Store.
//
// cross-org: the data subject's own export requests (KVKK erasure).
func (s gormStore) ClearExportIPs(ctx context.Context, userID uuid.UUID) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`UPDATE event_data_exports SET ip = NULL WHERE requested_by = ? AND ip IS NOT NULL`, userID)
	if res.Error != nil {
		return 0, wrap("clear export ips", res.Error)
	}
	return int(res.RowsAffected), nil
}
