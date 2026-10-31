package consent

import (
	"context"
	"fmt"
	"time"

	"koro/internal/platform/db"
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
// transaction each, oldest first. The terms of use acceptances of an
// account go 10 years after its erasure (terms_acceptance, BE-118; TBK
// m.146, Q-18, BE-152), the same way: never while the account lives.

const (
	// PolicyConsentProof is the retention policy of the consent ledger.
	PolicyConsentProof = "consent_proof"
	// PolicyTermsAcceptance is the retention policy of the terms of use
	// acceptances (000148).
	PolicyTermsAcceptance = "terms_acceptance"
	// RetentionBatch is the most consents one purge transaction deletes.
	RetentionBatch = 1000
)

// sweeper is consent's privacy.RetentionSweeper.
type sweeper struct{ s *service }

var _ privacy.RetentionSweeper = sweeper{}

// PolicyCodes implements privacy.RetentionSweeper.
func (sweeper) PolicyCodes() []string { return []string{PolicyConsentProof, PolicyTermsAcceptance} }

// Sweep implements privacy.RetentionSweeper.
func (w sweeper) Sweep(ctx context.Context, p privacy.RetentionPolicy, now time.Time) (privacy.SweepResult, error) {
	var (
		table string
		purge func(ctx context.Context, cutoff time.Time, limit int, dryRun bool) (int, error)
	)
	switch p.Code {
	case PolicyConsentProof:
		table, purge = "consents", w.s.d.Store.PurgeProof
	case PolicyTermsAcceptance:
		table, purge = "terms_acceptances", w.s.d.Store.PurgeTerms
	default:
		return privacy.SweepResult{}, fmt.Errorf("consent: no retention sweep for policy %q", p.Code)
	}
	cutoff := now.Add(-p.Retention)
	res := privacy.SweepResult{PolicyCode: p.Code, Table: table, Method: privacy.MethodHardDelete}
	if !p.LegalApproved {
		n, err := purge(ctx, cutoff, RetentionBatch, true)
		res.Count = n
		return res, err
	}
	n, err := db.Batches(ctx, w.s.d.Tx, RetentionBatch, func(ctx context.Context) (int, error) {
		return purge(ctx, cutoff, RetentionBatch, false)
	})
	res.Count = n
	return res, err
}
