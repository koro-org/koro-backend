package consent

import (
	"context"
	"fmt"
	"time"

	"koro/internal/platform/privacy"
)

// Retention (02 §4, §24.2; BE-057): the proof of a consent chain (every
// grant and withdrawal of a user, purpose and scope, with its İYS
// submissions) goes 3 years after the chain ended in a withdrawal, or after
// the account was erased (consent_proof, 6563 / Ticari İletişim Yön.). A
// granted chain of a live account is never purged: it is the proof of the
// consent in force. kvkk.retention_sweep calls the sweeper; an unapproved
// policy is a dry run (count only, inside a transaction kvkk rolls back),
// an approved one deletes in batches of RetentionBatch consents, one
// transaction each, oldest first.

const (
	// PolicyConsentProof is the retention policy consent sweeps.
	PolicyConsentProof = "consent_proof"
	// RetentionBatch is the most consents one purge transaction deletes.
	RetentionBatch = 1000
)

// sweeper is consent's privacy.RetentionSweeper.
type sweeper struct{ s *service }

var _ privacy.RetentionSweeper = sweeper{}

// PolicyCodes implements privacy.RetentionSweeper.
func (sweeper) PolicyCodes() []string { return []string{PolicyConsentProof} }

// Sweep implements privacy.RetentionSweeper.
func (w sweeper) Sweep(ctx context.Context, p privacy.RetentionPolicy, now time.Time) (privacy.SweepResult, error) {
	if p.Code != PolicyConsentProof {
		return privacy.SweepResult{}, fmt.Errorf("consent: no retention sweep for policy %q", p.Code)
	}
	cutoff := now.Add(-p.Retention)
	res := privacy.SweepResult{PolicyCode: p.Code, Table: "consents", Method: privacy.MethodHardDelete}
	if !p.LegalApproved {
		n, err := w.s.d.Store.PurgeProof(ctx, cutoff, RetentionBatch, true)
		res.Count = n
		return res, err
	}
	for {
		var n int
		err := w.s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
			var err error
			n, err = w.s.d.Store.PurgeProof(ctx, cutoff, RetentionBatch, false)
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
