package events

import (
	"context"
	"errors"
	"log/slog"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/notify"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
)

// Worker names of BE-028 (01 §6.2, §7; 01 §8: <module>.<verb>_<object>).
const (
	// CancelRegistrationsTaskName consumes event.cancelled: every live
	// registration of the event is cancelled (system), its ticket revoked
	// (event_cancelled), in batches of 1000.
	CancelRegistrationsTaskName = "events.cancel_registrations"
	// The in-app notices of the registration events (the ticket e-mail of
	// registration.confirmed and the .ics files: ticket_email.go).
	NotifySubmittedTaskName  = "events.notify_registration_submitted"
	NotifyWaitlistedTaskName = "events.notify_registration_waitlisted"
	NotifyRejectedTaskName   = "events.notify_registration_rejected"
	NotifyCancelledTaskName  = "events.notify_registration_cancelled"
	// WaitlistExpireJobName: waitlisted registrations of started events
	// become expired (every 15 minutes, default).
	WaitlistExpireJobName = "events.waitlist_expire"
	// ReconcileCountsJobName: confirmed_count is reconciled with the real
	// count (03:00, low); a drift is logged and fixed, freed seats go to
	// the waiting list.
	ReconcileCountsJobName = "events.reconcile_counts"

	jobBatch = 1000 // rows per transaction (01 §7)
	// reconcileWindow: events that ended earlier are not reconciled (their
	// count no longer admits anyone).
	reconcileWindow = 30 * 24 * time.Hour
)

// Templates of the in-app notices (internal/notify/templates/events):
// fixed texts, the data holds opaque IDs only.
const (
	templateSubmitted  = "events.registration_submitted"
	templateReceived   = "events.registration_received"
	templateWaitlisted = "events.registration_waitlisted"
	templateRejected   = "events.registration_rejected"
	templateCancelled  = "events.registration_cancelled"
)

// managerRoles receive the notice of a new application (the S cells of
// events.registration.decide).
var managerRoles = []authz.MemberRole{authz.MemberClubAdmin, authz.MemberUniversityRep, authz.MemberCareerStaff}

// Subscribe registers the outbox subscriptions of events (01 §6.2).
func (m Module) Subscribe(r *outbox.Registry) {
	r.Subscribe(domainevent.TopicEventCancelled, CancelRegistrationsTaskName, m.svc.handleEventCancelled,
		outbox.Queue(queue.Default))
	r.Subscribe(domainevent.TopicRegistrationSubmitted, NotifySubmittedTaskName, m.svc.handleSubmitted,
		outbox.Queue(queue.Default))
	r.Subscribe(domainevent.TopicRegistrationWaitlisted, NotifyWaitlistedTaskName, m.svc.handleWaitlisted,
		outbox.Queue(queue.Default))
	r.Subscribe(domainevent.TopicRegistrationRejected, NotifyRejectedTaskName, m.svc.handleRejected,
		outbox.Queue(queue.Default))
	r.Subscribe(domainevent.TopicRegistrationCancelled, NotifyCancelledTaskName, m.svc.handleCancelled,
		outbox.Queue(queue.Default))
	// BE-031: the ticket mails (01 §8: critical), the schedule update and
	// the cancellation .ics, the no-shows of a closed attendance.
	r.Subscribe(domainevent.TopicRegistrationConfirmed, SendTicketEmailTaskName, m.svc.handleConfirmed,
		outbox.Queue(queue.Critical))
	r.Subscribe(domainevent.TopicTicketReissued, SendReissuedTicketTaskName, m.svc.handleReissued,
		outbox.Queue(queue.Critical))
	r.Subscribe(domainevent.TopicEventScheduleChanged, SendScheduleUpdateTaskName, m.svc.handleScheduleChanged,
		outbox.Queue(queue.Default), outbox.Timeout(scheduleTimeout))
	r.Subscribe(domainevent.TopicRegistrationCancelled, SendCancellationEmailTaskName, m.svc.handleCancelledEmail,
		outbox.Queue(queue.Default))
	r.Subscribe(domainevent.TopicAttendanceFinalized, MarkNoShowsTaskName, m.svc.handleAttendanceFinalized,
		outbox.Queue(queue.Default))
	// T-176: a cover the scan rejected leaves its event (rejected.go).
	files.SubscribeRejected(r, coverOwner, DropRejectedCoverTaskName, m.svc.dropRejectedCover)
}

// Tasks returns the task handlers events enqueues itself (mechanism A):
// the ticket resend (BE-031) and the export build (BE-050).
func (m Module) Tasks() []queue.Task {
	return []queue.Task{{Name: ResendTicketTaskName, Handle: m.svc.handleResend},
		{Name: BuildExportTaskName, Handle: m.svc.handleBuildExport}}
}

// Jobs returns the scheduled jobs of 01 §7 owned by BE-028 and BE-031 (and
// events.complete_ended, T-264; events.purge_exports, BE-050).
func (m Module) Jobs() []queue.Job {
	return []queue.Job{
		{Name: WaitlistExpireJobName, Cron: "*/15 * * * *", Queue: queue.Default, Unique: 14 * time.Minute,
			Handle: m.svc.expireWaitlists},
		{Name: ReconcileCountsJobName, Cron: "0 3 * * *", Queue: queue.Low, Unique: 23 * time.Hour,
			Handle: m.svc.reconcileCounts},
		{Name: AutoFinalizeJobName, Cron: "20 * * * *", Queue: queue.Default, Unique: 50 * time.Minute,
			Handle: m.svc.autoFinalize},
		{Name: CompleteEndedJobName, Cron: "40 * * * *", Queue: queue.Default, Unique: 50 * time.Minute,
			Handle: m.svc.completeEnded}, // T-264 (BE-033)
		m.svc.purgeExportsJob(), // BE-050
	}
}

// handleEventCancelled cancels every live registration of a cancelled
// event (pending, waitlisted, confirmed → cancelled by the system, tickets
// revoked as event_cancelled, seats released, registration.cancelled for
// each: the registrants are notified). Batches of 1000, each in its own
// transaction under the event row lock; idempotent (a second run finds
// nothing live).
func (s *service) handleEventCancelled(ctx context.Context, env domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.EventCancelled](env)
	if err != nil {
		return queue.Permanent(err)
	}
	sys := authz.SystemActor(cancelEventJob)
	total, err := db.Batches(ctx, s.d.Tx, jobBatch, func(ctx context.Context) (int, error) {
		e, err := s.d.Store.LockEvent(ctx, p.EventID)
		switch {
		case errors.Is(err, ErrNotFound):
			return 0, nil
		case err != nil:
			return 0, err
		case e.OrgID != p.OrgID || e.Status != StatusCancelled:
			return 0, nil // not this org's event, or not cancelled: nothing to do
		}
		rows, err := s.d.Store.LockActiveRegistrations(ctx, e.OrgID, e.ID, jobBatch)
		if err != nil {
			return 0, err
		}
		now := s.now()
		for _, r := range rows {
			if err := s.endRegistration(ctx, sys, e, r, RegCancelled, BySystem, nil, RevokeEventCancelled, now); err != nil {
				return 0, err
			}
		}
		return len(rows), nil
	})
	if err != nil {
		return err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "registrations of a cancelled event cancelled",
		slog.String("event_id", p.EventID.String()), slog.Int("count", total))
	return nil
}

// handleSubmitted: the applicant gets "application received", the
// organiser's managers "new application" (01 §6.2).
func (s *service) handleSubmitted(ctx context.Context, env domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.RegistrationSubmitted](env)
	if err != nil {
		return queue.Permanent(err)
	}
	org := p.OrgID
	data := map[string]any{"registration_id": p.RegistrationID, "event_id": p.EventID}
	if err := s.notice(ctx, p.UserID, templateSubmitted, notify.MyRegistrationLink(p.RegistrationID), &org, data,
		"events.registration_submitted:"+p.RegistrationID.String()); err != nil {
		return err
	}
	members, err := s.d.Orgs.ActiveMemberIDs(ctx, p.OrgID)
	if err != nil {
		return err
	}
	link := notify.EventRegistrationLink(p.OrgID, p.EventID, p.RegistrationID)
	for _, m := range members {
		ok, err := s.d.Orgs.HasRole(ctx, p.OrgID, m, managerRoles...)
		if err != nil {
			return err
		}
		if !ok || m == p.UserID {
			continue
		}
		if err := s.notice(ctx, m, templateReceived, link, &org, data,
			"events.registration_received:"+p.RegistrationID.String()+":"+m.String()); err != nil {
			return err
		}
	}
	return nil
}

// handleWaitlisted: the registrant is told they are on the waiting list.
func (s *service) handleWaitlisted(ctx context.Context, env domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.RegistrationWaitlisted](env)
	if err != nil {
		return queue.Permanent(err)
	}
	return s.notice(ctx, p.UserID, templateWaitlisted, notify.MyRegistrationLink(p.RegistrationID), nil,
		map[string]any{"registration_id": p.RegistrationID, "event_id": p.EventID},
		"events.registration_waitlisted:"+p.RegistrationID.String())
}

// handleRejected: the applicant is told (never the decision note).
func (s *service) handleRejected(ctx context.Context, env domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.RegistrationRejected](env)
	if err != nil {
		return queue.Permanent(err)
	}
	return s.notice(ctx, p.UserID, templateRejected, notify.MyRegistrationLink(p.RegistrationID), nil,
		map[string]any{"registration_id": p.RegistrationID, "event_id": p.EventID},
		"events.registration_rejected:"+p.RegistrationID.String())
}

// handleCancelled: a registration the organiser or the system cancelled
// is notified to its registrant (their own cancellation is not).
func (s *service) handleCancelled(ctx context.Context, env domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.RegistrationCancelled](env)
	if err != nil {
		return queue.Permanent(err)
	}
	if p.By == string(ByUser) {
		return nil
	}
	return s.notice(ctx, p.UserID, templateCancelled, notify.MyRegistrationLink(p.RegistrationID), nil,
		map[string]any{"registration_id": p.RegistrationID, "event_id": p.EventID},
		"events.registration_cancelled:"+p.RegistrationID.String())
}

// notice sends one in-app notice (dedupe key: one per registration and
// recipient). A permanent failure (the account is gone) is logged and
// dropped; anything else is retried.
func (s *service) notice(ctx context.Context, to uuid.UUID, tmpl string, link notify.Link, org *uuid.UUID, data map[string]any, dedupe string) error {
	if s.d.Sender == nil {
		return errors.New("events: notify is not wired")
	}
	_, err := s.d.Sender.Send(ctx, notify.Message{
		UserID: &to, Channel: notify.ChannelInApp, Category: notify.CategoryTransactional, Template: tmpl,
		SourceOrgID: org, Data: data, LinkPath: link, DedupeKey: dedupe,
	})
	if err != nil && notify.IsPermanent(err) {
		s.d.Log.LogAttrs(ctx, slog.LevelError, "registration notice not deliverable", slog.String("template", tmpl),
			slog.String("err", err.Error()))
		return nil
	}
	return err
}

// expireWaitlists is events.waitlist_expire: waitlisted registrations of
// events that have started become expired (R-003 §2.5.3), batches of 1000,
// each audited by the system actor.
func (s *service) expireWaitlists(ctx context.Context) error {
	sys := authz.SystemActor(expireJob)
	if err := authz.Can(ctx, sys, authz.EventsRegistrationExpire, authz.Resource{}); err != nil {
		return err
	}
	total, err := db.Batches(ctx, s.d.Tx, jobBatch, func(ctx context.Context) (int, error) {
		rows, err := s.d.Store.ExpireWaitlisted(ctx, s.now(), jobBatch)
		if err != nil {
			return 0, err
		}
		for _, r := range rows {
			if err := s.d.Audit.Log(ctx, auditEvent(sys, authz.EventsRegistrationExpire, regAggType, r.ID, r.OrgID, "",
				map[string]any{"event_id": r.EventID.String()})); err != nil {
				return 0, err
			}
		}
		return len(rows), nil
	})
	if err != nil {
		return err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "waiting lists expired", slog.Int("count", total))
	return nil
}

// reconcileCounts is events.reconcile_counts: the confirmed_count of every
// non-draft event that ended at most 30 days ago is compared with its
// confirmed and checked-in registrations; a drift is fixed under the event
// row lock (the count is read after the lock, so no registration in
// flight is missed), logged as a warning, and freed seats go to the
// waiting list. A count above the capacity cannot be stored (CHECK): it
// is logged as an error and left for a person.
func (s *service) reconcileCounts(ctx context.Context) error {
	since, after, fixed := s.now().Add(-reconcileWindow), uuid.Nil(), 0
	for {
		counts, err := s.d.Store.SeatCounts(ctx, since, after, jobBatch)
		if err != nil {
			return err
		}
		for _, c := range counts {
			if c.Stored == c.Actual {
				continue
			}
			changed, err := s.fixCount(ctx, c)
			if err != nil {
				s.d.Log.LogAttrs(ctx, slog.LevelError, "confirmed_count not reconciled", slog.String("event_id", c.EventID.String()),
					slog.Int("stored", c.Stored), slog.Int("actual", c.Actual), slog.String("err", err.Error()), slog.Bool("alarm", true))
				continue
			}
			if changed {
				fixed++
			}
		}
		if len(counts) < jobBatch {
			break
		}
		after = counts[len(counts)-1].EventID
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "confirmed counts reconciled", slog.Int("fixed", fixed))
	return nil
}

// fixCount reconciles one event in its own transaction.
func (s *service) fixCount(ctx context.Context, c SeatCount) (bool, error) {
	changed := false
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		e, err := s.d.Store.LockEvent(ctx, c.EventID)
		if err != nil {
			return err
		}
		n, err := s.d.Store.CountSeats(ctx, e.OrgID, e.ID)
		if err != nil || n == e.ConfirmedCount {
			return err
		}
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "confirmed_count drift", slog.String("event_id", e.ID.String()),
			slog.Int("stored", e.ConfirmedCount), slog.Int("actual", n))
		if err := s.d.Store.SetConfirmedCount(ctx, e.OrgID, e.ID, n); err != nil {
			return err
		}
		changed = true
		if n >= e.ConfirmedCount {
			return nil
		}
		sens, err := s.sensitive(ctx, e)
		if err != nil {
			return err
		}
		return s.promote(ctx, e, sens, s.now())
	})
	return changed, err
}
