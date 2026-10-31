package feed

import (
	"context"
	"uuid"
)

// reportStore is content_reports (BE-032; 02 §12.9; report.go,
// moderation.go). Reports have no org scope: moderation is the platform's
// (02 §12.9 "Org kapsamı: yok"). The service authorises the admin surface
// before it reads them.
type reportStore interface {
	// InsertReport writes r as an open report, with its snapshot of the
	// reported post (BE-160), unless the reporter already has an open (open
	// or in_review) report on the target (content_reports_open_uq): then it
	// returns that report, its first snapshot unchanged, and created=false.
	// Concurrent duplicates end with one row. A snapshot is a post report's
	// only (content_reports_snapshot_target_chk, ErrValidation).
	InsertReport(ctx context.Context, r Report) (Report, bool, error)
	// GetReport returns the report or ErrNotFound.
	GetReport(ctx context.Context, id uuid.UUID) (Report, error)
	// LockReport is GetReport with the row locked for the transaction.
	LockReport(ctx context.Context, id uuid.UUID) (Report, error)
	// UpdateReport writes the status, assigned_to, resolution_note and
	// resolved_at of r and returns the stored row (ErrNotFound when gone);
	// the snapshot is never rewritten (content_reports_snapshot_guard).
	UpdateReport(ctx context.Context, r Report) (Report, error)
	// ListReports returns up to limit reports of f by (created_at, id),
	// oldest first unless f.Newest, after the position after.
	ListReports(ctx context.Context, f ReportFilter, after *After, limit int) ([]Report, error)
	// OpenReporters counts the users with an open report on the target (one
	// open report per user and target: the distinct reporters).
	OpenReporters(ctx context.Context, t TargetType, targetID uuid.UUID) (int, error)
	// ReportsOf returns every report filed by userID, oldest first (KVKK
	// export).
	ReportsOf(ctx context.Context, userID uuid.UUID) ([]Report, error)
	// DeleteReportsOf hard-deletes them (KVKK erase; the moderation actions
	// keep their decision, report_id becomes NULL) and returns the count.
	DeleteReportsOf(ctx context.Context, userID uuid.UUID) (int, error)
}

// ReportFilter narrows ListReports (adminListReports).
type ReportFilter struct {
	Statuses   []ReportStatus // empty: every status
	Reason     ReportReason   // "": every reason
	TargetType TargetType     // "": both
	Newest     bool           // newest first (sort=-created_at)
}
