package audit

import (
	"context"
	"fmt"
	"time"

	"koro/internal/platform/civil"
	"koro/internal/platform/privacy"
)

// Retention (02 §2.1 "İmha", §24.2; BE-057). kvkk.retention_sweep calls
// the sweeper; an unapproved policy is a dry run (count only, inside a
// transaction kvkk rolls back).
//
//	audit_events  2 years: the chained rows covered by the daily root of
//	              each day older than the period are deleted through
//	              audit_purge_until (000008), one day per transaction,
//	              oldest first. The chain stays verifiable: it continues
//	              from the newest purged day's root (BeginChain, Verify).
//	              A day's rows are its batch (the function deletes up to a
//	              root, not by count).
//	audit_roots   10 years: a root goes once a later root whose rows are
//	              all purged exists (000121 audit_daily_roots_purge), in
//	              batches of RetentionBatch.
//
// Unchained rows (no root yet) are never touched.

// Retention policy codes audit sweeps.
const (
	PolicyEvents = "audit_events"
	PolicyRoots  = "audit_roots"

	// RetentionBatch is the most roots one purge transaction deletes.
	RetentionBatch = 1000

	// purgeMarginDays keeps the newest day a purge may reach one day
	// inside the window audit_purge_until checks against the database
	// clock (Europe/Istanbul days), so a small clock skew cannot make it
	// refuse.
	purgeMarginDays = 1
)

// sweeper is audit's privacy.RetentionSweeper.
type sweeper struct{ s *service }

var _ privacy.RetentionSweeper = sweeper{}

// PolicyCodes implements privacy.RetentionSweeper.
func (sweeper) PolicyCodes() []string { return []string{PolicyEvents, PolicyRoots} }

// Sweep implements privacy.RetentionSweeper.
func (w sweeper) Sweep(ctx context.Context, p privacy.RetentionPolicy, now time.Time) (privacy.SweepResult, error) {
	switch p.Code {
	case PolicyEvents:
		n, err := w.sweepEvents(ctx, p, now)
		return privacy.SweepResult{PolicyCode: p.Code, Table: "audit_events", Method: privacy.MethodHardDelete, Count: n}, err
	case PolicyRoots:
		n, err := w.sweepRoots(ctx, p, now)
		return privacy.SweepResult{PolicyCode: p.Code, Table: "audit_daily_roots", Method: privacy.MethodHardDelete, Count: n}, err
	}
	return privacy.SweepResult{}, fmt.Errorf("audit: no retention sweep for policy %q", p.Code)
}

// sweepEvents purges the log day by day up to (excluding) the first day
// inside the period.
func (w sweeper) sweepEvents(ctx context.Context, p privacy.RetentionPolicy, now time.Time) (int, error) {
	before := civil.Today(now.Add(-p.Retention)).AddDays(-purgeMarginDays)
	if !p.LegalApproved {
		return w.s.d.Store.CountPurgeable(ctx, before)
	}
	total := 0
	for {
		days, err := w.s.d.Store.PurgeDays(ctx, before, 1)
		if err != nil || len(days) == 0 {
			return total, err
		}
		var n int
		if err := w.s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
			var err error
			n, err = w.s.d.Store.PurgeUntil(ctx, days[0])
			return err
		}); err != nil {
			return total, err
		}
		total += n
		if err := ctx.Err(); err != nil {
			return total, err
		}
	}
}

// sweepRoots purges the old roots in batches.
func (w sweeper) sweepRoots(ctx context.Context, p privacy.RetentionPolicy, now time.Time) (int, error) {
	cutoff := now.Add(-p.Retention)
	if !p.LegalApproved {
		return w.s.d.Store.PurgeRoots(ctx, cutoff, RetentionBatch, true)
	}
	total := 0
	for {
		var n int
		if err := w.s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
			var err error
			n, err = w.s.d.Store.PurgeRoots(ctx, cutoff, RetentionBatch, false)
			return err
		}); err != nil {
			return total, err
		}
		total += n
		if n < RetentionBatch {
			return total, nil
		}
		if err := ctx.Err(); err != nil {
			return total, err
		}
	}
}
