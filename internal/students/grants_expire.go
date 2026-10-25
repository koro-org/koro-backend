package students

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/notify"
)

// The expiry of suspended level 3 grants (BE-088; user decision Q-03,
// T-331). A grant suspended longer than the suspension window is revoked
// by the system (revoked_by_kind 'system', revoke_reason
// ReasonSuspensionExpired), one audit record each; the access_grant_revoked
// retention then deletes it 6 months later like any revoked grant. The
// student is told in-app, once per expiry run, without the company's name;
// the company is not told (it never learned of the suspension).
//
// The notices are sent after the revocations committed, from the grants
// themselves: every run (and every retry of a failed run) re-sends the
// notices of the expiries of the last expiryNoticeLookback, and notify
// dedupes on (student, revoked_at). A notice that failed on a transient
// error is therefore sent by the next attempt instead of being lost.
const (
	// ExpireSuspendedGrantsJobName revokes the suspended grants whose
	// window ran out and tells their students (01 §7).
	ExpireSuspendedGrantsJobName = "students.expire_suspended_grants" // 02:30, default

	// templateGrantsExpired is the student's in-app notice
	// (internal/notify/templates/students).
	templateGrantsExpired = "students.access_grants_expired"
	// expiryNoticeLookback is how far back a run re-sends expiry notices.
	expiryNoticeLookback = 72 * time.Hour
)

// expireSuspendedGrants is students.expire_suspended_grants: every open
// grant suspended before now − window is revoked (one transaction per
// student, under the student's grant lock), then the expiry notices are
// sent. A notice that cannot be sent fails the run after every other
// student was handled (the retry sends it).
func (s *service) expireSuspendedGrants(ctx context.Context) error {
	now := s.now()
	cutoff := now.Add(-s.suspensionWindow())
	after, revoked := uuid.Nil(), 0
	for ctx.Err() == nil {
		ids, err := s.d.Store.SuspendedStudentsBefore(ctx, cutoff, after, batchSize)
		if err != nil {
			return fmt.Errorf("%s: %w", ExpireSuspendedGrantsJobName, err)
		}
		if len(ids) == 0 {
			break
		}
		after = ids[len(ids)-1]
		for _, id := range ids {
			n, err := s.expireStudent(ctx, id, cutoff, now)
			if err != nil {
				return fmt.Errorf("%s: %w", ExpireSuspendedGrantsJobName, err)
			}
			revoked += n
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if revoked > 0 {
		s.d.Log.LogAttrs(ctx, slog.LevelInfo, "suspended access grants expired", slog.Int("revoked", revoked))
	}
	if err := s.noticeExpiries(ctx, now.Add(-expiryNoticeLookback)); err != nil {
		return fmt.Errorf("%s: %w", ExpireSuspendedGrantsJobName, err)
	}
	return nil
}

// expireStudent revokes studentID's grants suspended before cutoff, at at,
// with one audit record each (the system job is the actor).
func (s *service) expireStudent(ctx context.Context, studentID uuid.UUID, cutoff, at time.Time) (int, error) {
	sys := authz.SystemActor(ExpireSuspendedGrantsJobName)
	n := 0
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		if err := s.d.Store.LockGrants(ctx, studentID); err != nil {
			return err
		}
		gone, err := s.d.Store.ExpireSuspendedGrants(ctx, studentID, cutoff, at)
		if err != nil {
			return err
		}
		for _, g := range gone {
			ev := grantEvent(string(authz.StudentsAccessGrantRevoke), g, map[string]any{
				"revoked_by": string(RevokedBySystem), "reason": ReasonSuspensionExpired,
			})
			if err := s.d.Audit.Log(ctx, ev.By(sys)); err != nil {
				return err
			}
		}
		n = len(gone)
		return nil
	})
	return n, err
}

// noticeExpiries sends the notice of every expiry recorded since since
// (notify dedupes the ones already sent). Transient failures are joined
// and returned after the walk.
func (s *service) noticeExpiries(ctx context.Context, since time.Time) error {
	var errs []error
	var after *ExpiryKey
	for ctx.Err() == nil {
		rows, err := s.d.Store.ExpiredRevocations(ctx, since, after, batchSize)
		if err != nil {
			return err
		}
		for _, k := range rows {
			if err := s.noticeExpired(ctx, k); err != nil {
				errs = append(errs, err)
			}
		}
		if len(rows) < batchSize {
			break
		}
		after = &rows[len(rows)-1]
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.Join(errs...)
}

// noticeExpired sends one expiry's in-app notice: fixed text, no company
// named, a link to the grant list. A permanent failure (an account that
// cannot receive it) is logged and skipped.
func (s *service) noticeExpired(ctx context.Context, k ExpiryKey) error {
	if s.d.Sender == nil {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "expired grants notice not sent: notify is not wired",
			slog.String("job", ExpireSuspendedGrantsJobName), slog.String("user_id", k.StudentUserID.String()))
		return nil
	}
	to := k.StudentUserID
	_, err := s.d.Sender.Send(ctx, notify.Message{
		UserID: &to, Channel: notify.ChannelInApp, Category: notify.CategoryTransactional,
		Template: templateGrantsExpired, LinkPath: notify.MyAccessGrantsLink(),
		DedupeKey: templateGrantsExpired + ":" + k.StudentUserID.String() + ":" + strconv.FormatInt(k.RevokedAt.UnixMicro(), 10),
	})
	if err != nil && notify.IsPermanent(err) {
		s.d.Log.LogAttrs(ctx, slog.LevelError, "expired grants notice not deliverable",
			slog.String("job", ExpireSuspendedGrantsJobName), slog.String("user_id", k.StudentUserID.String()),
			slog.String("err", err.Error()))
		return nil
	}
	if err != nil {
		return fmt.Errorf("students: expired grants notice: %w", err)
	}
	return nil
}
