package identity

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"koro/internal/platform/db"
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

// SubscribeFileRejected registers file.rejected →
// identity.remove_rejected_document (01 §6.2: the owning module of a file
// the scan rejected drops its reference; BE-018 student documents). It is
// not wired yet: files' wiring test pins file.rejected to its own single
// subscriber (T-222). Until then a document the
// scan rejects stays in the queue, cannot be downloaded or approved
// (files.not_clean) and is rejected by the reviewer.
func (m Module) SubscribeFileRejected(r *outbox.Registry) {
	r.Subscribe(domainevent.TopicFileRejected, RemoveRejectedDocumentTaskName, m.svc.handleFileRejected, outbox.Queue(queue.Default))
}

// Tasks returns the task handlers identity enqueues itself (mechanism A).
func (m Module) Tasks() []queue.Task {
	return []queue.Task{
		{Name: SendSecurityNoticeTaskName, Handle: m.svc.handleSecurityNotice},
		{Name: SendRoleAlertTaskName, Handle: m.svc.handleRoleAlert},
		{Name: SendEduNoticeTaskName, Handle: m.svc.handleEduNotice},
	}
}

// Jobs returns the jobs of 01 §7: the cleanups (sessions and tokens give
// the retention periods session_expired and email_token) and the student
// verification jobs identity.edu_expire and identity.edu_renewal_reminders.
func (m Module) Jobs(sessions, tokens Retention) []queue.Job {
	return append([]queue.Job{
		{Name: SessionsCleanupJobName, Cron: "30 3 * * *", Queue: queue.Low, Unique: 23 * time.Hour,
			Handle: func(ctx context.Context) error {
				return m.svc.cleanup(ctx, SessionsCleanupJobName, sessions, m.svc.d.Store.DeleteEndedSessions)
			}},
		{Name: EmailTokensCleanupJobName, Cron: "35 3 * * *", Queue: queue.Low, Unique: 23 * time.Hour,
			Handle: func(ctx context.Context) error {
				return m.svc.cleanup(ctx, EmailTokensCleanupJobName, tokens, m.svc.d.Store.DeleteEndedEmailTokens)
			}},
	}, m.svc.eduJobs()...)
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
	total, err := db.Batches(ctx, s.d.Tx, cleanupBatch, func(ctx context.Context) (int, error) {
		return del(ctx, cutoff, cleanupBatch)
	})
	if err != nil {
		return fmt.Errorf("%s: %w", job, err)
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "cleanup done", slog.String("job", job), slog.Int("deleted", total))
	return nil
}
