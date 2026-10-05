package feed

import (
	"context"
	"fmt"
	"uuid"

	"gorm.io/gorm/clause"

	"koro/internal/platform/db"
	"koro/internal/platform/problem"
)

// The reports, moderation and trust statements of the GORM store (BE-032;
// 02 §12.1, §12.9–12.10).

const (
	reportCols   = `id, reporter_user_id, target_type, target_id, reason, details, status, assigned_to, resolution_note, resolved_at, created_at, updated_at`
	actionCols   = `id, report_id, target_type, target_id, action, reason_code, actor_user_id, note, created_at`
	settingsCols = `org_id, trust_level, approved_posts_count, restricted_until, reason, updated_by, created_at, updated_at`
)

// insertReportTries bounds the loop of InsertReport: a conflicting open
// report resolved between the INSERT and the SELECT is retried.
const insertReportTries = 3

// InsertReport implements Store: INSERT … ON CONFLICT DO NOTHING on the
// partial unique index content_reports_open_uq (a concurrent duplicate
// waits for the first and finds its row), then the open report.
//
// cross-org: reports have no org scope (moderation is the platform's, 02
// §12.9); the reporter is the caller.
func (s gormStore) InsertReport(ctx context.Context, r Report) (Report, bool, error) {
	if r.ID == uuid.Nil() {
		return Report{}, false, fmt.Errorf("feed: insert report: %w", problem.ErrInternal)
	}
	conn := db.Conn(ctx, s.orm)
	args := map[string]any{
		"id": r.ID, "reporter": r.ReporterUserID, "target_type": string(r.TargetType), "target_id": r.TargetID,
		"reason": string(r.Reason), "details": r.Details,
	}
	for range insertReportTries {
		var rows []Report
		if err := conn.Raw(`INSERT INTO content_reports (id, reporter_user_id, target_type, target_id, reason, details)
			VALUES ( @id, @reporter, @target_type, @target_id, @reason, @details)
			ON CONFLICT (reporter_user_id, target_type, target_id) WHERE status IN ('open', 'in_review') DO NOTHING
			RETURNING `+reportCols, args).Scan(&rows).Error; err != nil {
			return Report{}, false, wrap("insert report", err)
		}
		if len(rows) == 1 {
			return rows[0], true, nil
		}
		if err := conn.Raw(`SELECT `+reportCols+` FROM content_reports
			WHERE reporter_user_id = @reporter AND target_type = @target_type AND target_id = @target_id
			AND status IN ('open', 'in_review')`, args).Scan(&rows).Error; err != nil {
			return Report{}, false, wrap("insert report", err)
		}
		if len(rows) == 1 {
			return rows[0], false, nil
		}
	}
	return Report{}, false, fmt.Errorf("feed: insert report: %w", problem.ErrConflict)
}

// GetReport implements Store.
//
// cross-org: a report is addressed by its id on the admin surface.
func (s gormStore) GetReport(ctx context.Context, id uuid.UUID) (Report, error) {
	return take[Report]("get report", reportCols, db.Conn(ctx, s.orm).Where("id = ?", id))
}

// LockReport implements Store.
//
// cross-org: as GetReport, with the row locked.
func (s gormStore) LockReport(ctx context.Context, id uuid.UUID) (Report, error) {
	q := db.Conn(ctx, s.orm).Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate})
	return take[Report]("lock report", reportCols, q.Where("id = ?", id))
}

// UpdateReport implements Store.
//
// cross-org: as GetReport.
func (s gormStore) UpdateReport(ctx context.Context, r Report) (Report, error) {
	return returning[Report](ctx, s.orm, "update report", `UPDATE content_reports SET
		status = @status, assigned_to = @assigned_to, resolution_note = @resolution_note, resolved_at = @resolved_at
		WHERE id = @id RETURNING `+reportCols, map[string]any{
		"id": r.ID, "status": string(r.Status), "assigned_to": r.AssignedTo, "resolution_note": r.ResolutionNote,
		"resolved_at": r.ResolvedAt,
	})
}

// ListReports implements Store over content_reports_queue_idx (open
// reports) or the primary key.
//
// cross-org: the moderation queue spans every org.
func (s gormStore) ListReports(ctx context.Context, f ReportFilter, after *After, limit int) ([]Report, error) {
	q := db.Conn(ctx, s.orm).Select(reportCols)
	if len(f.Statuses) > 0 {
		ss := make([]string, len(f.Statuses))
		for i, st := range f.Statuses {
			ss[i] = string(st)
		}
		q = q.Where("status IN ?", ss)
	}
	if f.Reason != "" {
		q = q.Where("reason = ?", string(f.Reason))
	}
	if f.TargetType != "" {
		q = q.Where("target_type = ?", string(f.TargetType))
	}
	order := "created_at, id"
	if after != nil {
		if f.Newest {
			q = q.Where("(created_at, id) < ( ?, ?)", after.At, after.ID)
		} else {
			q = q.Where("(created_at, id) > ( ?, ?)", after.At, after.ID)
		}
	}
	if f.Newest {
		order = "created_at DESC, id DESC"
	}
	var out []Report
	if err := q.Order(order).Limit(limit).Find(&out).Error; err != nil {
		return nil, wrap("list reports", err)
	}
	return out, nil
}

// OpenReporters implements Store.
//
// cross-org: the reports on one target.
func (s gormStore) OpenReporters(ctx context.Context, t TargetType, targetID uuid.UUID) (int, error) {
	var n int64
	if err := db.Conn(ctx, s.orm).Model(&Report{}).
		Where("target_type = ? AND target_id = ? AND status IN ('open', 'in_review')", string(t), targetID).
		Distinct("reporter_user_id").Count(&n).Error; err != nil {
		return 0, wrap("open reporters", err)
	}
	return int(n), nil
}

// ReportsOf implements Store.
//
// cross-org: the data subject's own reports (KVKK export).
func (s gormStore) ReportsOf(ctx context.Context, userID uuid.UUID) ([]Report, error) {
	var out []Report
	if err := db.Conn(ctx, s.orm).Select(reportCols).Where("reporter_user_id = ?", userID).Order("id").
		Find(&out).Error; err != nil {
		return nil, wrap("reports of", err)
	}
	return out, nil
}

// DeleteReportsOf implements Store (moderation_actions.report_id goes to
// NULL: ON DELETE SET NULL, the one change its trigger allows).
//
// cross-org: the data subject's own reports (KVKK erase).
func (s gormStore) DeleteReportsOf(ctx context.Context, userID uuid.UUID) (int, error) {
	res := db.Conn(ctx, s.orm).Exec(`DELETE FROM content_reports WHERE reporter_user_id = ?`, userID)
	if res.Error != nil {
		return 0, wrap("delete reports of", res.Error)
	}
	return int(res.RowsAffected), nil
}

// InsertModerationAction implements Store.
//
// cross-org: moderation actions have no org scope (02 §12.10); the target
// was read and authorised by the service.
func (s gormStore) InsertModerationAction(ctx context.Context, m ModerationAction) (ModerationAction, error) {
	if m.ID == uuid.Nil() {
		return ModerationAction{}, fmt.Errorf("feed: insert moderation action: %w", problem.ErrInternal)
	}
	return returning[ModerationAction](ctx, s.orm, "insert moderation action", `INSERT INTO moderation_actions
		(id, report_id, target_type, target_id, action, reason_code, actor_user_id, note)
		VALUES ( @id, @report_id, @target_type, @target_id, @action, @reason_code, @actor, @note)
		RETURNING `+actionCols, map[string]any{
		"id": m.ID, "report_id": m.ReportID, "target_type": string(m.TargetType), "target_id": m.TargetID,
		"action": string(m.Action), "reason_code": m.ReasonCode, "actor": m.ActorUserID, "note": m.Note,
	})
}

// GetModerationAction implements Store.
//
// cross-org: an action is read back by its id after the decision.
func (s gormStore) GetModerationAction(ctx context.Context, id uuid.UUID) (ModerationAction, error) {
	return take[ModerationAction]("get moderation action", actionCols, db.Conn(ctx, s.orm).Where("id = ?", id))
}

// ListModerationActions implements Store over the primary key or
// moderation_actions_target_idx.
//
// cross-org: the moderation history spans every org.
func (s gormStore) ListModerationActions(ctx context.Context, f ActionFilter, after *uuid.UUID, limit int) ([]ModerationAction, error) {
	q := db.Conn(ctx, s.orm).Select(actionCols)
	if f.TargetType != "" {
		q = q.Where("target_type = ?", string(f.TargetType))
	}
	if f.TargetID != nil {
		q = q.Where("target_id = ?", *f.TargetID)
	}
	if after != nil {
		q = q.Where("id < ?", *after)
	}
	var out []ModerationAction
	if err := q.Order("id DESC").Limit(limit).Find(&out).Error; err != nil {
		return nil, wrap("list moderation actions", err)
	}
	return out, nil
}

// LatestActions implements Store (DISTINCT ON over
// moderation_actions_target_idx).
//
// cross-org: the actions on posts or orgs the service already read.
func (s gormStore) LatestActions(ctx context.Context, t TargetType, ids []uuid.UUID) (map[uuid.UUID]ModerationAction, error) {
	out := map[uuid.UUID]ModerationAction{}
	if len(ids) == 0 {
		return out, nil
	}
	var rows []ModerationAction
	if err := db.Conn(ctx, s.orm).Raw(`SELECT DISTINCT ON (target_id) `+actionCols+` FROM moderation_actions
		WHERE target_type = @type AND target_id = ANY(CAST( @ids AS uuid[]))
		ORDER BY target_id, created_at DESC, id DESC`,
		map[string]any{"type": string(t), "ids": db.UUIDArray(ids)}).Scan(&rows).Error; err != nil {
		return nil, wrap("latest actions", err)
	}
	for _, m := range rows {
		out[m.TargetID] = m
	}
	return out, nil
}

// ReportActions implements Store.
//
// cross-org: the actions of one report.
func (s gormStore) ReportActions(ctx context.Context, reportID uuid.UUID) (int, error) {
	var n int64
	if err := db.Conn(ctx, s.orm).Model(&ModerationAction{}).Where("report_id = ?", reportID).Count(&n).Error; err != nil {
		return 0, wrap("report actions", err)
	}
	return int(n), nil
}

// ActionsBy implements Store.
//
// cross-org: the data subject's own moderation work (KVKK export).
func (s gormStore) ActionsBy(ctx context.Context, userID uuid.UUID) ([]ModerationAction, error) {
	var out []ModerationAction
	if err := db.Conn(ctx, s.orm).Select(actionCols).Where("actor_user_id = ?", userID).Order("id").
		Find(&out).Error; err != nil {
		return nil, wrap("actions by", err)
	}
	return out, nil
}

// ListReview implements Store over posts_review_idx.
//
// cross-org: the review queue spans every org (moderators, 03 §5.7).
func (s gormStore) ListReview(ctx context.Context, after *After, limit int) ([]Post, error) {
	q := db.Conn(ctx, s.orm).Table("posts AS p").Select(prefixed).Where("p.status = 'pending_review'")
	if after != nil {
		q = q.Where("(p.created_at, p.id) > ( ?, ?)", after.At, after.ID)
	}
	var out []Post
	if err := q.Order("p.created_at, p.id").Limit(limit).Scan(&out).Error; err != nil {
		return nil, wrap("list review", err)
	}
	return out, nil
}

// AddApprovedPost implements Store in one statement (a concurrent approval
// waits on the row and counts on top of it).
func (s gormStore) AddApprovedPost(ctx context.Context, orgID uuid.UUID) (OrgSettings, error) {
	return returning[OrgSettings](ctx, s.orm, "add approved post", `INSERT INTO feed_org_settings (org_id, approved_posts_count)
		VALUES ( @org_id, 1)
		ON CONFLICT (org_id) DO UPDATE SET approved_posts_count = feed_org_settings.approved_posts_count + 1
		WHERE feed_org_settings.org_id = @org_id
		RETURNING `+settingsCols, map[string]any{"org_id": orgID})
}

// PromoteTrusted implements Store.
func (s gormStore) PromoteTrusted(ctx context.Context, orgID uuid.UUID, threshold int) (bool, error) {
	res := db.Conn(ctx, s.orm).Exec(`UPDATE feed_org_settings SET trust_level = 'trusted', updated_by = NULL
		WHERE org_id = @org_id AND trust_level = 'new' AND approved_posts_count >= @threshold`,
		map[string]any{"org_id": orgID, "threshold": threshold})
	if res.Error != nil {
		return false, wrap("promote trusted", res.Error)
	}
	return res.RowsAffected == 1, nil
}

// SetTrust implements Store.
func (s gormStore) SetTrust(ctx context.Context, o OrgSettings) (OrgSettings, error) {
	return returning[OrgSettings](ctx, s.orm, "set trust", `INSERT INTO feed_org_settings
		(org_id, trust_level, restricted_until, reason, updated_by)
		VALUES ( @org_id, @trust_level, @restricted_until, @reason, @updated_by)
		ON CONFLICT (org_id) DO UPDATE SET trust_level = EXCLUDED.trust_level,
		restricted_until = EXCLUDED.restricted_until, reason = EXCLUDED.reason, updated_by = EXCLUDED.updated_by
		WHERE feed_org_settings.org_id = @org_id
		RETURNING `+settingsCols, map[string]any{
		"org_id": o.OrgID, "trust_level": string(o.TrustLevel), "restricted_until": o.RestrictedUntil,
		"reason": o.Reason, "updated_by": o.UpdatedBy,
	})
}
