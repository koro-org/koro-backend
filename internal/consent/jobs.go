package consent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"koro/internal/platform/domainevent"
	"koro/internal/platform/obs"
	"koro/internal/platform/queue"
)

// Topic, task and job names (01 §6.2, §7).
const (
	// TopicRecorded is the event of a commercial consent (01 §6.2, O).
	TopicRecorded = domainevent.TopicConsentRecorded
	// SubmitIYSTaskName is the outbox subscription of consent.recorded.
	SubmitIYSTaskName = "consent.submit_iys"
	// ReconcileJobName retries open İYS submissions (01 §7: daily 05:30, default).
	ReconcileJobName = "consent.iys_reconcile"

	// ReconcileGrace leaves fresh submissions to their task: the job only
	// takes rows untouched for this long (a lost event, exhausted retries).
	ReconcileGrace = 10 * time.Minute

	reconcileCron   = "30 5 * * *"
	reconcileUnique = 23 * time.Hour
	batchSize       = 1000 // 01 §7: ≤ 1000 rows per batch
	maxLastError    = 2000 // iys_submissions_last_error_chk
	// alarmAttempts raises the alarm flag on a submission that keeps
	// failing (6563: İYS must know the consent state).
	alarmAttempts = 10
)

// handleRecorded is the consent.recorded subscription (consent.submit_iys).
// The event is written for every purpose (BE-104); only a requires_iys
// purpose has a submission to deliver, every other purpose is a no-op.
func (s *service) handleRecorded(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.ConsentRecorded](e)
	if err != nil {
		return queue.Permanent(err)
	}
	sub, err := s.d.Store.SubmissionOf(ctx, p.ConsentID)
	switch {
	case errors.Is(err, ErrNotFound):
		return s.noSubmission(ctx, p)
	case err != nil:
		return err
	}
	return s.deliver(ctx, sub)
}

// noSubmission decides an event without a submission: nothing to send for
// a purpose without İYS; for a requires_iys purpose (the submission is
// written with the event in one transaction) or an unknown one it is a
// bug, not something a retry fixes.
func (s *service) noSubmission(ctx context.Context, p domainevent.ConsentRecorded) error {
	def, err := s.d.Store.Purpose(ctx, Purpose(p.Purpose))
	switch {
	case errors.Is(err, ErrUnknownPurpose):
		return queue.Permanent(fmt.Errorf("consent: event of consent %s names unknown purpose %q", p.ConsentID, p.Purpose))
	case err != nil:
		return err
	case def.RequiresIYS:
		return queue.Permanent(fmt.Errorf("consent: no İYS submission for consent %s", p.ConsentID))
	}
	return nil
}

// deliver sends one open submission to İYS outside any transaction (05 §6.2:
// no external I/O in a transaction) and records the outcome on the locked
// row. It is idempotent: a submission that is no longer open is left alone.
// A failed delivery returns the error so the task is retried with back-off;
// consent.iys_reconcile picks up whatever stays open.
func (s *service) deliver(ctx context.Context, sub Submission) error {
	if !sub.Status.open() {
		return nil
	}
	c, err := s.consentOf(ctx, sub.ConsentID)
	if err != nil {
		return err
	}
	var (
		receipt IYSReceipt
		sendErr error
		skipped = !s.d.IYS.Real()
	)
	if !skipped {
		receipt, sendErr = s.d.IYS.Submit(ctx, IYSRecord{
			SubmissionID: sub.ID, ConsentID: c.ID, UserID: c.UserID, Channel: sub.Channel, Action: c.Action, Source: c.Channel,
			OccurredAt: c.OccurredAt,
		})
	}
	var final Submission
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		cur, err := s.d.Store.LockSubmission(ctx, sub.ID)
		if err != nil {
			return err
		}
		if !cur.Status.open() { // a concurrent delivery finished first
			final = cur
			return nil
		}
		switch {
		case skipped:
			msg := "no real İYS client configured (" + s.d.IYS.Name() + ")"
			cur.Status, cur.LastError = SubmissionSkipped, &msg
		case sendErr != nil:
			msg := cut(obs.Scrub(sendErr.Error()), maxLastError)
			cur.Status, cur.LastError = SubmissionFailed, &msg
			cur.Attempts++
		default:
			now := s.now()
			tx := receipt.TransactionID
			cur.Status, cur.SentAt, cur.LastError = SubmissionSent, &now, nil
			cur.IYSTransactionID = nullable(tx)
			cur.Attempts++
		}
		final = cur
		return s.d.Store.UpdateSubmission(ctx, cur)
	})
	if err != nil {
		return err
	}
	attrs := []slog.Attr{
		slog.String("submission_id", sub.ID.String()), slog.String("consent_id", c.ID.String()),
		slog.String("channel", string(sub.Channel)), slog.String("status", string(final.Status)),
		slog.Int("attempts", final.Attempts), slog.String("client", s.d.IYS.Name()),
	}
	switch {
	case sendErr != nil && final.Status == SubmissionFailed:
		permanent := errors.Is(sendErr, ErrIYSPermanent)
		level := slog.LevelWarn
		if final.Attempts >= alarmAttempts || permanent {
			level, attrs = slog.LevelError, append(attrs, slog.Bool("alarm", true), slog.Bool("permanent", permanent))
		}
		s.d.Log.LogAttrs(ctx, level, "iys submission failed", attrs...)
		err := fmt.Errorf("consent: İYS submission %s: %s", sub.ID, cut(obs.Scrub(sendErr.Error()), maxLastError))
		if permanent {
			return queue.Permanent(err) // a task retry cannot help; the DPO is alarmed
		}
		return err
	default:
		s.d.Log.LogAttrs(ctx, slog.LevelInfo, "iys submission done", attrs...)
	}
	return nil
}

// consentOf reads the consent row of a submission.
func (s *service) consentOf(ctx context.Context, id uuid.UUID) (Consent, error) {
	c, err := s.d.Store.ConsentByID(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return Consent{}, queue.Permanent(fmt.Errorf("consent: submission of missing consent %s", id))
	}
	return c, err
}

// reconcileJob is consent.iys_reconcile (01 §7): every pending or failed
// submission untouched for ReconcileGrace is delivered again.
func (s *service) reconcileJob() queue.Job {
	return queue.Job{
		Name: ReconcileJobName, Cron: reconcileCron, Queue: queue.Default, Unique: reconcileUnique,
		Handle: s.reconcile,
	}
}

func (s *service) reconcile(ctx context.Context) error {
	cutoff := s.now().Add(-ReconcileGrace)
	sent, failed, after := 0, 0, uuid.Nil()
	for {
		batch, err := s.d.Store.DueSubmissions(ctx, cutoff, after, batchSize)
		if err != nil {
			return err
		}
		for _, sub := range batch {
			after = sub.ID
			if err := s.deliver(ctx, sub); err != nil {
				if ctx.Err() != nil {
					return err
				}
				failed++ // recorded on the row (or logged); the next run tries again
				continue
			}
			sent++
		}
		if len(batch) < batchSize {
			break
		}
	}
	level := slog.LevelInfo
	if failed > 0 {
		level = slog.LevelWarn
	}
	s.d.Log.LogAttrs(ctx, level, "iys reconcile done", slog.Int("delivered", sent), slog.Int("failed", failed))
	return nil
}
