package entitlements

import (
	"context"
	"fmt"
	"time"

	"koro/internal/platform/db"
	"koro/internal/platform/privacy"
)

// Retention (02 §10, §24.2; BE-057): an admin or promo grant goes 2 years
// after its validity ended, together with its usages (entitlement_other).
// A usage that still proves a targeted post, a dispatch or a contact
// request keeps the grant until that record goes. Order rights follow
// their order (financial_records). kvkk.retention_sweep calls the sweeper;
// an unapproved policy is a dry run (count only, inside a transaction kvkk
// rolls back), an approved one deletes in batches of at most
// RetentionBatch rows, one transaction each, oldest first.

const (
	// PolicyOther is the retention policy entitlements sweeps.
	PolicyOther = "entitlement_other"
	// RetentionBatch is the most rows (grants and usages) one purge
	// transaction deletes.
	RetentionBatch = 1000
)

// sweeper is entitlements' privacy.RetentionSweeper.
type sweeper struct{ s *service }

var _ privacy.RetentionSweeper = sweeper{}

// PolicyCodes implements privacy.RetentionSweeper.
func (sweeper) PolicyCodes() []string { return []string{PolicyOther} }

// Sweep implements privacy.RetentionSweeper.
func (w sweeper) Sweep(ctx context.Context, p privacy.RetentionPolicy, now time.Time) (privacy.SweepResult, error) {
	if p.Code != PolicyOther {
		return privacy.SweepResult{}, fmt.Errorf("entitlements: no retention sweep for policy %q", p.Code)
	}
	cutoff := now.Add(-p.Retention)
	res := privacy.SweepResult{PolicyCode: p.Code, Table: "entitlements", Method: privacy.MethodHardDelete}
	if !p.LegalApproved {
		n, err := w.s.d.Store.PurgeEndedGrants(ctx, cutoff, RetentionBatch, true)
		res.Count = n
		return res, err
	}
	// A batch takes whole grants with their usages, so it may stop short of
	// RetentionBatch rows while more are left: repeat until one is empty.
	n, err := db.Batches(ctx, w.s.d.Tx, db.UntilEmpty, func(ctx context.Context) (int, error) {
		return w.s.d.Store.PurgeEndedGrants(ctx, cutoff, RetentionBatch, false)
	})
	res.Count = n
	return res, err
}
