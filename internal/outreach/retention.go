package outreach

import (
	"context"
	"fmt"
	"time"

	"koro/internal/platform/db"
	"koro/internal/platform/privacy"
)

// Retention (02 §13.2, §24.2; BE-057 pattern): kvkk.retention_sweep calls
// the sweeper with the outreach_dispatch policy (2 years after created_at,
// hard delete). A policy that is not legally approved is a dry run (count
// only); an approved one deletes finished dispatches in batches of
// RetentionBatch rows, each in its own transaction, oldest first. A
// dispatch that never finished (scheduled or running) is kept: its unit
// and its run are still open.

// Retention policy codes of outreach (retention_policies.code).
const (
	PolicyOutreachDispatch = "outreach_dispatch"

	// RetentionBatch is the most rows one purge transaction deletes (01 §7).
	RetentionBatch = 1000
)

// sweeper is outreach's privacy.RetentionSweeper.
type sweeper struct{ s *service }

var _ privacy.RetentionSweeper = sweeper{}

// PolicyCodes implements privacy.RetentionSweeper.
func (sweeper) PolicyCodes() []string { return []string{PolicyOutreachDispatch} }

// Sweep implements privacy.RetentionSweeper.
func (w sweeper) Sweep(ctx context.Context, p privacy.RetentionPolicy, now time.Time) (privacy.SweepResult, error) {
	if p.Code != PolicyOutreachDispatch {
		return privacy.SweepResult{}, fmt.Errorf("outreach: no retention sweep for policy %q", p.Code)
	}
	cutoff := now.Add(-p.Retention)
	res := privacy.SweepResult{PolicyCode: p.Code, Table: "outreach_dispatches", Method: privacy.MethodHardDelete}
	if !p.LegalApproved {
		n, err := w.s.d.Store.PurgeDispatches(ctx, cutoff, 0, true)
		res.Count = n
		return res, err
	}
	n, err := db.Batches(ctx, w.s.d.Tx, RetentionBatch, func(ctx context.Context) (int, error) {
		return w.s.d.Store.PurgeDispatches(ctx, cutoff, RetentionBatch, false)
	})
	res.Count = n
	return res, err
}
