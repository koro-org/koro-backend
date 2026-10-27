package students

import (
	"context"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/notify"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/queue"
)

// Suspension of level 3 grants (BE-077; user decision 2026-10-21, KVKK md.
// 11). A student gave each grant while verified; when the verification
// lapses every active grant is suspended: the company sees the student as
// unknown (404) and is never told. A suspended grant never becomes active
// again by itself — not when the student verifies again either (consent
// principle): the student revokes it, or accepts a new contact request of
// the company after verifying again, which ends the suspended grant and
// gives a fresh one (Grant). The student is told in-app; the company is not.
//
// Until the suspension runs, the company views already hide a student
// without an active verification (companyTarget, cardSources, Has), so the
// access ends at the verification's expiry, not at the nightly
// identity.edu_expire run. In v1 only the expiry ends a verification
// (identity writes no 'revoked' row); an operation that withdraws one must
// emit user.edu_verification_lapsed too, or the grants are not suspended
// (T-333).
const (
	// SuspendLapsedGrantsTaskName consumes user.edu_verification_lapsed:
	// the verification's year ran out (identity.edu_expire).
	SuspendLapsedGrantsTaskName = "students.suspend_lapsed_grants"
	// SuspendGapGrantsTaskName consumes user.edu_verified: a verification
	// that replaced none in effect (renewal false) follows a gap even when
	// identity.edu_expire never lapsed the old one (a renewal after its
	// expiry, before the nightly run); the grants given before it are
	// suspended as if the lapse had been recorded.
	SuspendGapGrantsTaskName = "students.suspend_gap_grants"

	// auditGrantSuspend is the audit action of a suspension (03 §4.1
	// format; a system write, not an HTTP action).
	auditGrantSuspend = "students.access_grant.suspend"
	// templateGrantsSuspended is the student's in-app notice
	// (internal/notify/templates/students).
	templateGrantsSuspended = "students.access_grants_suspended"
)

// handleLapsedGrants is students.suspend_lapsed_grants
// (user.edu_verification_lapsed).
func (s *service) handleLapsedGrants(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.UserEduVerificationLapsed](e)
	if err != nil {
		return queue.Permanent(err)
	}
	return s.suspendGrants(ctx, p.UserID, s.eventTime(e), SuspendLapsedGrantsTaskName, "verification_lapsed", e.ID)
}

// handleGapGrants is students.suspend_gap_grants (user.edu_verified): a
// renewal of a verification in effect changes nothing; any other
// verification suspends the grants given before it that are still active
// (none when the lapse was already recorded, nor for a first
// verification).
func (s *service) handleGapGrants(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.UserEduVerified](e)
	if err != nil {
		return queue.Permanent(err)
	}
	if p.Renewal {
		return nil
	}
	return s.suspendGrants(ctx, p.UserID, s.eventTime(e), SuspendGapGrantsTaskName, "verification_gap", e.ID)
}

// eventTime is when identity recorded the event (outbox occurred_at) in
// the database's precision; the service clock when the envelope has none.
// Grants given after it (a new acceptance under a new verification) are
// never suspended by it, whenever the task runs.
func (s *service) eventTime(e domainevent.Envelope) time.Time {
	if e.OccurredAt.IsZero() {
		return s.now()
	}
	return e.OccurredAt.UTC().Truncate(time.Microsecond)
}

// suspendGrants suspends, at at, every active grant studentID gave at or
// before at, one audit record each (the system job is the actor), in one
// transaction under the student's grant lock; then tells the student once
// per event (notify dedupes on the event). Idempotent: a retry suspends
// nothing new and still sends the notice if it was not sent.
func (s *service) suspendGrants(ctx context.Context, studentID uuid.UUID, at time.Time, job, reason string, eventID uuid.UUID) error {
	sys := authz.SystemActor(job)
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		if err := s.grants().LockGrants(ctx, studentID); err != nil {
			return err
		}
		suspended, err := s.grants().SuspendGrants(ctx, studentID, at)
		if err != nil {
			return err
		}
		for _, g := range suspended {
			ev := grantEvent(auditGrantSuspend, g, map[string]any{"reason": reason})
			if err := s.d.Audit.Log(ctx, ev.By(sys)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	n, err := s.grants().CountSuspendedAt(ctx, studentID, at)
	if err != nil || n == 0 {
		return err
	}
	return s.noticeSuspended(ctx, studentID, job, eventID)
}

// noticeSuspended sends the student's in-app notice: fixed text, no
// company named, a link to the grant list. A permanent failure (an account
// that cannot receive it) is logged and skipped; anything else is returned
// so the task is retried.
func (s *service) noticeSuspended(ctx context.Context, studentID uuid.UUID, job string, eventID uuid.UUID) error {
	if s.d.Sender == nil {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "suspended grants notice not sent: notify is not wired",
			slog.String("task", job), slog.String("user_id", studentID.String()))
		return nil
	}
	to := studentID
	_, err := s.d.Sender.Send(ctx, notify.Message{
		UserID: &to, Channel: notify.ChannelInApp, Category: notify.CategoryTransactional,
		Template: templateGrantsSuspended, LinkPath: notify.MyAccessGrantsLink(),
		DedupeKey: templateGrantsSuspended + ":" + eventID.String(),
	})
	if err != nil && notify.IsPermanent(err) {
		s.d.Log.LogAttrs(ctx, slog.LevelError, "suspended grants notice not deliverable", slog.String("task", job),
			slog.String("user_id", studentID.String()), slog.String("err", err.Error()))
		return nil
	}
	if err != nil {
		return fmt.Errorf("students: suspended grants notice: %w", err)
	}
	return nil
}
