package events

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"uuid"

	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/orgs"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
)

// eventAggType is the aggregate type of the events' domain events.
const eventAggType = "event"

// CreateEvent implements Service. Order: the org (404) → authz on its
// management surface (404/403; companies have no grant, k13) → the
// organiser kind → the org status (403 orgs.org_suspended) → the body
// (422) → in one transaction: the draft, its empty form draft, the cover
// attachment and the audit record.
func (s *service) CreateEvent(ctx context.Context, a authz.Actor, orgID uuid.UUID, in EventInput) (EventView, error) {
	o, err := s.org(ctx, orgID)
	if err != nil {
		return EventView{}, err
	}
	if err := authz.Can(ctx, a, authz.EventsEventCreate, orgResource(o)); err != nil {
		return EventView{}, err
	}
	if !organizerKinds[o.Kind] {
		return EventView{}, authz.ErrForbidden // T-17: no organiser kind beyond club, university, platform
	}
	if err := writable(a, o); err != nil {
		return EventView{}, err
	}
	base := Event{
		OrgID: o.ID, CreatedBy: a.UserID, Status: StatusDraft, Visibility: VisibilityPublic,
		RequiresRegistration: true, RegistrationMode: ModeAuto,
	}
	ap, err := s.apply(ctx, a, o, base, in.patch())
	if err != nil {
		return EventView{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op  string
		Org uuid.UUID
		In  EventInput
	}{"create", o.ID, in})
	if err != nil {
		return EventView{}, err
	}
	id, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		e := ap.next
		e.ID = uuid.NewV7()
		e, err := s.insertEvent(ctx, e, in.Slug != "")
		if err != nil {
			return uuid.Nil(), err
		}
		// The empty registration form draft (02 §11.3); org_id from the events row.
		if err := s.d.Store.InsertForm(ctx, e.OrgID, Form{
			ID: uuid.NewV7(), EventID: e.ID, DraftSchema: emptyFormSchema, UpdatedBy: &a.UserID,
		}); err != nil {
			return uuid.Nil(), err
		}
		if err := s.attachCover(ctx, a, e, ap.cover); err != nil {
			return uuid.Nil(), err
		}
		return e.ID, s.d.Audit.Log(ctx, auditEvent(a, authz.EventsEventCreate, eventAggType, e.ID, e.OrgID, "",
			map[string]any{"status": string(e.Status), "visibility": string(e.Visibility)}))
	})
	if err != nil {
		return EventView{}, err
	}
	return s.managedView(ctx, o, id)
}

// insertEvent writes e with its requested slug (taken → ErrSlugTaken) or
// with the first free slug derived from the title ("go-atolyesi",
// "go-atolyesi-2", …, then a random suffix).
func (s *service) insertEvent(ctx context.Context, e Event, requested bool) (Event, error) {
	if requested {
		stored, ok, err := s.d.Store.InsertEvent(ctx, e)
		switch {
		case err != nil:
			return Event{}, err
		case !ok:
			return Event{}, ErrSlugTaken
		}
		return stored, nil
	}
	base := textSlug(e.Title)
	for i := 1; i <= slugAttempts+1; i++ {
		var suffix string
		switch {
		case i == 1:
		case i <= slugAttempts:
			suffix = "-" + strconv.Itoa(i)
		default:
			b := make([]byte, 4)
			_, _ = rand.Read(b)
			suffix = "-" + hex.EncodeToString(b)
		}
		e.Slug = trimSlug(base, maxSlug-len(suffix)) + suffix
		stored, ok, err := s.d.Store.InsertEvent(ctx, e)
		if err != nil {
			return Event{}, err
		}
		if ok {
			return stored, nil
		}
	}
	return Event{}, ErrSlugTaken
}

// attachCover binds a new cover to the event in the transaction (files k30:
// the upload was authorised for event_cover already; checkCover checked
// its owner).
func (s *service) attachCover(ctx context.Context, a authz.Actor, e Event, cover *uuid.UUID) error {
	if cover == nil {
		return nil
	}
	return s.d.Files.Attach(ctx, a, *cover, files.Owner{Module: "events", EntityID: e.ID, Purpose: files.PurposeEventCover})
}

// locked reads the event for a change in the transaction carried by ctx:
// the row (locked; 404) → authz (404 invisible, 403 without the role) → the
// organiser's status (403 orgs.org_suspended; a closed org takes nothing).
// org_id comes from the row (06 H-38).
func (s *service) locked(ctx context.Context, a authz.Actor, act authz.Action, id uuid.UUID) (Event, orgs.OrgSummary, error) {
	e, err := s.d.Store.LockEvent(ctx, id)
	if err != nil {
		return Event{}, orgs.OrgSummary{}, err
	}
	if err := s.authorize(ctx, a, act, e); err != nil {
		return Event{}, orgs.OrgSummary{}, err
	}
	o, err := s.org(ctx, e.OrgID)
	if err != nil {
		return Event{}, orgs.OrgSummary{}, err
	}
	if err := writable(a, o); err != nil {
		return Event{}, orgs.OrgSummary{}, err
	}
	return e, o, nil
}

// UpdateEvent implements Service. Order: the event (404) → authz (404/403)
// → the org status (403) → the event status (409: cancelled and completed
// events are final) → If-Match (428/412) → the patch (422). A changed
// schedule or venue of a published event emits event.schedule_changed (O)
// in the same transaction.
func (s *service) UpdateEvent(ctx context.Context, a authz.Actor, id uuid.UUID, ifMatch string, p EventPatch) (EventView, error) {
	var (
		out Event
		org orgs.OrgSummary
	)
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		e, o, err := s.locked(ctx, a, authz.EventsEventUpdate, id)
		if err != nil {
			return err
		}
		org = o
		if e.Status != StatusDraft && e.Status != StatusPublished {
			return ErrInvalidState
		}
		if err := checkIfMatch(ifMatch, etagOf(e.UpdatedAt)); err != nil {
			return err
		}
		if p.empty() {
			return ErrEmptyPatch
		}
		ap, err := s.apply(ctx, a, o, e, p)
		if err != nil {
			return err
		}
		if out, err = s.d.Store.UpdateEvent(ctx, ap.next); err != nil {
			return err
		}
		if err := s.attachCover(ctx, a, out, ap.cover); err != nil {
			return err
		}
		if moved := scheduleChanges(ap.changed); e.Status == StatusPublished && len(moved) > 0 {
			if err := s.emit(ctx, domainevent.EventScheduleChanged{EventID: e.ID, OrgID: e.OrgID, Fields: moved}, e.ID); err != nil {
				return err
			}
		}
		if err := s.followRegistrations(ctx, e, out, ap.changed); err != nil {
			return err
		}
		return s.d.Audit.Log(ctx, auditEvent(a, authz.EventsEventUpdate, eventAggType, e.ID, e.OrgID, "",
			map[string]any{"fields": ap.changed}))
	})
	if err != nil {
		return EventView{}, err
	}
	return s.view(ctx, out, org, true), nil
}

// followRegistrations keeps the registrations in step with an edited
// event (BE-028, in the update's transaction under the event row lock): a
// new end moves the answer purge dates (02 §11.6), and a raised or removed
// capacity of a published event gives the new seats to the waiting list.
func (s *service) followRegistrations(ctx context.Context, before, after Event, changed []string) error {
	if contains(changed, "ends_at") {
		answers, sens, err := s.purgeDates(ctx, after.EndsAt)
		if err != nil {
			return err
		}
		if err := s.d.Store.ShiftPurgeDates(ctx, after.OrgID, after.ID, answers, sens); err != nil {
			return err
		}
	}
	raised := before.Capacity != nil && (after.Capacity == nil || *after.Capacity > *before.Capacity)
	if !raised || after.Status != StatusPublished {
		return nil
	}
	sens, err := s.sensitive(ctx, after)
	if err != nil {
		return err
	}
	return s.promote(ctx, after, sens, s.now())
}

// scheduleChanges keeps the schedule and venue fields of changed.
func scheduleChanges(changed []string) []string {
	out := []string{}
	for _, f := range changed {
		if contains(scheduleFields, f) {
			out = append(out, f)
		}
	}
	return out
}

// emit writes a domain event to the outbox in the transaction (O).
func (s *service) emit(ctx context.Context, p domainevent.Payload, eventID uuid.UUID) error {
	env, err := domainevent.New(p, eventAggType, eventID)
	if err != nil {
		return err
	}
	return s.d.Outbox.Add(ctx, env)
}

// PublishEvent implements Service: draft → published. The row must pass
// the rules of a published event (an online or hybrid event needs its
// link; 422), it must not have ended (422), and an event with registration
// needs a published form version (422 events.form_required; BE-026).
func (s *service) PublishEvent(ctx context.Context, a authz.Actor, id uuid.UUID) (EventView, error) {
	hash, err := idempotency.Hash(struct {
		Op    string
		Event uuid.UUID
	}{"publish", id})
	if err != nil {
		return EventView{}, err
	}
	var org orgs.OrgSummary
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		e, o, err := s.locked(ctx, a, authz.EventsEventPublish, id)
		if err != nil {
			return uuid.Nil(), err
		}
		org = o
		if e.Status != StatusDraft {
			return uuid.Nil(), ErrInvalidState
		}
		now := s.now()
		if !e.EndsAt.After(now) {
			return uuid.Nil(), fieldErr("body.ends_at", "a past event cannot be published")
		}
		next := e
		next.Status, next.PublishedAt = StatusPublished, &now
		if err := s.validateRow(ctx, o, next); err != nil {
			return uuid.Nil(), err
		}
		if e.RequiresRegistration {
			ok, err := s.d.Store.HasPublishedForm(ctx, e.OrgID, e.ID)
			if err != nil {
				return uuid.Nil(), err
			}
			if !ok {
				return uuid.Nil(), ErrFormRequired
			}
		}
		if _, err := s.d.Store.UpdateEvent(ctx, next); err != nil {
			return uuid.Nil(), err
		}
		return e.ID, s.d.Audit.Log(ctx, auditEvent(a, authz.EventsEventPublish, eventAggType, e.ID, e.OrgID, "", nil))
	})
	if err != nil {
		return EventView{}, err
	}
	return s.managedView(ctx, org, id)
}

// CancelEvent implements Service: published → cancelled with the reason;
// event.cancelled (O) in the same transaction (registrations, tickets and
// linked offerings follow in their modules: BE-028, BE-031, catalog).
func (s *service) CancelEvent(ctx context.Context, a authz.Actor, id uuid.UUID, reason string) (EventView, error) {
	hash, err := idempotency.Hash(struct {
		Op     string
		Event  uuid.UUID
		Reason string
	}{"cancel", id, reason})
	if err != nil {
		return EventView{}, err
	}
	var org orgs.OrgSummary
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		e, o, err := s.locked(ctx, a, authz.EventsEventCancel, id)
		if err != nil {
			return uuid.Nil(), err
		}
		org = o
		if e.Status != StatusPublished {
			return uuid.Nil(), ErrInvalidState
		}
		r, err := cleanText("body.cancel_reason", reason, minCancelReason, maxCancelReason)
		if err != nil {
			return uuid.Nil(), err
		}
		now := s.now()
		next := e
		next.Status, next.CancelledAt, next.CancelReason = StatusCancelled, &now, &r
		if _, err := s.d.Store.UpdateEvent(ctx, next); err != nil {
			return uuid.Nil(), err
		}
		if err := s.emit(ctx, domainevent.EventCancelled{EventID: e.ID, OrgID: e.OrgID}, e.ID); err != nil {
			return uuid.Nil(), err
		}
		return e.ID, s.d.Audit.Log(ctx, auditEvent(a, authz.EventsEventCancel, eventAggType, e.ID, e.OrgID, r, nil))
	})
	if err != nil {
		return EventView{}, err
	}
	return s.managedView(ctx, org, id)
}

// DeleteEventDraft implements Service: only a draft may be deleted (409
// otherwise); its form draft and staff rows go with it.
func (s *service) DeleteEventDraft(ctx context.Context, a authz.Actor, id uuid.UUID) error {
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		e, _, err := s.locked(ctx, a, authz.EventsEventDelete, id)
		if err != nil {
			return err
		}
		if e.Status != StatusDraft {
			return ErrInvalidState
		}
		if err := s.d.Store.DeleteDraft(ctx, e.OrgID, e.ID); err != nil {
			return err
		}
		return s.d.Audit.Log(ctx, auditEvent(a, authz.EventsEventDelete, eventAggType, e.ID, e.OrgID, "", nil))
	})
}

// managedView reads the event after a write by one of its managers.
func (s *service) managedView(ctx context.Context, o orgs.OrgSummary, id uuid.UUID) (EventView, error) {
	e, err := s.d.Store.GetEvent(ctx, id)
	if err != nil {
		return EventView{}, err
	}
	if o.ID != e.OrgID { // a replayed request: the org was not read in this call
		if o, err = s.org(ctx, e.OrgID); err != nil {
			return EventView{}, err
		}
	}
	return s.view(ctx, e, o, true), nil
}
