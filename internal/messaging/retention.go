package messaging

import (
	"context"
	"fmt"
	"time"

	"koro/internal/platform/privacy"
)

// Retention (02 §14.1, §24.2; BE-056): a closed request (accepted,
// declined, expired, withdrawn) is hard deleted 12 months after it closed
// (contact_request_closed; the policy's anchor closed_at is the row's
// updated_at: the transition that closed it is its last update). A pending
// request is never swept (it closes within 14 days). kvkk.retention_sweep
// calls the sweeper; an unapproved policy is a dry run (count only), an
// approved one deletes in batches of RetentionBatch, one transaction each,
// oldest first.

const (
	// PolicyContactRequestClosed is the retention policy messaging sweeps.
	PolicyContactRequestClosed = "contact_request_closed"
	// RetentionBatch is the most requests one purge transaction deletes.
	RetentionBatch = 1000
)

// sweeper is messaging's privacy.RetentionSweeper.
type sweeper struct{ s *service }

var _ privacy.RetentionSweeper = sweeper{}

// PolicyCodes implements privacy.RetentionSweeper.
func (sweeper) PolicyCodes() []string { return []string{PolicyContactRequestClosed} }

// Sweep implements privacy.RetentionSweeper.
func (w sweeper) Sweep(ctx context.Context, p privacy.RetentionPolicy, now time.Time) (privacy.SweepResult, error) {
	if p.Code != PolicyContactRequestClosed {
		return privacy.SweepResult{}, fmt.Errorf("messaging: no retention sweep for policy %q", p.Code)
	}
	cutoff := now.Add(-p.Retention)
	res := privacy.SweepResult{PolicyCode: p.Code, Table: "contact_requests", Method: privacy.MethodHardDelete}
	if !p.LegalApproved {
		n, err := w.s.d.Store.CountClosedBefore(ctx, cutoff)
		res.Count = n
		return res, err
	}
	for {
		var n int
		err := w.s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
			var err error
			n, err = w.s.d.Store.PurgeClosed(ctx, cutoff, RetentionBatch)
			return err
		})
		if err != nil {
			return res, err
		}
		res.Count += n
		if n < RetentionBatch {
			return res, nil
		}
		if err := ctx.Err(); err != nil {
			return res, err
		}
	}
}
