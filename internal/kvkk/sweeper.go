package kvkk

import (
	"context"
	"fmt"
	"time"

	"koro/internal/platform/privacy"
)

// kvkk's own retention (02 §16, §24.2; BE-057): closed requests (3 years),
// closed breaches (5 years) and the destruction record itself (3 years,
// last in SweepOrder: tonight's rows are written by this run). The export
// files of 7 days are not a sweeper: kvkk.purge_exports deletes them every
// hour whatever the legal approval (T-83, 06 H-48), one destruction row
// per file.

// Retention policy codes kvkk sweeps.
const (
	PolicyDSRRecord      = "dsr_record"
	PolicyBreachRecord   = "breach_record"
	PolicyDestructionLog = "destruction_log"

	// RetentionBatch is the most rows one purge transaction deletes (01 §7).
	RetentionBatch = 1000
)

// sweeper is kvkk's privacy.RetentionSweeper.
type sweeper struct{ s *service }

var _ privacy.RetentionSweeper = sweeper{}

// PolicyCodes implements privacy.RetentionSweeper.
func (sweeper) PolicyCodes() []string {
	return []string{PolicyBreachRecord, PolicyDestructionLog, PolicyDSRRecord}
}

// Sweep implements privacy.RetentionSweeper: unapproved, it only counts
// (kvkk.retention_sweep rolls the transaction back); approved, it deletes
// in batches of RetentionBatch rows, one transaction each, oldest first.
func (w sweeper) Sweep(ctx context.Context, p privacy.RetentionPolicy, now time.Time) (privacy.SweepResult, error) {
	cutoff := now.Add(-p.Retention)
	var (
		table string
		purge func(ctx context.Context, cutoff time.Time, limit int, dry bool) (int, error)
	)
	switch p.Code {
	case PolicyDSRRecord:
		table, purge = "data_subject_requests", w.s.d.Store.PurgeClosed
	case PolicyBreachRecord:
		table, purge = "breach_incidents", w.s.d.Records.PurgeBreaches
	case PolicyDestructionLog:
		table, purge = "destruction_logs", w.s.d.Records.PurgeDestructionLogs
	default:
		return privacy.SweepResult{}, fmt.Errorf("kvkk: no retention sweep for policy %q", p.Code)
	}
	n, err := w.s.purgeBatches(ctx, p.LegalApproved, func(ctx context.Context, limit int, dry bool) (int, error) {
		return purge(ctx, cutoff, limit, dry)
	})
	return privacy.SweepResult{PolicyCode: p.Code, Table: table, Method: privacy.MethodHardDelete, Count: n}, err
}

// purgeBatches runs purge: unapproved, one counting call; approved, one
// transaction per batch of RetentionBatch rows until a batch comes back
// short (or ctx ends). It returns the rows counted or deleted.
func (s *service) purgeBatches(ctx context.Context, approved bool, purge func(ctx context.Context, limit int, dry bool) (int, error)) (int, error) {
	if !approved {
		return purge(ctx, 0, true)
	}
	total := 0
	for {
		var n int
		err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
			var err error
			n, err = purge(ctx, RetentionBatch, false)
			return err
		})
		if err != nil {
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
