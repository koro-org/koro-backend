package notify

import (
	"context"
	"fmt"
	"slices"
	"time"

	"koro/internal/platform/privacy"
)

// Retention (02 §24.2; BE-057, the notify.purge row of 01 §7):
// kvkk.retention_sweep calls the sweeper once per policy. A policy that is
// not legally approved is a dry run: the sweeper only counts, inside a
// transaction kvkk always rolls back. An approved policy deletes in batches
// of RetentionBatch rows, each in its own transaction, oldest first, so an
// interrupted run continues where it stopped.

// Retention policy codes of notify (retention_policies.code).
const (
	PolicyNotificationDelivery = "notification_delivery" // 12 months after created_at
	PolicyEmailSuppression     = "email_suppression"     // 3 years after created_at
	PolicyPushToken            = "push_token"            // 90 days after last_seen_at
	PolicyInAppNotification    = "in_app_notification"   // 6 months after created_at

	// RetentionBatch is the most rows one purge transaction deletes (01 §7).
	RetentionBatch = 1000
)

// retentionTables maps each policy notify sweeps to its table.
var retentionTables = map[string]string{
	PolicyNotificationDelivery: "notification_deliveries",
	PolicyEmailSuppression:     "email_suppressions",
	PolicyPushToken:            "push_tokens",
	PolicyInAppNotification:    "in_app_notifications",
}

// RetentionStore purges the rows whose retention period ended (BE-057).
type RetentionStore interface {
	// PurgeExpired deletes up to limit rows of the policy's table whose
	// anchor is before cutoff, oldest first, and returns how many it
	// deleted. With dryRun it counts every such row and deletes nothing.
	// An unknown policy is an error.
	PurgeExpired(ctx context.Context, policy string, cutoff time.Time, limit int, dryRun bool) (int, error)
}

// sweeper is notify's privacy.RetentionSweeper.
type sweeper struct{ s *service }

var _ privacy.RetentionSweeper = sweeper{}

// PolicyCodes implements privacy.RetentionSweeper.
func (sweeper) PolicyCodes() []string {
	out := make([]string, 0, len(retentionTables))
	for code := range retentionTables {
		out = append(out, code)
	}
	slices.Sort(out)
	return out
}

// Sweep implements privacy.RetentionSweeper.
func (w sweeper) Sweep(ctx context.Context, p privacy.RetentionPolicy, now time.Time) (privacy.SweepResult, error) {
	table, ok := retentionTables[p.Code]
	if !ok {
		return privacy.SweepResult{}, fmt.Errorf("notify: no retention sweep for policy %q", p.Code)
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
	total := 0
	for {
		var n int
		err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
			var err error
			n, err = purge(ctx, RetentionBatch, false)
			return err
		})
		if err != nil {
			return total, err
		}
		total += n
		if n < RetentionBatch {
			return total, nil
		}
		if err := ctx.Err(); err != nil {
			return total, err
		}
	}
}
