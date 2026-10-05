package events

import (
	"context"
	"errors"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/problem"
	"koro/internal/platform/queue"
)

// Tickets of the registrant and the organiser (BE-031; 04 §2.6, §E.3,
// 02 §11.7, R-003 §2.5.4).
//
//   - getMyTicket: the QR payload is re-derived from the registration and
//     the ticket version (the database holds only its SHA-256), so the app
//     shows the same QR as the e-mail and can keep it for offline use.
//     Only a seat holder (confirmed, checked_in) has one; else 409.
//   - resendMyTicket: the same token again by e-mail (ticket_resend: 3 per
//     hour), through a queued task (mechanism A; 503 when the queue is
//     down, so the client knows to retry).
//   - reissue (the registrant, or the organiser's managers for a
//     registration of their event): version + 1 in one transaction: the
//     old ticket is revoked (reissued), its QR stops working at the door,
//     the new one is mailed (ticket.reissued, O).

// TicketView is the registrant's ticket (TicketDTO, 04 §E.3).
type TicketView struct {
	Ticket Ticket
	// Payload is the QR content "KORO1:<token>": a bearer credential, only
	// for the owner, never logged.
	Payload string
	Event   EventView
}

// ticketResource is the authz view of the caller's own ticket (03 §3.5
// ticket: OrgID = the event's organiser, OwnerUserID = the registrant).
func ticketResource(r Registration) authz.Resource {
	org, owner := r.OrgID, r.UserID
	return authz.Resource{Type: authz.TypeTicket, ID: r.ID, OrgID: &org, OwnerUserID: &owner, Attrs: map[string]string{}}
}

// ownTicketRegistration reads the caller's own registration (404 for
// anyone else's) and authorises act on its ticket.
func (s *service) ownTicketRegistration(ctx context.Context, a authz.Actor, act authz.Action, id uuid.UUID) (Registration, error) {
	if !a.IsUser() {
		return Registration{}, authz.ErrForbidden
	}
	r, err := s.d.Store.GetUserRegistration(ctx, a.UserID, id)
	if err != nil {
		return Registration{}, err
	}
	if err := authz.Can(ctx, a, act, ticketResource(r)); err != nil {
		return Registration{}, err
	}
	return r, nil
}

// GetMyTicket implements Service.
func (s *service) GetMyTicket(ctx context.Context, a authz.Actor, id uuid.UUID) (TicketView, error) {
	r, err := s.ownTicketRegistration(ctx, a, authz.EventsTicketReadOwn, id)
	if err != nil {
		return TicketView{}, err
	}
	return s.ticketView(ctx, r)
}

// ticketView is the ticket of a seat holder: 409 events.invalid_state for
// a registration without a seat or an active ticket, or a ticket of a
// retired key (reissue it).
func (s *service) ticketView(ctx context.Context, r Registration) (TicketView, error) {
	t, token, err := s.liveTicket(ctx, r)
	if err != nil {
		return TicketView{}, err
	}
	e, err := s.d.Store.GetEvent(ctx, r.EventID)
	if err != nil {
		return TicketView{}, err
	}
	o, err := s.org(ctx, e.OrgID)
	if err != nil {
		return TicketView{}, err
	}
	return TicketView{Ticket: t, Payload: TicketPayload(token), Event: s.view(ctx, e, o, false)}, nil
}

// liveTicket is the active ticket of a seat holder with its token.
func (s *service) liveTicket(ctx context.Context, r Registration) (Ticket, string, error) {
	if !r.Status.Counted() {
		return Ticket{}, "", ErrInvalidState
	}
	t, ok, err := s.d.Store.ActiveTicket(ctx, r.OrgID, r.ID)
	switch {
	case err != nil:
		return Ticket{}, "", err
	case !ok:
		return Ticket{}, "", ErrInvalidState
	}
	token, err := s.ticketToken(t)
	switch {
	case errors.Is(err, errTicketKey):
		return Ticket{}, "", ErrInvalidState
	case err != nil:
		return Ticket{}, "", err
	}
	return t, token, nil
}

// ResendMyTicket implements Service: the ticket mail again (the same
// token), audited; the mail goes out from the worker.
func (s *service) ResendMyTicket(ctx context.Context, a authz.Actor, id uuid.UUID) error {
	r, err := s.ownTicketRegistration(ctx, a, authz.EventsTicketResend, id)
	if err != nil {
		return err
	}
	t, _, err := s.liveTicket(ctx, r)
	if err != nil {
		return err
	}
	if s.d.Queue == nil {
		return problem.ErrUnavailable
	}
	task := resendTask{RegistrationID: r.ID, EventID: r.EventID, OrgID: r.OrgID, TicketVersion: t.Version, Ref: uuid.NewV7()}
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		return s.d.Audit.Log(ctx, auditEvent(a, authz.EventsTicketResend, ticketAggType, t.ID, r.OrgID, "",
			map[string]any{"registration_id": r.ID.String(), "ticket_version": t.Version}))
	})
	if err != nil {
		return err
	}
	if err := s.d.Queue.Enqueue(ctx, ResendTicketTaskName, task, ResendTicketTaskName+":"+task.Ref.String(),
		queue.Queue(queue.Critical)); err != nil {
		return problem.ErrUnavailable.Wrap(err)
	}
	return nil
}

// ticketAggType is the audit resource type and outbox aggregate of tickets.
const ticketAggType = "ticket"

// ReissueMyTicket implements Service: the registrant's ticket becomes
// version + 1 (the old one is revoked).
func (s *service) ReissueMyTicket(ctx context.Context, a authz.Actor, id uuid.UUID) (TicketView, error) {
	r, err := s.ownTicketRegistration(ctx, a, authz.EventsTicketReissue, id)
	if err != nil {
		return TicketView{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op string
		ID uuid.UUID
	}{"reissue_own", r.ID})
	if err != nil {
		return TicketView{}, err
	}
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (int, error) {
		// The event row FOR UPDATE: a concurrent scan (FOR KEY SHARE) of the
		// old QR either commits first (the registration is then checked in:
		// 409 here) or waits and sees the old ticket revoked.
		e, err := s.d.Store.LockEvent(ctx, r.EventID)
		switch {
		case err != nil:
			return 0, err
		case e.OrgID != r.OrgID:
			return 0, ErrNotFound
		}
		cur, err := s.d.Store.LockRegistration(ctx, e.OrgID, e.ID, r.ID)
		if err != nil {
			return 0, err
		}
		return s.reissue(ctx, a, e, cur)
	})
	if err != nil {
		return TicketView{}, err
	}
	cur, err := s.d.Store.GetUserRegistration(ctx, a.UserID, r.ID)
	if err != nil {
		return TicketView{}, err
	}
	return s.ticketView(ctx, cur)
}

// ReissueRegistrationTicket implements Service: the organiser's managers
// reissue the ticket of a registration of their event (the token is never
// returned to them; the registrant gets it by e-mail).
func (s *service) ReissueRegistrationTicket(ctx context.Context, a authz.Actor, eventID, id uuid.UUID) (int, error) {
	hash, err := idempotency.Hash(struct {
		Op    string
		Event uuid.UUID
		ID    uuid.UUID
	}{"reissue_org", eventID, id})
	if err != nil {
		return 0, err
	}
	v, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (int, error) {
		e, _, err := s.locked(ctx, a, authz.EventsTicketReissue, eventID)
		if err != nil {
			return 0, err
		}
		r, err := s.d.Store.LockRegistration(ctx, e.OrgID, e.ID, id)
		if err != nil {
			return 0, err
		}
		return s.reissue(ctx, a, e, r)
	})
	return v, err
}

// reissue moves a confirmed registration's ticket to the next version in
// the caller's transaction (the event row is locked FOR UPDATE first, so
// no scan runs meanwhile): the active
// ticket is revoked (reissued), a new one issued, the change audited and
// ticket.reissued written (O). A registration without a seat, of an event
// that is not published, or already checked in is 409.
func (s *service) reissue(ctx context.Context, a authz.Actor, e Event, r Registration) (int, error) {
	if r.Status != RegConfirmed || e.Status != StatusPublished {
		return 0, ErrInvalidState
	}
	now := s.now()
	if _, err := s.d.Store.RevokeTickets(ctx, e.OrgID, r.ID, RevokeReissued, now); err != nil {
		return 0, err
	}
	t, err := s.issueTicket(ctx, r, now)
	if err != nil {
		return 0, err
	}
	if err := s.d.Audit.Log(ctx, auditEvent(a, authz.EventsTicketReissue, ticketAggType, t.ID, e.OrgID, "",
		map[string]any{"registration_id": r.ID.String(), "event_id": e.ID.String(), "ticket_version": t.Version})); err != nil {
		return 0, err
	}
	env, err := domainevent.New(domainevent.TicketReissued{RegistrationID: r.ID, TicketID: t.ID, TicketVersion: t.Version},
		ticketAggType, t.ID)
	if err != nil {
		return 0, err
	}
	if err := s.d.Outbox.Add(ctx, env); err != nil {
		return 0, err
	}
	return t.Version, nil
}
