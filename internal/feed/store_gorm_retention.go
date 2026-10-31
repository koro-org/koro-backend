package feed

import (
	"context"
	"time"
	"uuid"

	"koro/internal/platform/db"
)

// removedDueSQL selects the removed posts past their retention that no
// dispatch or contact request refers to (T-152 option a).
const removedDueSQL = `p.status = 'removed' AND p.removed_at < @cutoff
	AND NOT EXISTS (SELECT 1 FROM outreach_dispatches d WHERE d.post_id = p.id)
	AND NOT EXISTS (SELECT 1 FROM contact_requests c WHERE c.opportunity_post_id = p.id)`

// RemovedPostsDue implements Store.
//
// cross-org: retention spans every org.
func (s gormStore) RemovedPostsDue(ctx context.Context, cutoff time.Time, limit int) ([]Post, error) {
	var rows []Post
	err := db.Conn(ctx, s.orm).Raw(`SELECT `+prefixed+` FROM posts p WHERE `+removedDueSQL+`
		ORDER BY p.removed_at, p.id LIMIT @limit FOR UPDATE OF p SKIP LOCKED`,
		map[string]any{"cutoff": cutoff, "limit": limit}).Scan(&rows).Error
	if err != nil {
		return nil, wrap("removed posts due", err)
	}
	return rows, nil
}

// CountRemovedPostsDue implements Store.
//
// cross-org: retention spans every org.
func (s gormStore) CountRemovedPostsDue(ctx context.Context, cutoff time.Time) (int, error) {
	var n int64
	if err := db.Conn(ctx, s.orm).Raw(`SELECT count(*) FROM posts p WHERE `+removedDueSQL,
		map[string]any{"cutoff": cutoff}).Scan(&n).Error; err != nil {
		return 0, wrap("count removed posts due", err)
	}
	return int(n), nil
}

// PurgePosts implements Store.
//
// cross-org: the posts RemovedPostsDue returned (retention).
func (s gormStore) PurgePosts(ctx context.Context, ids []uuid.UUID) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM posts WHERE id = ANY ( @ids)`, map[string]any{"ids": db.UUIDArray(ids)})
	if res.Error != nil {
		return 0, wrap("purge posts", res.Error)
	}
	return int(res.RowsAffected), nil
}

// droppedDueSQL selects the dropped images past their retention whose
// post has no report open or in review (BE-158: the evidence waits).
const droppedDueSQL = `d.dropped_at < @cutoff
	AND NOT EXISTS (SELECT 1 FROM content_reports r WHERE r.target_type = 'post' AND r.target_id = d.post_id
		AND r.status IN ('open', 'in_review'))`

// DroppedMediaDue implements Store.
//
// cross-org: retention spans every org.
func (s gormStore) DroppedMediaDue(ctx context.Context, cutoff time.Time, limit int) ([]DroppedMedia, error) {
	var rows []DroppedMedia
	err := db.Conn(ctx, s.orm).Raw(`SELECT d.file_id, d.post_id, d.alt_text, d.width, d.height, d.dropped_at
		FROM post_dropped_media d WHERE `+droppedDueSQL+`
		ORDER BY d.dropped_at, d.file_id LIMIT @limit FOR UPDATE OF d SKIP LOCKED`,
		map[string]any{"cutoff": cutoff, "limit": limit}).Scan(&rows).Error
	if err != nil {
		return nil, wrap("dropped media due", err)
	}
	return rows, nil
}

// CountDroppedMediaDue implements Store.
//
// cross-org: retention spans every org.
func (s gormStore) CountDroppedMediaDue(ctx context.Context, cutoff time.Time) (int, error) {
	var n int64
	if err := db.Conn(ctx, s.orm).Raw(`SELECT count(*) FROM post_dropped_media d WHERE `+droppedDueSQL,
		map[string]any{"cutoff": cutoff}).Scan(&n).Error; err != nil {
		return 0, wrap("count dropped media due", err)
	}
	return int(n), nil
}

// PurgeResolvedReports implements Store.
//
// cross-org: reports have no org scope (02 §12.9).
func (s gormStore) PurgeResolvedReports(ctx context.Context, cutoff time.Time, limit int, dryRun bool) (int, error) {
	args := map[string]any{"cutoff": cutoff, "limit": limit}
	if dryRun {
		var n int64
		if err := db.Conn(ctx, s.orm).Raw(`SELECT count(*) FROM content_reports
			WHERE status IN ('actioned','dismissed') AND resolved_at < @cutoff`, args).Scan(&n).Error; err != nil {
			return 0, wrap("count resolved reports", err)
		}
		return int(n), nil
	}
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM content_reports WHERE id IN (SELECT id FROM content_reports
		WHERE status IN ('actioned','dismissed') AND resolved_at < @cutoff
		ORDER BY resolved_at, id LIMIT @limit FOR UPDATE SKIP LOCKED)`, args)
	if res.Error != nil {
		return 0, wrap("purge resolved reports", res.Error)
	}
	return int(res.RowsAffected), nil
}

// PurgeModerationActions implements Store.
//
// cross-org: moderation actions have no org scope (02 §12.10).
func (s gormStore) PurgeModerationActions(ctx context.Context, cutoff time.Time, limit int, dryRun bool) (int, error) {
	var n int64
	if err := db.Conn(ctx, s.orm).Raw(`SELECT moderation_actions_purge( ?, ?, ?)`, cutoff, max(limit, 1), dryRun).Scan(&n).Error; err != nil {
		return 0, wrap("purge moderation actions", err)
	}
	return int(n), nil
}
