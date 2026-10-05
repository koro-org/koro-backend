package directory

import (
	"context"
	"fmt"
	"time"

	"koro/internal/authz"
	"koro/internal/platform/privacy"
)

// Retention (02 §8.6, §24.2; BE-057): a decided club claim (approved,
// rejected or withdrawn) goes a year after the decision, with its evidence
// file (01 §6.3 directory → files.Files.Delete, same transaction; the
// objects go after the commit). The role an approved claim granted is not
// touched. kvkk.retention_sweep calls the sweeper; an unapproved policy is
// a dry run (count only, inside a transaction kvkk rolls back), an approved
// one deletes in batches of RetentionBatch claims, one transaction each,
// oldest first.

const (
	// PolicyClubClaim is the retention policy directory sweeps.
	PolicyClubClaim = "club_claim"
	// RetentionBatch is the most claims one purge transaction deletes.
	RetentionBatch = 1000

	retentionJob = "directory.retention"
)

// sweeper is directory's privacy.RetentionSweeper.
type sweeper struct{ s *service }

var _ privacy.RetentionSweeper = sweeper{}

// PolicyCodes implements privacy.RetentionSweeper.
func (sweeper) PolicyCodes() []string { return []string{PolicyClubClaim} }

// Sweep implements privacy.RetentionSweeper.
func (w sweeper) Sweep(ctx context.Context, p privacy.RetentionPolicy, now time.Time) (privacy.SweepResult, error) {
	if p.Code != PolicyClubClaim {
		return privacy.SweepResult{}, fmt.Errorf("directory: no retention sweep for policy %q", p.Code)
	}
	cutoff := now.Add(-p.Retention)
	res := privacy.SweepResult{PolicyCode: p.Code, Table: "club_claims", Method: privacy.MethodHardDelete}
	if !p.LegalApproved {
		n, err := w.s.d.Store.CountClaimsDecidedBefore(ctx, cutoff)
		res.Count = n
		return res, err
	}
	sys := authz.SystemActor(retentionJob)
	for {
		var n int
		err := w.s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
			claims, err := w.s.d.Store.ClaimsDecidedBefore(ctx, cutoff, RetentionBatch)
			if err != nil {
				return err
			}
			for _, c := range claims {
				if err := w.s.d.Store.DeleteClaim(ctx, c.ID); err != nil {
					return err
				}
				if c.EvidenceFileID != nil {
					if err := w.s.removeFile(ctx, sys, *c.EvidenceFileID); err != nil {
						return err
					}
				}
			}
			n = len(claims)
			return nil
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
