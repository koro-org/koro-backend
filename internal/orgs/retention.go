package orgs

import (
	"context"
	"fmt"
	"time"

	"koro/internal/platform/db"
	"koro/internal/platform/privacy"
)

// Retention (02 §7.3, §24.2; BE-057): kvkk.retention_sweep calls the
// sweeper once per policy. An unapproved policy is a dry run (count only,
// inside a transaction kvkk rolls back); an approved one deletes in batches
// of RetentionBatch rows, each in its own transaction, oldest first, so an
// interrupted run goes on next time.
//
// membership_ended_sensitive (0) is not swept: a sensitive club's
// membership is deleted in the transaction that ends it, and the ended rows
// of a club that becomes sensitive by orgs.purge_sensitive_memberships, both
// regardless of the legal approval (T-83: data minimisation of special
// category data is not deferred).

// Retention policy codes orgs sweeps (retention_policies.code).
const (
	PolicyMembershipEnded = "membership_ended" // 6 months after ended_at
	PolicyOrgInvitation   = "org_invitation"   // 30 days after it closed

	// RetentionBatch is the most rows one purge transaction deletes (01 §7).
	RetentionBatch = 1000
)

var retentionTables = map[string]string{
	PolicyMembershipEnded: "org_members",
	PolicyOrgInvitation:   "org_invitations",
}

// sweeper is orgs' privacy.RetentionSweeper.
type sweeper struct{ s *service }

var _ privacy.RetentionSweeper = sweeper{}

// PolicyCodes implements privacy.RetentionSweeper.
func (sweeper) PolicyCodes() []string { return []string{PolicyMembershipEnded, PolicyOrgInvitation} }

// Sweep implements privacy.RetentionSweeper.
func (w sweeper) Sweep(ctx context.Context, p privacy.RetentionPolicy, now time.Time) (privacy.SweepResult, error) {
	table, ok := retentionTables[p.Code]
	if !ok {
		return privacy.SweepResult{}, fmt.Errorf("orgs: no retention sweep for policy %q", p.Code)
	}
	cutoff := now.Add(-p.Retention)
	n, err := w.s.purgeBatches(ctx, p.LegalApproved, func(ctx context.Context, limit int, dry bool) (int, error) {
		return w.s.d.Store.PurgeExpired(ctx, p.Code, cutoff, limit, dry)
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
	return db.Batches(ctx, s.d.Tx, RetentionBatch, func(ctx context.Context) (int, error) {
		return purge(ctx, RetentionBatch, false)
	})
}
