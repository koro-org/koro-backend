package ledger

import (
	"context"
	"fmt"
	"time"

	"koro/internal/platform/clock"
	"koro/internal/platform/privacy"
)

// Retention of the financial records (02 §24.2 financial_records, 10 years;
// 06 H-57, T-224; BE-057). TTK md. 82 and VUK md. 253 count the 10 years
// from the end of the calendar year of the record: a record of 2026 may go
// on 1 January 2037 at the earliest, so the cut-off is the start of the
// calendar year of now − 10 years (Europe/Istanbul). The deletion is
// 000121's financial_records_purge (app_rw holds no DELETE on these tables,
// the ledger is append-only): whole order aggregates (order, lines,
// payments, splits, refunds, matched transfers, invoices, ledger
// transactions, payouts) of which every row is older than the cut-off and
// whose per-payee entries net to zero, then the loose and master rows
// nothing refers to any more. The function repeats the rounding, never
// passes now − 10 years and refuses a real purge of an unapproved policy.
//
// kvkk.retention_sweep calls the sweeper; an unapproved policy is a dry run
// (count only, inside a transaction kvkk rolls back), an approved one
// works in batches of FinancialBatch order aggregates (and as many loose
// rows of each kind), one transaction each, until nothing is left.

const (
	// PolicyFinancialRecords is the retention policy ledger sweeps (its
	// owner module; the tables span orders, payments, ledger, billing and
	// parties).
	PolicyFinancialRecords = "financial_records"
	// FinancialBatch is the most order aggregates one purge transaction
	// deletes (about ten rows each: ≈ 1000 rows, 01 §7).
	FinancialBatch = 100
)

// FinancialCutoff is the cut-off of the financial records at now: the
// start (Europe/Istanbul) of the calendar year of now − retention. Records
// created before it are past their retention.
func FinancialCutoff(now time.Time, retention time.Duration) time.Time {
	at := now.Add(-retention).In(clock.Istanbul())
	return time.Date(at.Year(), time.January, 1, 0, 0, 0, 0, clock.Istanbul())
}

// RetentionStore is what the sweeper needs from the store (BE-057).
type RetentionStore interface {
	// PurgeFinancialRecords deletes up to limit order aggregates (and up to
	// limit loose rows of each kind) past cutoff and returns the rows
	// deleted; dryRun counts every such row and deletes nothing.
	PurgeFinancialRecords(ctx context.Context, cutoff time.Time, limit int, dryRun bool) (int, error)
}

// sweeper is ledger's privacy.RetentionSweeper.
type sweeper struct{ s *service }

var _ privacy.RetentionSweeper = sweeper{}

// PolicyCodes implements privacy.RetentionSweeper.
func (sweeper) PolicyCodes() []string { return []string{PolicyFinancialRecords} }

// Sweep implements privacy.RetentionSweeper.
func (w sweeper) Sweep(ctx context.Context, p privacy.RetentionPolicy, now time.Time) (privacy.SweepResult, error) {
	if p.Code != PolicyFinancialRecords {
		return privacy.SweepResult{}, fmt.Errorf("ledger: no retention sweep for policy %q", p.Code)
	}
	cutoff := FinancialCutoff(now, p.Retention)
	res := privacy.SweepResult{PolicyCode: p.Code, Table: "orders, payments, ledger, payouts, invoices, parties",
		Method: privacy.MethodHardDelete}
	if !p.LegalApproved {
		n, err := w.s.d.Store.PurgeFinancialRecords(ctx, cutoff, FinancialBatch, true)
		res.Count = n
		return res, err
	}
	for {
		var n int
		err := w.s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
			var err error
			n, err = w.s.d.Store.PurgeFinancialRecords(ctx, cutoff, FinancialBatch, false)
			return err
		})
		if err != nil {
			return res, err
		}
		res.Count += n
		if n == 0 {
			return res, nil
		}
		if err := ctx.Err(); err != nil {
			return res, err
		}
	}
}
