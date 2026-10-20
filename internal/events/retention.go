package events

import (
	"context"
	"fmt"
	"slices"
	"time"

	"koro/internal/platform/privacy"
)

// Retention (02 §11.6, §24.2; BE-057, the events.purge_form_answers row of
// 01 §7): kvkk.retention_sweep calls the sweeper once per policy. The first
// three are anchored on the end of the event (events.ends_at): the cut-off
// is now minus the policy's period, so a moved end moves the purge with it
// and a period the lawyer changes applies to every registration. The export
// log (BE-050) is anchored on the export's created_at.
// answers_purge_at / sensitive_purge_at stay informative (the date shown to
// the registrant, written with the period of the time).
//
//	event_form_answers            answers → '{}', the encrypted special
//	                              category answers → NULL, answers_purged_at
//	                              set; the registration itself (status,
//	                              ticket, check-in) stays
//	event_form_answers_sensitive  the encrypted special category answers
//	                              → NULL (sooner: 30 days)
//	event_staff                   the staff assignments of the event go
//	event_export_log              the export rows (who exported what, why)
//	                              go 2 years after the request; a row still
//	                              holding its file waits for
//	                              events.purge_exports (event_export_file,
//	                              a scheduled job, never deferred)
//
// An unapproved policy is a dry run (count only, inside a transaction kvkk
// rolls back); an approved one works in batches of RetentionBatch rows, one
// transaction each, oldest event first.

// Retention policy codes events sweeps.
const (
	PolicyFormAnswers          = "event_form_answers"
	PolicyFormAnswersSensitive = "event_form_answers_sensitive"
	PolicyStaff                = "event_staff"

	// RetentionBatch is the most rows one purge transaction touches.
	RetentionBatch = 1000
)

var retentionTargets = map[string]struct{ table, method string }{
	PolicyFormAnswers:          {"event_registrations.answers", privacy.MethodNullColumns},
	PolicyFormAnswersSensitive: {"event_registrations.answers_sensitive_enc", privacy.MethodNullColumns},
	PolicyStaff:                {"event_staff", privacy.MethodHardDelete},
	PolicyExportLog:            {"event_data_exports", privacy.MethodHardDelete},
}

// RetentionStore is what the sweeper needs from the store (BE-057).
type RetentionStore interface {
	// PurgeExpired applies the policy to up to limit rows of events that
	// ended before cutoff, oldest end first (the export log: exports
	// created before cutoff without a file, oldest first), and returns how
	// many it changed or deleted; dryRun counts every such row and changes
	// nothing. A row already purged is not counted again. Unknown policy:
	// error.
	PurgeExpired(ctx context.Context, policy string, cutoff time.Time, limit int, dryRun bool) (int, error)
}

// sweeper is events' privacy.RetentionSweeper.
type sweeper struct{ s *service }

var _ privacy.RetentionSweeper = sweeper{}

// PolicyCodes implements privacy.RetentionSweeper.
func (sweeper) PolicyCodes() []string {
	out := make([]string, 0, len(retentionTargets))
	for code := range retentionTargets {
		out = append(out, code)
	}
	slices.Sort(out)
	return out
}

// Sweep implements privacy.RetentionSweeper.
func (w sweeper) Sweep(ctx context.Context, p privacy.RetentionPolicy, now time.Time) (privacy.SweepResult, error) {
	target, ok := retentionTargets[p.Code]
	if !ok {
		return privacy.SweepResult{}, fmt.Errorf("events: no retention sweep for policy %q", p.Code)
	}
	cutoff := now.Add(-p.Retention)
	res := privacy.SweepResult{PolicyCode: p.Code, Table: target.table, Method: target.method}
	if !p.LegalApproved {
		n, err := w.s.d.Store.PurgeExpired(ctx, p.Code, cutoff, 0, true)
		res.Count = n
		return res, err
	}
	for {
		var n int
		err := w.s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
			var err error
			n, err = w.s.d.Store.PurgeExpired(ctx, p.Code, cutoff, RetentionBatch, false)
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
