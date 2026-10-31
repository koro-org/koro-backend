package messaging

import (
	"context"
	"fmt"
	"time"

	"koro/internal/platform/db"
	"koro/internal/platform/privacy"
)

// Retention (02 §14.1, §24.2; BE-056, BE-162): a closed request (accepted,
// declined, expired, withdrawn) is hard deleted 12 months after it closed
// (contact_request_closed; the policy's anchor closed_at is the row's
// updated_at: the transition that closed it is its last update). A pending
// request is never swept (it closes within 14 days). kvkk.retention_sweep
// calls the sweeper; an unapproved policy is a dry run (count only), an
// approved one deletes in batches of RetentionBatch, one transaction each,
// oldest first.

const (
	// PolicyContactRequestClosed is the retention policy of the closed
	// requests.
	PolicyContactRequestClosed = "contact_request_closed"
	// PolicyConversationClosed is the retention policy of the conversations
	// (BE-162): a conversation (its messages and states with it) is hard
	// deleted 12 months after its last activity: the closing moment of a
	// closed one, the last message of one still open (an open conversation
	// silent for the period counts as closed at its last message).
	PolicyConversationClosed = "conversation_closed"
	// RetentionBatch is the most rows one purge transaction deletes.
	RetentionBatch = 1000
)

// sweeper is messaging's privacy.RetentionSweeper.
type sweeper struct{ s *service }

var _ privacy.RetentionSweeper = sweeper{}

// PolicyCodes implements privacy.RetentionSweeper.
func (sweeper) PolicyCodes() []string {
	return []string{PolicyContactRequestClosed, PolicyConversationClosed}
}

// purge is one policy's dry run (count) and batch delete.
type purge struct {
	table string
	count func(ctx context.Context, cutoff time.Time) (int, error)
	purge func(ctx context.Context, cutoff time.Time, limit int) (int, error)
}

// Sweep implements privacy.RetentionSweeper.
func (w sweeper) Sweep(ctx context.Context, p privacy.RetentionPolicy, now time.Time) (privacy.SweepResult, error) {
	var pu purge
	switch p.Code {
	case PolicyContactRequestClosed:
		pu = purge{table: "contact_requests", count: w.s.requests().CountClosedBefore, purge: w.s.requests().PurgeClosed}
	case PolicyConversationClosed:
		pu = purge{table: "conversations", count: w.s.conversations().CountInactiveBefore, purge: w.s.conversations().PurgeInactive}
	default:
		return privacy.SweepResult{}, fmt.Errorf("messaging: no retention sweep for policy %q", p.Code)
	}
	cutoff := now.Add(-p.Retention)
	res := privacy.SweepResult{PolicyCode: p.Code, Table: pu.table, Method: privacy.MethodHardDelete}
	if !p.LegalApproved {
		n, err := pu.count(ctx, cutoff)
		res.Count = n
		return res, err
	}
	n, err := db.Batches(ctx, w.s.d.Tx, RetentionBatch, func(ctx context.Context) (int, error) {
		return pu.purge(ctx, cutoff, RetentionBatch)
	})
	res.Count = n
	return res, err
}
