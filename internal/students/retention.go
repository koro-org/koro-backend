package students

import (
	"context"
	"fmt"
	"time"

	"koro/internal/platform/db"
	"koro/internal/platform/privacy"
)

// Retention (02 §9.7, §24.2; BE-046): a revoked level 3 grant is hard
// deleted 6 months after its revocation (access_grant_revoked; the
// policy's anchor ended_at is the grant's revoked_at). Active grants live
// with the account (the KVKK provider erases them). kvkk.retention_sweep
// calls the sweeper; an unapproved policy is a dry run (count only), an
// approved one deletes in batches of RetentionBatch grants, one
// transaction each, oldest revocation first.

const (
	// PolicyAccessGrantRevoked is the retention policy students sweeps.
	PolicyAccessGrantRevoked = "access_grant_revoked"
	// RetentionBatch is the most grants one purge transaction deletes.
	RetentionBatch = 1000
)

// sweeper is students' privacy.RetentionSweeper.
type sweeper struct{ s *service }

var _ privacy.RetentionSweeper = sweeper{}

// PolicyCodes implements privacy.RetentionSweeper.
func (sweeper) PolicyCodes() []string { return []string{PolicyAccessGrantRevoked} }

// Sweep implements privacy.RetentionSweeper.
func (w sweeper) Sweep(ctx context.Context, p privacy.RetentionPolicy, now time.Time) (privacy.SweepResult, error) {
	if p.Code != PolicyAccessGrantRevoked {
		return privacy.SweepResult{}, fmt.Errorf("students: no retention sweep for policy %q", p.Code)
	}
	cutoff := now.Add(-p.Retention)
	res := privacy.SweepResult{PolicyCode: p.Code, Table: "profile_access_grants", Method: privacy.MethodHardDelete}
	if !p.LegalApproved {
		n, err := w.s.d.Store.CountRevokedGrantsBefore(ctx, cutoff)
		res.Count = n
		return res, err
	}
	n, err := db.Batches(ctx, w.s.d.Tx, RetentionBatch, func(ctx context.Context) (int, error) {
		return w.s.d.Store.PurgeRevokedGrants(ctx, cutoff, RetentionBatch)
	})
	res.Count = n
	return res, err
}
