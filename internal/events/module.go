// Package events is the event module (01 §4.10). BE-024 builds the core:
// the event lifecycle (draft → published → cancelled; a draft may be
// deleted), editing with optimistic concurrency (ETag / If-Match), the
// visibility rules of the public list and the event page, the empty
// registration form draft created with every event, and the event staff
// (scanner / manager). Forms (BE-026), registrations (BE-028), tickets and
// check-in (BE-031), exports (BE-050) and the live board (BE-054) build on
// it.
//
// Rules the package enforces:
//
//   - Org isolation (03 §3.6, §7.6, 06 H-38): an event is addressed by its
//     id; its org_id comes from the row, never from the request, and every
//     later read and write is scoped to that org. An event the caller may
//     not see is 404, a visible one without the role 403. The form draft
//     and the staff rows copy org_id from the events row.
//   - Visibility (03 §3.6): public events are everyone's; university
//     events are the given university's verified students', the
//     organiser's members' and the staff's; members events are the
//     organiser's members' and the staff's; a draft is its organiser's
//     club_admin / university_rep's. Platform roles see everything. The
//     public list never carries personal data, and online_url is shown only
//     to the event's managers (approved attendees: BE-028).
//   - Sensitive events (02 §11.1): is_sensitive_category or a sensitive
//     club as organiser. The flag travels in Events.Get and in the authz
//     Attrs, never in a public DTO or a log.
//   - Who organises (03 §5.6, T-17): clubs, universities and the platform
//     org; companies cannot create events in v1 (k13, no entitlement
//     feature). A suspended org's members cannot write on its behalf (403
//     orgs.org_suspended, 03 §2.3); a closed org takes no change.
//   - State machine: publish needs a draft whose dates are valid, a link
//     for online/hybrid events and, when registration is required, a
//     published form version (422 events.form_required; versions are
//     BE-026). cancel needs a published event and emits event.cancelled
//     (O) in the same transaction; a schedule or venue change of a
//     published event emits event.schedule_changed (O). Only a draft may be
//     deleted (409 otherwise; the database trigger is the second layer).
//   - Staff (03 §1.1, T-65): the organiser's club_admin / university_rep
//     (career_staff) and, for the platform org, platform_admin add, change
//     and remove scanners and managers; the account must exist (404
//     otherwise, invitations are phase 2). The staff list shows first name
//     and last-name initial only.
//   - Turkish search (02 §0.3, 05 §9.5): BE-020's pattern — koro_tr full
//     text plus koro_norm substring / trigram matches on the title.
package events

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/outbox"
	"koro/internal/platform/privacy"
)

// Events is the read contract of other modules (01 §4.10).
type Events interface {
	// Get returns the summary of an event (any status) or ErrNotFound.
	// Sensitive = is_sensitive_category OR the organiser is a sensitive club.
	Get(ctx context.Context, eventID uuid.UUID) (EventSummary, error)
}

// Service is the events service: the cross-module contract plus the HTTP
// use cases of BE-024 (04 §2.6). Every method that takes an actor calls
// authz.Can first (05 §5.3).
type Service interface {
	Events

	// ListEvents is listEvents (events.event.list, P): published events the
	// caller may see.
	ListEvents(ctx context.Context, a authz.Actor, q ListQuery) (EventPage, error)
	// GetEvent is getEvent (events.event.read, P).
	GetEvent(ctx context.Context, a authz.Actor, id uuid.UUID) (EventView, error)
	// ListOrgEvents is listOrgEvents (events.event.list_org): drafts included.
	ListOrgEvents(ctx context.Context, a authz.Actor, orgID uuid.UUID, q OrgListQuery) (EventPage, error)
	// CreateEvent is createEvent (events.event.create): a draft with an
	// empty form draft in the same transaction.
	CreateEvent(ctx context.Context, a authz.Actor, orgID uuid.UUID, in EventInput) (EventView, error)
	// UpdateEvent is updateEvent (events.event.update): ifMatch is the
	// If-Match header (04 §1.13: missing → 428, stale → 412).
	UpdateEvent(ctx context.Context, a authz.Actor, id uuid.UUID, ifMatch string, p EventPatch) (EventView, error)
	// PublishEvent is publishEvent (events.event.publish): draft → published.
	PublishEvent(ctx context.Context, a authz.Actor, id uuid.UUID) (EventView, error)
	// CancelEvent is cancelEvent (events.event.cancel): published →
	// cancelled with event.cancelled in the same transaction.
	CancelEvent(ctx context.Context, a authz.Actor, id uuid.UUID, reason string) (EventView, error)
	// DeleteEventDraft is deleteEventDraft (events.event.delete): drafts only.
	DeleteEventDraft(ctx context.Context, a authz.Actor, id uuid.UUID) error

	// ListStaff is listEventStaff (events.staff.list).
	ListStaff(ctx context.Context, a authz.Actor, eventID uuid.UUID) ([]StaffView, error)
	// AddStaff is addEventStaff (events.staff.manage): the account of email
	// (404 when there is none) becomes a scanner or manager.
	AddStaff(ctx context.Context, a authz.Actor, eventID uuid.UUID, email string, role StaffRole) (StaffView, error)
	// UpdateStaff is updateEventStaff (events.staff.manage).
	UpdateStaff(ctx context.Context, a authz.Actor, eventID, userID uuid.UUID, role StaffRole) (StaffView, error)
	// RemoveStaff is removeEventStaff (events.staff.manage).
	RemoveStaff(ctx context.Context, a authz.Actor, eventID, userID uuid.UUID) error
}

// Deps are the dependencies of the events module. Store, Tx, Outbox,
// Audit, Orgs, Catalog, Idempotency, Clock and Log are required (New
// returns an error otherwise).
type Deps struct {
	Store       Store
	Tx          db.TxManager
	Outbox      outbox.Writer       // event.cancelled, event.schedule_changed (O)
	Audit       AuditLogger         // every write, same transaction
	Orgs        Orgs                // the organiser's kind, status and name
	Catalog     Catalog             // sensitive clubs, a club's university, universities
	Idempotency *idempotency.Keeper // optional Idempotency-Key of the POST operations (06 H-5)
	Clock       clock.Clock
	Log         *slog.Logger

	// Users is identity's user directory (staff e-mail lookup, names). nil
	// → addEventStaff finds nobody (404) and staff names are empty.
	Users Users
	// Files attaches and serves the event cover. nil → cover_file_id is
	// refused (503) and no cover_url is returned.
	Files Files
}

func (d Deps) validate() error {
	var errs []error
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"Store", d.Store != nil}, {"Tx", d.Tx != nil}, {"Outbox", d.Outbox != nil}, {"Audit", d.Audit != nil},
		{"Orgs", d.Orgs != nil}, {"Catalog", d.Catalog != nil}, {"Idempotency", d.Idempotency != nil},
		{"Clock", d.Clock != nil}, {"Log", d.Log != nil},
	} {
		if !c.ok {
			errs = append(errs, fmt.Errorf("events.Deps.%s is missing", c.name))
		}
	}
	return errors.Join(errs...)
}

// Module is what events hands to the composition root.
type Module struct {
	Service Service
	// Privacy is the KVKK provider (event staff assignments of a user; the
	// events rows hold no personal data, 02 §11.1).
	Privacy privacy.Provider

	svc *service
}

// New builds the events module; a missing dependency is a wiring bug that
// stops the process at start.
func New(d Deps) (Module, error) {
	if err := d.validate(); err != nil {
		return Module{}, err
	}
	s := &service{d: d}
	return Module{Service: s, Privacy: privacyProvider{s: s}, svc: s}, nil
}

type service struct{ d Deps }

var _ Service = (*service)(nil)

// now is the service clock in the database's precision (microseconds).
func (s *service) now() time.Time { return s.d.Clock.Now().UTC().Truncate(time.Microsecond) }
