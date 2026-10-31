package consent

import (
	"context"
	"time"
)

// iysStateStore is the state of the İYS status reconciliation
// (iys_status.go): iys_status_syncs, one row per channel (BE-103), and
// iys_change_feeds, the read position of each change feed (BE-105).
type iysStateStore interface {
	// IYSSync returns the reconciliation state of an İYS channel;
	// found=false before the first complete run (BE-103).
	IYSSync(ctx context.Context, ch IYSChannel) (st IYSSync, found bool, err error)
	// SaveIYSSync writes channel, synced_at, checked and withdrawn of st
	// (updated_at is the database's). A row whose synced_at is newer than
	// st's is kept: a late run never moves the state back.
	SaveIYSSync(ctx context.Context, st IYSSync) error

	// IYSFeed returns the read position of an İYS change feed; found=false
	// before its first run (BE-105).
	IYSFeed(ctx context.Context, feed string) (st IYSFeedState, found bool, err error)
	// AdvanceIYSFeed writes cursor and horizon of feed (creating the row)
	// unless the stored horizon is newer: a late or concurrent run never
	// moves the feed back.
	AdvanceIYSFeed(ctx context.Context, feed, cursor string, horizon time.Time) error
	// MarkIYSSweep records that a full reconciliation of feed is due since
	// at (the feed lost changes); the latest mark is kept (creating the row).
	MarkIYSSweep(ctx context.Context, feed string, at time.Time) error
	// ClearIYSSweep ends the due reconciliation of feed when it was marked
	// at or before upTo (a reconciliation that started at upTo covers it).
	ClearIYSSweep(ctx context.Context, feed string, upTo time.Time) error
}
