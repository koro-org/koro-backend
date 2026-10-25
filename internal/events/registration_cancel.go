package events

import (
	"context"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/problem"
)

// ownResource is the authz view of the caller's own registration (03 §3.5
// registration: OrgID = the event's organiser, OwnerUserID = the
// registrant; Attrs status, event_staff_role).
func ownResource(r Registration) authz.Resource {
	org, owner := r.OrgID, r.UserID
	return authz.Resource{Type: authz.TypeRegistration, ID: r.ID, OrgID: &org, OwnerUserID: &owner, Attrs: map[string]string{
		authz.AttrStatus: string(r.Status), authz.AttrEventStaffRole: "none",
	}}
}

// CancelMyRegistration implements Service. Order: the caller's own
// registration (404 for anyone else's) → authz → in one transaction (the
// event row first, then the registration): a confirmed registration is
// cancelled until cancel_deadline_at and the event's start (409
// events.registration_closed), its ticket revoked, its seat released and
// the waiting list moved up; a pending or waitlisted one is withdrawn;
// any other status is 409 events.invalid_state.
func (s *service) CancelMyRegistration(ctx context.Context, a authz.Actor, id uuid.UUID, reason string) (RegistrationView, error) {
	if !a.IsUser() {
		return RegistrationView{}, authz.ErrForbidden
	}
	r, err := s.d.Store.GetUserRegistration(ctx, a.UserID, id)
	if err != nil {
		return RegistrationView{}, err
	}
	if err := authz.Can(ctx, a, authz.EventsRegistrationCancelOwn, ownResource(r)); err != nil {
		return RegistrationView{}, err
	}
	why, err := cleanOptional("body.reason", reason, maxRegCancel)
	if err != nil {
		return RegistrationView{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op     string
		ID     uuid.UUID
		Reason *string
	}{"cancel_own", r.ID, why})
	if err != nil {
		return RegistrationView{}, err
	}
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		e, err := s.d.Store.LockEvent(ctx, r.EventID)
		if err != nil {
			return uuid.Nil(), err
		}
		cur, err := s.d.Store.LockRegistration(ctx, e.OrgID, e.ID, r.ID)
		if err != nil {
			return uuid.Nil(), err
		}
		now := s.now()
		to := RegWithdrawn
		switch cur.Status {
		case RegConfirmed:
			if e.CancelDeadlineAt != nil && !now.Before(*e.CancelDeadlineAt) || !now.Before(e.StartsAt) {
				return uuid.Nil(), ErrRegistrationClosed
			}
			to = RegCancelled
		case RegPending, RegWaitlisted:
		default:
			return uuid.Nil(), ErrInvalidState
		}
		return cur.ID, s.endRegistration(ctx, a, e, cur, to, ByUser, why, RevokeRegistrationCancelled, now)
	})
	if err != nil {
		return RegistrationView{}, err
	}
	return s.ownView(ctx, a.UserID, r.ID, false)
}

// CancelEventRegistration implements Service: the organiser cancels a
// pending, waitlisted or confirmed registration (cancelled_by_kind org;
// the reason is required). Order: the event (locked; 404) → authz
// (404/403) → the org status (403) → the registration of this event (404)
// → its status (409) → the reason (422).
func (s *service) CancelEventRegistration(ctx context.Context, a authz.Actor, eventID, id uuid.UUID, reason string) (RegistrationView, error) {
	hash, err := idempotency.Hash(struct {
		Op     string
		Event  uuid.UUID
		ID     uuid.UUID
		Reason string
	}{"cancel_org", eventID, id, reason})
	if err != nil {
		return RegistrationView{}, err
	}
	var ev Event
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		e, _, err := s.locked(ctx, a, authz.EventsRegistrationCancel, eventID)
		if err != nil {
			return uuid.Nil(), err
		}
		ev = e
		r, err := s.d.Store.LockRegistration(ctx, e.OrgID, e.ID, id)
		if err != nil {
			return uuid.Nil(), err
		}
		switch r.Status {
		case RegConfirmed, RegPending, RegWaitlisted:
		default:
			return uuid.Nil(), ErrInvalidState
		}
		why, err := cleanText("body.cancel_reason", reason, minOrgCancel, maxRegCancel)
		if err != nil {
			return uuid.Nil(), err
		}
		return r.ID, s.endRegistration(ctx, a, e, r, RegCancelled, ByOrg, &why, RevokeRegistrationCancelled, s.now())
	})
	if err != nil {
		return RegistrationView{}, err
	}
	return s.managedRegistration(ctx, ev, eventID, id)
}

// DecideRegistration implements Service: a pending application of a
// manual event becomes confirmed (a seat by the conditional UPDATE, 409
// events.capacity_full when none is left; its ticket), waitlisted (the
// waiting list must be open) or rejected. The decider is approved_by; the
// note is for the organiser only (never in an event, a log or the
// student's view).
func (s *service) DecideRegistration(ctx context.Context, a authz.Actor, eventID, id uuid.UUID, d Decision) (RegistrationView, error) {
	switch d.Status {
	case RegConfirmed, RegWaitlisted, RegRejected:
	default:
		return RegistrationView{}, problem.Field("body.decision", "must be confirmed, waitlisted or rejected")
	}
	note, err := cleanOptional("body.decision_note", d.Note, maxDecisionNote)
	if err != nil {
		return RegistrationView{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op    string
		Event uuid.UUID
		ID    uuid.UUID
		D     Decision
	}{"decide", eventID, id, d})
	if err != nil {
		return RegistrationView{}, err
	}
	var ev Event
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		e, _, err := s.locked(ctx, a, authz.EventsRegistrationDecide, eventID)
		if err != nil {
			return uuid.Nil(), err
		}
		ev = e
		r, err := s.d.Store.LockRegistration(ctx, e.OrgID, e.ID, id)
		if err != nil {
			return uuid.Nil(), err
		}
		if e.RegistrationMode != ModeManual || r.Status != RegPending || e.Status != StatusPublished {
			return uuid.Nil(), ErrInvalidState
		}
		now := s.now()
		r.Status, r.ApprovedBy, r.ApprovedAt, r.DecisionNote = d.Status, &a.UserID, &now, note
		switch d.Status {
		case RegConfirmed:
			ok, err := s.d.Store.ClaimSeat(ctx, e.OrgID, e.ID)
			if err != nil {
				return uuid.Nil(), err
			}
			if !ok {
				return uuid.Nil(), ErrCapacityFull
			}
			r.ConfirmedAt = &now
		case RegWaitlisted:
			if !e.WaitlistEnabled {
				return uuid.Nil(), ErrInvalidState
			}
			r.WaitlistedAt = &now
		}
		stored, err := s.d.Store.UpdateRegistration(ctx, r)
		if err != nil {
			return uuid.Nil(), err
		}
		ticketVersion := 0
		if stored.Status == RegConfirmed {
			t, err := s.issueTicket(ctx, stored, now)
			if err != nil {
				return uuid.Nil(), err
			}
			ticketVersion = t.Version
		}
		sens, err := s.sensitive(ctx, e)
		if err != nil {
			return uuid.Nil(), err
		}
		if err := s.d.Audit.Log(ctx, auditEvent(a, authz.EventsRegistrationDecide, regAggType, stored.ID, e.OrgID, "",
			map[string]any{"event_id": e.ID.String(), "decision": string(stored.Status)})); err != nil {
			return uuid.Nil(), err
		}
		return stored.ID, s.emitAdmitted(ctx, stored, sens, false, ticketVersion)
	})
	if err != nil {
		return RegistrationView{}, err
	}
	return s.managedRegistration(ctx, ev, eventID, id)
}

// endRegistration moves a registration to cancelled or withdrawn in the
// caller's transaction (the event row is already locked): its ticket is
// revoked, a held seat released and given to the waiting list (FIFO),
// registration.cancelled emitted (not for a withdrawal) and the change
// audited.
func (s *service) endRegistration(ctx context.Context, by authz.Actor, e Event, r Registration, to RegStatus, kind CancelledBy,
	reason *string, revoke RevokeReason, now time.Time) error {
	held := r.Status.Counted()
	r.Status, r.CancelledAt, r.CancelledByKind, r.CancelReason = to, &now, &kind, reason
	if _, err := s.d.Store.UpdateRegistration(ctx, r); err != nil {
		return err
	}
	if _, err := s.d.Store.RevokeTickets(ctx, e.OrgID, r.ID, revoke, now); err != nil {
		return err
	}
	if held {
		if err := s.d.Store.ReleaseSeat(ctx, e.OrgID, e.ID); err != nil {
			return err
		}
	}
	sens, err := s.sensitive(ctx, e)
	if err != nil {
		return err
	}
	if to == RegCancelled {
		if err := s.emitReg(ctx, domainevent.RegistrationCancelled{RegistrationID: r.ID, EventID: r.EventID, UserID: r.UserID,
			By: string(kind), Sensitive: sens}, r.ID); err != nil {
			return err
		}
	}
	act := authz.EventsRegistrationCancel
	if kind == ByUser {
		act = authz.EventsRegistrationCancelOwn
	}
	auditReason := ""
	if reason != nil && kind != ByUser {
		auditReason = *reason
	}
	meta := map[string]any{"event_id": e.ID.String(), "status": string(to), "by": string(kind), "seat_released": held}
	if err := s.d.Audit.Log(ctx, auditEvent(by, act, regAggType, r.ID, e.OrgID, auditReason, meta)); err != nil {
		return err
	}
	if held {
		return s.promote(ctx, e, sens, now)
	}
	return nil
}

// promote fills the free seats of e from its waiting list in the caller's
// transaction (01 §6.3 "İptal + terfi"): the first waitlisted registration
// (FIFO by waitlisted_at, locked FOR UPDATE SKIP LOCKED) takes a seat by
// the conditional UPDATE, becomes confirmed with a ticket and
// registration.confirmed (Promoted). Idempotent: without a free seat or a
// waiting registration nothing happens, and a promoted registration is no
// longer waitlisted. Nothing moves once the event has started (the
// waiting list expires then).
func (s *service) promote(ctx context.Context, e Event, sens bool, now time.Time) error {
	if !now.Before(e.StartsAt) || e.Status != StatusPublished {
		return nil
	}
	sys := authz.SystemActor(promoteJob)
	if err := authz.Can(ctx, sys, authz.EventsRegistrationPromoteWaitlist, authz.Resource{}); err != nil {
		return err
	}
	for range maxPromotions {
		next, ok, err := s.d.Store.NextWaitlisted(ctx, e.OrgID, e.ID)
		if err != nil || !ok {
			return err
		}
		claimed, err := s.d.Store.ClaimSeat(ctx, e.OrgID, e.ID)
		if err != nil || !claimed {
			return err
		}
		next.Status, next.ConfirmedAt = RegConfirmed, &now
		stored, err := s.d.Store.UpdateRegistration(ctx, next)
		if err != nil {
			return err
		}
		t, err := s.issueTicket(ctx, stored, now)
		if err != nil {
			return err
		}
		if err := s.d.Audit.Log(ctx, auditEvent(sys, authz.EventsRegistrationPromoteWaitlist, regAggType, stored.ID, e.OrgID, "",
			map[string]any{"event_id": e.ID.String(), "ticket_version": t.Version})); err != nil {
			return err
		}
		if err := s.emitAdmitted(ctx, stored, sens, true, t.Version); err != nil {
			return err
		}
	}
	return nil
}

// managedRegistration reads a registration after a write by one of the
// event's managers (the organiser view without the sensitive answers).
func (s *service) managedRegistration(ctx context.Context, e Event, eventID, id uuid.UUID) (RegistrationView, error) {
	if e.ID == uuid.Nil() { // a replayed request: the event was not read in this call
		var err error
		if e, err = s.d.Store.GetEvent(ctx, eventID); err != nil {
			return RegistrationView{}, err
		}
	}
	r, err := s.d.Store.GetRegistration(ctx, e.OrgID, e.ID, id)
	if err != nil {
		return RegistrationView{}, err
	}
	vs, err := s.orgViews(ctx, e, []Registration{r}, true)
	if err != nil {
		return RegistrationView{}, err
	}
	return vs[0], nil
}
