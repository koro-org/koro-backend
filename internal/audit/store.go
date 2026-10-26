package audit

import (
	"context"
	"time"
	"uuid"

	"koro/internal/platform/civil"
)

// Store is the persistence boundary of audit (05 §2.2). The GORM store
// (store_gorm.go) works in the transaction carried by ctx (db.Conn); the
// in-memory store (store_mem.go) backs service and wiring tests; both pass
// the same contract (store_contract_test.go, 05 §13.3).
//
// Lists return rows in (occurred_at DESC, id DESC) order, at most limit,
// strictly after a cursor position.
type Store interface {
	// Insert appends r (Seq is assigned by the store; chain fields stay
	// empty).
	Insert(ctx context.Context, r Record) error

	// BeginChain prepares the transaction in ctx for chaining: it takes the
	// chain lock (one chainer at a time across processes), switches to the
	// audit_chainer role and returns the chain head: the newest chained
	// row, or the newest daily root when its rows were purged. It must run
	// inside a transaction.
	BeginChain(ctx context.Context) (Link, error)
	// Unchained returns up to limit unchained rows in seq order.
	Unchained(ctx context.Context, limit int) ([]Record, error)
	// SetChain writes the chain columns of the unchained row id.
	SetChain(ctx context.Context, id uuid.UUID, seq int64, prev, hash []byte, at time.Time) error

	// Chained returns up to limit chained rows with chain_seq > after, in
	// chain_seq order (verification).
	Chained(ctx context.Context, after int64, limit int) ([]Record, error)
	// HeadAt returns the newest chained row with chained_at < before (the
	// chain at the end of a day); the zero Link when there is none.
	HeadAt(ctx context.Context, before time.Time) (Link, error)
	// OldestUnchained returns the occurred_at of the oldest unchained row.
	OldestUnchained(ctx context.Context) (time.Time, bool, error)

	// InsertRoot stores a daily root; ErrRootExists when the day has one.
	InsertRoot(ctx context.Context, r DailyRoot) error
	// LastRoot returns the newest daily root.
	LastRoot(ctx context.Context) (DailyRoot, bool, error)
	// RootBefore returns the newest daily root of a day before day.
	RootBefore(ctx context.Context, day civil.Date) (DailyRoot, bool, error)
	// Roots returns the daily roots from..to (inclusive) by day.
	Roots(ctx context.Context, from, to civil.Date) ([]DailyRoot, error)

	// ListOwn lists the security events of user: the user's own identity.*
	// and consent.* events, the break-glass accesses to the user's data
	// (metadata break_glass and subject_user_id) and the refused sign-ins
	// to the user's account, whoever the actor (identity.session.create
	// denied, resource user = user; BE-078).
	ListOwn(ctx context.Context, user uuid.UUID, after *After, limit int) ([]Record, error)
	// ListOrg lists the events of org orgID matching f (f.OrgID ignored).
	ListOrg(ctx context.Context, orgID uuid.UUID, f Filter, after *After, limit int) ([]Record, error)
	// List lists every event matching f (adminQueryAuditEvents).
	List(ctx context.Context, f Filter, after *After, limit int) ([]Record, error)

	// Retention (BE-057; 02 §2.1 "İmha", §24.2 audit_events, audit_roots).
	//
	// PurgeDays returns up to limit days before day that have a daily
	// root and still chained rows covered by it, oldest first.
	PurgeDays(ctx context.Context, before civil.Date, limit int) ([]civil.Date, error)
	// CountPurgeable counts the chained rows covered by the newest root
	// of a day before day (what purging up to it would delete).
	CountPurgeable(ctx context.Context, before civil.Date) (int, error)
	// PurgeUntil deletes the chained rows covered by the root of day
	// through audit_purge_until (000008; the only delete path of the
	// append-only table) and returns how many.
	PurgeUntil(ctx context.Context, day civil.Date) (int, error)
	// PurgeRoots deletes up to limit roots created before cutoff, oldest
	// first, of which a later root whose rows are all purged exists (the
	// verification starts there), through 000121's audit_daily_roots_purge
	// (it never passes now − the policy's period and refuses a real purge
	// of an unapproved policy); dryRun counts.
	PurgeRoots(ctx context.Context, cutoff time.Time, limit int, dryRun bool) (int, error)
}
