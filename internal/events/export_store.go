package events

import (
	"context"
	"time"
	"uuid"
)

// ExportStore is the persistence of event_data_exports (02 §11.9; BE-050).
// An export is addressed through its event: every read and write is scoped
// to the event's org and the event (06 H-38), except the cross-org system
// reads of the jobs and the KVKK provider.
type ExportStore interface {
	// InsertExport writes x for the event x.EventID of x.OrgID (event_id
	// and org_id from the events row) and returns the stored row.
	// ErrNotFound when the event is not x.OrgID's.
	InsertExport(ctx context.Context, x Export) (Export, error)
	// GetExport returns export id of event eventID of orgID or ErrNotFound.
	GetExport(ctx context.Context, orgID, eventID, id uuid.UUID) (Export, error)
	// LockExport is GetExport with the row locked for the transaction.
	LockExport(ctx context.Context, orgID, eventID, id uuid.UUID) (Export, error)
	// UpdateExport writes the state columns of x (status, row_count,
	// file_id, file_sha256, purged_at) scoped to x.OrgID and x.EventID and
	// returns the row; ErrNotFound when it is gone.
	UpdateExport(ctx context.Context, x Export) (Export, error)
	// RecordDownload counts one download of a ready export (download_count
	// + 1, first_downloaded_at set once to at) in one statement and returns
	// the row; ErrNotFound when the export is not ready (any more).
	RecordDownload(ctx context.Context, orgID, eventID, id uuid.UUID, at time.Time) (Export, error)
	// ListExports returns up to limit exports of the event after the
	// position after (created_at, id), newest first.
	ListExports(ctx context.Context, orgID, eventID uuid.UUID, after *After, limit int) ([]Export, error)
	// ExportsDue returns up to limit ready exports whose expires_at is at
	// or before now with an id after after (uuid.Nil: from the start), by
	// id (events.purge_exports; cross-org system job).
	ExportsDue(ctx context.Context, now time.Time, after uuid.UUID, limit int) ([]Export, error)
	// QueuedExports returns up to limit queued exports created before
	// before with an id after after, by id (events.purge_exports repairs a
	// lost build task and gives up on a stale one; cross-org system job).
	QueuedExports(ctx context.Context, before time.Time, after uuid.UUID, limit int) ([]Export, error)
	// ExportsOfUser returns the exports userID requested, oldest first
	// (KVKK export; cross-org).
	ExportsOfUser(ctx context.Context, userID uuid.UUID) ([]Export, error)
	// ClearExportIPs removes the IP address of the exports userID requested
	// and reports how many rows changed (KVKK erasure; cross-org).
	ClearExportIPs(ctx context.Context, userID uuid.UUID) (int, error)
}
