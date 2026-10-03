package kvkk

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/notify"
	"koro/internal/platform/clock"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/obs"
	"koro/internal/platform/queue"
)

// Scheduled jobs (01 §7).
const (
	// DeadlinesJobName warns the DPO about requests with ≤ 7 days left
	// (daily 09:00, default).
	DeadlinesJobName = "kvkk.dsr_deadlines"
	// PurgeExportsJobName deletes export files past their retention
	// (hourly :27, low; T-83: 7 days, never deferred).
	PurgeExportsJobName = "kvkk.purge_exports"

	deadlinesCron   = "0 9 * * *"
	deadlinesUnique = 23 * time.Hour
	purgeCron       = "27 * * * *"
	purgeUnique     = 55 * time.Minute
	batchSize       = 1000 // 01 §7: ≤ 1000 rows per batch
)

// E-mail templates (internal/notify/templates/kvkk, BE-001 R8).
const (
	tmplSubmitted = "kvkk.dsr_submitted"
	tmplDeadlines = "kvkk.dsr_deadlines"
	tmplCompleted = "kvkk.dsr_completed"
)

// dateOf formats t as the Europe/Istanbul calendar date of the e-mails.
func dateOf(t time.Time) string { return t.In(clock.Istanbul()).Format("2006-01-02") }

// send hands m to notify; a permanent refusal (suppressed address, bad
// message) is logged and swallowed, anything else is retried.
func (s *service) send(ctx context.Context, m notify.Message, dsrID uuid.UUID) error {
	if _, err := s.d.Mailer.Send(ctx, m); err != nil {
		if notify.IsPermanent(err) {
			s.d.Log.LogAttrs(ctx, slog.LevelWarn, "kvkk e-mail not sent", slog.String("template", m.Template),
				slog.String("dsr_id", dsrID.String()), slog.String("err", obs.Scrub(err.Error())))
			return nil
		}
		return err
	}
	return nil
}

// handleSubmitted is the dsr.submitted subscription (kvkk.notify_dpo): the
// DPO mailbox learns the request ID, type and due date — never its
// content.
func (s *service) handleSubmitted(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.DSRSubmitted](e)
	if err != nil {
		return queue.Permanent(err)
	}
	d, err := s.d.Store.Get(ctx, p.DSRID)
	switch {
	case errors.Is(err, ErrNotFound):
		return queue.Permanent(fmt.Errorf("kvkk: submitted request %s not found", p.DSRID))
	case err != nil:
		return err
	}
	if s.d.DPOEmail == "" {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "no DPO mailbox configured (KVKK_DPO_EMAIL): new request not mailed",
			slog.String("dsr_id", d.ID.String()))
		return nil
	}
	return s.send(ctx, notify.Message{
		ToEmail: s.d.DPOEmail, Channel: notify.ChannelEmail, Category: notify.CategoryTransactional,
		Template: tmplSubmitted, Locale: "tr", DedupeKey: "kvkk.dsr_submitted:" + d.ID.String(),
		Data: map[string]any{"dsr_id": d.ID.String(), "type": string(d.Type), "channel": string(d.Channel),
			"due_date": dateOf(d.DueAt)},
	}, d.ID)
}

// handleCompleted is the dsr.completed subscription (kvkk.finish_request):
// the export is built first (if the answer is one), then the subject is
// told by e-mail.
func (s *service) handleCompleted(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.DSRCompleted](e)
	if err != nil {
		return queue.Permanent(err)
	}
	d, err := s.d.Store.Get(ctx, p.DSRID)
	switch {
	case errors.Is(err, ErrNotFound):
		return queue.Permanent(fmt.Errorf("kvkk: completed request %s not found", p.DSRID))
	case err != nil:
		return err
	}
	if p.Outcome == OutcomeExport {
		if err := s.buildExport(ctx, d); err != nil {
			return err
		}
	}
	return s.notifySubject(ctx, d, p.Outcome)
}

// notifySubject e-mails the applicant that the request was answered: the
// account holder (while the account lives) or the address the DPO entered.
// The dedupe key is shared by every path, so the subject gets one e-mail.
func (s *service) notifySubject(ctx context.Context, d DSR, outcome string) error {
	m := notify.Message{
		Channel: notify.ChannelEmail, Category: notify.CategoryTransactional, Template: tmplCompleted,
		DedupeKey: "kvkk.dsr_completed:" + d.ID.String(), Locale: "tr",
		Data: map[string]any{"name": "", "dsr_id": d.ID.String(), "type": string(d.Type), "outcome": outcome},
	}
	switch {
	case d.UserID != nil:
		c, found, err := s.d.Users.Contact(ctx, *d.UserID)
		if err != nil {
			return err
		}
		if !found { // erased (the erasure run mailed before anonymising) or unknown
			return nil
		}
		m.ToEmail, m.Locale, m.Data["name"] = c.Email, c.Locale, c.FirstName
	case d.RequesterEmail != "":
		m.ToEmail, m.Data["name"] = d.RequesterEmail, d.RequesterName
	default:
		return nil
	}
	return s.send(ctx, m, d.ID)
}

// deadlinesJob is kvkk.dsr_deadlines (01 §7).
func (s *service) deadlinesJob() queue.Job {
	return queue.Job{Name: DeadlinesJobName, Cron: deadlinesCron, Queue: queue.Default, Unique: deadlinesUnique,
		Handle: s.warnDeadlines}
}

// dueSoon returns the open requests whose deadline is at most WarnBefore
// away (or already passed), soonest first.
func (s *service) dueSoon(ctx context.Context, now time.Time) ([]DSR, error) {
	var out []DSR
	var after *After
	for {
		rows, err := s.d.Store.DueBy(ctx, now.Add(WarnBefore), after, batchSize)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
		if len(rows) < batchSize {
			return out, nil
		}
		last := rows[len(rows)-1]
		after = &After{At: last.DueAt, ID: last.ID}
	}
}

// warnDeadlines sends the DPO one digest of the requests with ≤ 7 days
// left (overdue ones included) and logs the counts.
func (s *service) warnDeadlines(ctx context.Context) error {
	now := s.now()
	due, err := s.dueSoon(ctx, now)
	if err != nil || len(due) == 0 {
		return err
	}
	items := make([]map[string]any, len(due))
	overdue := 0
	for i, d := range due {
		left := d.DaysLeft(now)
		if left <= 0 {
			overdue++
		}
		items[i] = map[string]any{"dsr_id": d.ID.String(), "type": string(d.Type), "status": string(d.Status),
			"due_date": dateOf(d.DueAt), "days_left": left}
	}
	s.d.Log.LogAttrs(ctx, slog.LevelWarn, "data subject requests near their legal deadline",
		slog.Int("count", len(due)), slog.Int("overdue", overdue))
	if s.d.DPOEmail == "" {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "no DPO mailbox configured (KVKK_DPO_EMAIL): deadline digest not mailed")
		return nil
	}
	return s.send(ctx, notify.Message{
		ToEmail: s.d.DPOEmail, Channel: notify.ChannelEmail, Category: notify.CategoryTransactional,
		Template: tmplDeadlines, Locale: "tr", DedupeKey: "kvkk.dsr_deadlines:" + dateOf(now),
		Data: map[string]any{"count": len(due), "overdue": overdue, "items": items},
	}, uuid.Nil())
}

// purgeExportsJob is kvkk.purge_exports.
func (s *service) purgeExportsJob() queue.Job {
	return queue.Job{Name: PurgeExportsJobName, Cron: purgeCron, Queue: queue.Low, Unique: purgeUnique,
		Handle: func(ctx context.Context) error { _, err := s.purgeExports(ctx); return err }}
}

// purgeExports deletes every export file of a request completed more than
// ExportRetention ago: the reference is cleared and the file deleted in
// one transaction per request (files removes the objects after the
// commit), audited as kvkk.retention.sweep (policy dsr_export_file).
func (s *service) purgeExports(ctx context.Context) (int, error) {
	sys := authz.SystemActor(purgeJob)
	if err := authz.Can(ctx, sys, authz.KvkkRetentionSweep, authz.Resource{}); err != nil {
		return 0, err
	}
	cutoff := s.now().Add(-s.d.ExportRetention)
	n, after := 0, uuid.Nil()
	for {
		rows, err := s.d.Store.ExportsCompletedBefore(ctx, cutoff, after, batchSize)
		if err != nil {
			return n, err
		}
		for _, d := range rows {
			after = d.ID
			purged, err := s.purgeOne(ctx, sys, d.ID, cutoff)
			if err != nil {
				return n, err
			}
			if purged {
				n++
			}
		}
		if len(rows) < batchSize {
			break
		}
	}
	if n > 0 {
		s.d.Log.LogAttrs(ctx, slog.LevelInfo, "expired data exports deleted", slog.Int("count", n))
	}
	return n, nil
}

func (s *service) purgeOne(ctx context.Context, sys authz.Actor, id uuid.UUID, cutoff time.Time) (bool, error) {
	purged := false
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		d, err := s.d.Store.Lock(ctx, id)
		if err != nil {
			return err
		}
		if d.ExportFileID == nil || d.CompletedAt == nil || d.CompletedAt.After(cutoff) {
			return nil
		}
		fileID := *d.ExportFileID
		d.ExportFileID = nil
		if err := s.d.Store.Update(ctx, d); err != nil {
			return err
		}
		if err := s.d.RemoveFile(ctx, sys, fileID); err != nil {
			return err
		}
		purged = true
		return s.d.Audit.Log(ctx, event(sys, authz.KvkkRetentionSweep, d, map[string]any{
			"policy_code": "dsr_export_file", "file_id": fileID.String()}))
	})
	return purged, err
}
