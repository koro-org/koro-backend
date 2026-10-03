package identity

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
)

// Scheduled jobs (01 §7).
const (
	SessionsCleanupJobName    = "identity.sessions_cleanup"     // 03:30, low
	EmailTokensCleanupJobName = "identity.email_tokens_cleanup" // 03:35, low

	// DefaultRetention is the retention of ended sessions and tokens
	// (retention_policies session_expired / email_token: 30 days).
	DefaultRetention = 30 * 24 * time.Hour

	cleanupBatch = 1000 // rows per transaction (01 §7)
)

// Retention returns a retention period at run time (hooks.retention, then
// the default).
type Retention func(ctx context.Context) (time.Duration, error)

// Subscribe registers the outbox subscription of identity (01 §6.2):
// identity.email_token_issued → identity.send_email_token.
func (m Module) Subscribe(r *outbox.Registry) {
	r.Subscribe(domainevent.TopicIdentityEmailTokenIssued, SendEmailTokenTaskName, m.svc.handleEmailTokenIssued, outbox.Queue(queue.Default))
}

// Tasks returns the task handlers identity enqueues itself (mechanism A).
func (m Module) Tasks() []queue.Task {
	return []queue.Task{
		{Name: SendSecurityNoticeTaskName, Handle: m.svc.handleSecurityNotice},
		{Name: SendRoleAlertTaskName, Handle: m.svc.handleRoleAlert},
	}
}

// Jobs returns the cleanup jobs of 01 §7. sessions and tokens give the
// retention periods (session_expired, email_token).
func (m Module) Jobs(sessions, tokens Retention) []queue.Job {
	return []queue.Job{
		{Name: SessionsCleanupJobName, Cron: "30 3 * * *", Queue: queue.Low, Unique: 23 * time.Hour,
			Handle: func(ctx context.Context) error {
				return m.svc.cleanup(ctx, SessionsCleanupJobName, sessions, m.svc.d.Store.DeleteEndedSessions)
			}},
		{Name: EmailTokensCleanupJobName, Cron: "35 3 * * *", Queue: queue.Low, Unique: 23 * time.Hour,
			Handle: func(ctx context.Context) error {
				return m.svc.cleanup(ctx, EmailTokensCleanupJobName, tokens, m.svc.d.Store.DeleteEndedEmailTokens)
			}},
	}
}

// cleanup deletes rows that ended before now − retention, in batches of
// cleanupBatch rows per transaction (idempotent).
func (s *service) cleanup(ctx context.Context, job string, retention Retention, del func(context.Context, time.Time, int) (int, error)) error {
	period := DefaultRetention
	if retention != nil {
		d, err := retention(ctx)
		if err != nil {
			return err
		}
		if d > 0 {
			period = d
		}
	}
	cutoff := s.now().Add(-period)
	total := 0
	for {
		var n int
		err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
			var err error
			n, err = del(ctx, cutoff, cleanupBatch)
			return err
		})
		if err != nil {
			return fmt.Errorf("%s: %w", job, err)
		}
		total += n
		if n < cleanupBatch || ctx.Err() != nil {
			break
		}
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "cleanup done", slog.String("job", job), slog.Int("deleted", total))
	return ctx.Err()
}
