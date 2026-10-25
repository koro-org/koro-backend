package identity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"koro/internal/authz"
	"koro/internal/platform/db"
	"koro/internal/platform/privacy"
)

// Retention (02 §5, §24.2; BE-057): kvkk.retention_sweep calls the sweeper
// once per policy. An unapproved policy is a dry run (count only, inside a
// transaction kvkk rolls back); an approved one works in batches of
// RetentionBatch rows, each in its own transaction, oldest first, so an
// interrupted run continues next time.
//
//	edu_document              the student document file of a decided
//	                          document verification goes 30 days after the
//	                          decision (verified_at, else the last update);
//	                          the row stays, document_file_id = NULL
//	edu_verification_history  a verification that ended (rejected, expired
//	                          at expires_at, revoked, superseded at the last
//	                          update) goes after a year, with its file
//	role_history              a platform role assignment revoked or
//	                          rejected goes after 3 years
//	privileged_action_history a four-eyes request against a privileged
//	                          account goes 3 years after its approval or
//	                          lapse (000121 privileged_account_actions_purge:
//	                          app_rw holds no DELETE)
//
// session_expired and email_token stay the cleanup jobs of jobs.go
// (operational security data; not swept here).

// Retention policy codes identity sweeps.
const (
	PolicyEduDocument             = "edu_document"
	PolicyEduVerificationHistory  = "edu_verification_history"
	PolicyRoleHistory             = "role_history"
	PolicyPrivilegedActionHistory = "privileged_action_history"

	// RetentionBatch is the most rows one purge transaction handles (01 §7).
	RetentionBatch = 1000

	retentionJob = "identity.retention"
)

// RetentionStore is what the sweeper needs from the store (BE-057).
type RetentionStore interface {
	// PurgeExpired deletes up to limit rows of PolicyRoleHistory or
	// PolicyPrivilegedActionHistory whose anchor is before cutoff, oldest
	// first, and returns how many; dryRun counts every such row and
	// deletes nothing. Unknown policy: error.
	PurgeExpired(ctx context.Context, policy string, cutoff time.Time, limit int, dryRun bool) (int, error)
	// EduRetentionDue returns up to limit verifications the policy
	// (PolicyEduDocument or PolicyEduVerificationHistory) is due for at
	// cutoff, oldest anchor first, locked for the transaction (rows locked
	// elsewhere are skipped). PolicyEduDocument: decided document
	// verifications still holding their file.
	EduRetentionDue(ctx context.Context, policy string, cutoff time.Time, limit int) ([]EduRecord, error)
	// CountEduRetentionDue counts what EduRetentionDue would return
	// without a limit (dry run).
	CountEduRetentionDue(ctx context.Context, policy string, cutoff time.Time) (int, error)
}

var retentionTables = map[string]string{
	PolicyEduDocument:             "files:edu_document",
	PolicyEduVerificationHistory:  "edu_verifications",
	PolicyRoleHistory:             "platform_role_assignments",
	PolicyPrivilegedActionHistory: "privileged_account_actions",
}

// errNoFiles: a document cannot be deleted without the files module.
var errNoFiles = errors.New("identity: student documents cannot be purged: files not wired")

// sweeper is identity's privacy.RetentionSweeper.
type sweeper struct{ s *service }

var _ privacy.RetentionSweeper = sweeper{}

// PolicyCodes implements privacy.RetentionSweeper.
func (sweeper) PolicyCodes() []string {
	return []string{PolicyEduDocument, PolicyEduVerificationHistory, PolicyPrivilegedActionHistory, PolicyRoleHistory}
}

// Sweep implements privacy.RetentionSweeper.
func (w sweeper) Sweep(ctx context.Context, p privacy.RetentionPolicy, now time.Time) (privacy.SweepResult, error) {
	table, ok := retentionTables[p.Code]
	if !ok {
		return privacy.SweepResult{}, fmt.Errorf("identity: no retention sweep for policy %q", p.Code)
	}
	res := privacy.SweepResult{PolicyCode: p.Code, Table: table, Method: privacy.MethodHardDelete}
	cutoff := now.Add(-p.Retention)
	var purge func(ctx context.Context, limit int, dry bool) (int, error)
	switch p.Code {
	case PolicyEduDocument, PolicyEduVerificationHistory:
		if p.Code == PolicyEduDocument {
			res.Method = privacy.MethodFileDelete
		}
		purge = func(ctx context.Context, limit int, dry bool) (int, error) {
			return w.s.purgeEdu(ctx, p.Code, cutoff, limit, dry)
		}
	default:
		purge = func(ctx context.Context, limit int, dry bool) (int, error) {
			return w.s.d.Store.PurgeExpired(ctx, p.Code, cutoff, limit, dry)
		}
	}
	n, err := w.s.purgeBatches(ctx, p.LegalApproved, purge)
	res.Count = n
	return res, err
}

// purgeEdu handles one batch of the student verification policies: the
// row's reference to its document is cleared (edu_document) or the row
// deleted (history), and the document file is deleted in the same
// transaction (01 §6.3 identity → files.Files.Delete; the objects go after
// the commit).
func (s *service) purgeEdu(ctx context.Context, policy string, cutoff time.Time, limit int, dry bool) (int, error) {
	if dry {
		return s.d.Store.CountEduRetentionDue(ctx, policy, cutoff)
	}
	rows, err := s.d.Store.EduRetentionDue(ctx, policy, cutoff, limit)
	if err != nil {
		return 0, err
	}
	sys := authz.SystemActor(retentionJob)
	for _, v := range rows {
		file := v.DocumentFileID
		if file != nil && s.d.Files == nil {
			return 0, errNoFiles
		}
		if policy == PolicyEduDocument {
			v.DocumentFileID = nil
			if err := s.d.Store.UpdateEduVerification(ctx, v); err != nil {
				return 0, err
			}
		} else if _, err := s.d.Store.DeleteEduVerification(ctx, v.ID); err != nil {
			return 0, err
		}
		if file != nil {
			if err := s.d.Files.Delete(ctx, sys, *file); err != nil {
				return 0, err
			}
		}
	}
	return len(rows), nil
}

// purgeBatches runs purge: unapproved, one counting call; approved, one
// transaction per batch of RetentionBatch rows until a batch comes back
// short (or ctx ends). It returns the rows counted or handled.
func (s *service) purgeBatches(ctx context.Context, approved bool, purge func(ctx context.Context, limit int, dry bool) (int, error)) (int, error) {
	if !approved {
		return purge(ctx, 0, true)
	}
	return db.Batches(ctx, s.d.Tx, RetentionBatch, func(ctx context.Context) (int, error) {
		return purge(ctx, RetentionBatch, false)
	})
}
