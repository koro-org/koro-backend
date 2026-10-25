package events

import (
	"context"
	"errors"
	"log/slog"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/queue"
)

// Attendance (BE-031; 01 §6.2 attendance.finalized, registration.no_show,
// §7 events.auto_finalize_attendance, 02 §11.1, §11.6, 04 §2.6).
//
// Closing the attendance of an event with check-in (after its end) freezes
// it: no check-in and no void any more (409). The confirmed registrations
// that never checked in become no_show, in batches by the
// attendance.finalized consumer (each with registration.no_show for
// talent); a no_show holds no seat, so confirmed_count drops with them in
// the same transaction (no drift for events.reconcile_counts). The
// organiser's managers or a manager of the staff close it
// (finalizeEventAttendance, once: a second call is 409); otherwise the
// system does 24 hours after the end when the event had at least one
// check-in (01 §7). The event's status does not change.

const (
	// MarkNoShowsTaskName consumes attendance.finalized.
	MarkNoShowsTaskName = "events.mark_no_shows"
	// AutoFinalizeJobName closes the attendance of events that ended 24
	// hours ago (every hour at :20, default).
	AutoFinalizeJobName = "events.auto_finalize_attendance"

	// autoFinalizeAfter is how long after its end the system closes an
	// attendance (01 §7; 04 §E.6 said 48 hours, see the BE-031 notes).
	autoFinalizeAfter = 24 * time.Hour
)

// Who closed an attendance (attendance.finalized FinalizedBy).
const (
	finalizedByUser   = "user"
	finalizedBySystem = "system"
)

// FinalizeEventAttendance implements Service. Order, in one transaction
// under the event row lock: the event (404) → authz (404/403: stf Em, the
// organiser's managers, k-po) → the organiser's status (403) → check-in
// enabled, published or completed, ended, not finalised (409) → the
// attendance is closed, audited and attendance.finalized written (O). It
// returns how many confirmed registrations become no_show.
func (s *service) FinalizeEventAttendance(ctx context.Context, a authz.Actor, eventID uuid.UUID) (int, error) {
	hash, err := idempotency.Hash(struct {
		Op    string
		Event uuid.UUID
	}{"finalize_attendance", eventID})
	if err != nil {
		return 0, err
	}
	n, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (int, error) {
		e, _, err := s.locked(ctx, a, authz.EventsAttendanceFinalize, eventID)
		if err != nil {
			return 0, err
		}
		by := a.UserID
		return s.finalize(ctx, a, e, &by, finalizedByUser, s.now())
	})
	return n, err
}

// finalize closes the attendance of the locked event e in the caller's
// transaction (by nil: the system).
func (s *service) finalize(ctx context.Context, a authz.Actor, e Event, by *uuid.UUID, kind string, now time.Time) (int, error) {
	switch {
	case !e.CheckinEnabled, e.AttendanceFinalizedAt != nil, now.Before(e.EndsAt),
		e.Status != StatusPublished && e.Status != StatusCompleted:
		return 0, ErrInvalidState
	}
	ok, err := s.d.Store.FinalizeAttendance(ctx, e.OrgID, e.ID, by, now)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, ErrInvalidState
	}
	n, err := s.d.Store.CheckinCounts(ctx, e.OrgID, e.ID)
	if err != nil {
		return 0, err
	}
	noShows := n.Confirmed - n.CheckedIn
	if err := s.d.Audit.Log(ctx, auditEvent(a, authz.EventsAttendanceFinalize, eventAggType, e.ID, e.OrgID, "",
		map[string]any{"by": kind, "no_show_count": noShows, "checked_in": n.CheckedIn})); err != nil {
		return 0, err
	}
	if err := s.emit(ctx, domainevent.AttendanceFinalized{EventID: e.ID, FinalizedBy: kind}, e.ID); err != nil {
		return 0, err
	}
	return noShows, nil
}

// handleAttendanceFinalized is events.mark_no_shows: the confirmed
// registrations of a finalised event become no_show in batches, each
// batch in its own transaction under the event row lock (the seats leave
// confirmed_count with them), with registration.no_show and a system
// audit record each. Idempotent: a second delivery finds none left.
func (s *service) handleAttendanceFinalized(ctx context.Context, env domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.AttendanceFinalized](env)
	if err != nil {
		return queue.Permanent(err)
	}
	sys := authz.SystemActor(MarkNoShowsTaskName)
	if err := authz.Can(ctx, sys, authz.EventsAttendanceFinalize, authz.Resource{}); err != nil {
		return err
	}
	total, err := db.Batches(ctx, s.d.Tx, s.batch, func(ctx context.Context) (int, error) {
		e, err := s.d.Store.LockEvent(ctx, p.EventID)
		switch {
		case errors.Is(err, ErrNotFound):
			return 0, nil
		case err != nil:
			return 0, err
		case e.AttendanceFinalizedAt == nil:
			return 0, nil // not finalised: nothing to mark
		}
		rows, err := s.d.Store.MarkNoShows(ctx, e.OrgID, e.ID, s.batch)
		if err != nil {
			return 0, err
		}
		sens, err := s.sensitive(ctx, e)
		if err != nil {
			return 0, err
		}
		for _, r := range rows {
			if err := s.d.Audit.Log(ctx, auditEvent(sys, authz.EventsAttendanceFinalize, regAggType, r.ID, e.OrgID, "",
				map[string]any{"event_id": e.ID.String(), "status": string(RegNoShow)})); err != nil {
				return 0, err
			}
			if err := s.emitReg(ctx, domainevent.RegistrationNoShow{RegistrationID: r.ID, EventID: e.ID, UserID: r.UserID,
				Sensitive: sens}, r.ID); err != nil {
				return 0, err
			}
		}
		return len(rows), nil
	})
	if err != nil {
		return err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "no-shows marked", slog.String("event_id", p.EventID.String()),
		slog.Int("count", total))
	return nil
}

// autoFinalize is events.auto_finalize_attendance: the attendance of every
// event with check-in that ended 24 hours ago, had at least one check-in
// and is still open is closed by the system, each event in its own
// transaction under its row lock (re-checked there).
func (s *service) autoFinalize(ctx context.Context) error {
	sys := authz.SystemActor(AutoFinalizeJobName)
	if err := authz.Can(ctx, sys, authz.EventsAttendanceFinalize, authz.Resource{}); err != nil {
		return err
	}
	cutoff, after, closed := s.now().Add(-autoFinalizeAfter), uuid.Nil(), 0
	for {
		due, err := s.d.Store.DueAttendance(ctx, cutoff, after, s.batch)
		if err != nil {
			return err
		}
		for _, d := range due {
			err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
				e, err := s.d.Store.LockEvent(ctx, d.ID)
				if err != nil {
					return err
				}
				if e.AttendanceFinalizedAt != nil || e.EndsAt.After(cutoff) {
					return nil // closed or moved meanwhile
				}
				_, err = s.finalize(ctx, sys, e, nil, finalizedBySystem, s.now())
				if errors.Is(err, ErrInvalidState) {
					return nil
				}
				return err
			})
			if err != nil {
				return err
			}
			closed++
		}
		if len(due) < s.batch {
			break
		}
		after = due[len(due)-1].ID
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "attendances closed", slog.Int("count", closed))
	return nil
}
