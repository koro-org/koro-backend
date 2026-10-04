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
//
// BE-026 adds the registration form (ADR-0013, R-003 §2.5.1–2.5.2):
//
//   - The schema is a strict JSON document of at most 20 typed fields
//     (form_schema.go). The organiser edits one draft per event
//     (putEventFormDraft, If-Match) and publishes it as an immutable
//     version: schema_sha256, version_no + 1 and current_version_id change
//     in one transaction under the form row lock. A registration keeps its
//     version, so a later version never breaks it; an earlier key cannot
//     come back with another type, and no new required field may appear
//     after the first registration (422 events.form_incompatible).
//   - KVKK lint (form_lint.go; 02 §11.4–11.5, T-133): blocked fields are
//     never published (422 events.form_blocked_field, no override);
//     warn_special and review_identifier fields need a reasoned override
//     (422 events.lint_override_required). Every draft save returns the
//     lint preview. The rules are data the DPO manages (/admin/form-lint-
//     rules); a pattern must compile as RE2 (422).
//   - Answers (form_answers.go): ValidateAnswers is the single server-side
//     check (BE-028 calls it): a JSON Schema 2020-12 document derived from
//     the version plus the rules it cannot express. Typed answers, never
//     "everything is a string".
//   - Consent fields name a per-registration consent purpose; publishing
//     needs its current consent text (consent.Notices), the form returns
//     it, and a minor never sees a minor-restricted consent field (03 k12).
//   - Isolation: the form, its draft and its versions take event_id and
//     org_id from the event row (06 H-38); another org's form draft is 404.
//
// BE-028 adds the registrations (01 §6.3, 02 §11.6–11.7, 04 §E.2, §E.6):
//
//   - Who registers (03 §5.6, T-54, T-22): verified students who see the
//     event (a university event: its university's students); an account
//     without a student verification gets 403
//     students.verification_required; a minor answers the form without the
//     sponsor consent field (ForReader).
//   - The chain, in one transaction with the Idempotency-Key (06 H-5): the
//     conditional seat UPDATE (confirmed_count + 1 while below the
//     capacity; no interleaving exceeds it, proven by the concurrency test
//     in memory and PostgreSQL) → the registration → its ticket (only the
//     SHA-256 of the derived token; the QR and the e-mail are BE-031's) →
//     consent.Recorder for the consent fields answered yes → audit →
//     outbox. auto mode: confirmed, else waitlisted (open list, event not
//     started) or 409 events.capacity_full; manual mode: pending, then the
//     organiser decides (confirmed / waitlisted / rejected).
//   - Answers: validated with ValidateAnswers against the current version
//     (an older version: 409 events.form_version_stale); warn_special
//     answers are envelope encrypted (answers_sensitive_enc, purpose
//     event_answers_sensitive:<id>), never in a list, opened only for the
//     owner and the organiser's managers, every such read audited.
//   - Cancellation and the waiting list: a confirmed registration released
//     by its owner (until cancel_deadline_at), the organiser, the system
//     (event.cancelled) or the erasure gives its seat to the first
//     waitlisted one (FOR UPDATE SKIP LOCKED, FIFO) in the same
//     transaction; promotion is idempotent (a seat by the conditional
//     UPDATE, a waiting row). Jobs: events.waitlist_expire,
//     events.reconcile_counts. Lock order: the events row, then
//     registrations.
//   - The organiser's reads are the event's sub-surface (03 §7.6), support
//     and platform_admin read by break-glass only, never a sensitive event.
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

// Registrations is the registration read contract of other modules
// (students' member card and admin view, T-241). No answers, no ticket.
type Registrations interface {
	// CountInOrg returns how many live or attended registrations userID has
	// for the events of orgID (club_event_registrations_count, 04 §V.4).
	CountInOrg(ctx context.Context, orgID, userID uuid.UUID) (int, error)
	// SummaryOf returns userID's registrations, newest first, without their
	// answers; registrations of sensitive events are left out (03 Y2: the
	// admin view is a break-glass read).
	SummaryOf(ctx context.Context, userID uuid.UUID) ([]RegistrationSummary, error)
}

// Service is the events service: the cross-module contract plus the HTTP
// use cases of BE-024 and BE-026 (04 §2.6). Every method that takes an actor calls
// authz.Can first (05 §5.3).
type Service interface {
	Events
	Registrations

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

	// GetForm is getEventForm (events.form.read, P): the current published
	// version as the caller may answer it, with its consent texts.
	GetForm(ctx context.Context, a authz.Actor, eventID uuid.UUID, locale string) (FormView, error)
	// GetFormDraft is getEventFormDraft (events.form.manage): the draft
	// with a lint preview.
	GetFormDraft(ctx context.Context, a authz.Actor, eventID uuid.UUID) (FormDraftView, error)
	// PutFormDraft is putEventFormDraft (events.form.manage): ifMatch is
	// the If-Match header (missing → 428, stale → 412); draft is the JSON
	// of the new schema.
	PutFormDraft(ctx context.Context, a authz.Actor, eventID uuid.UUID, ifMatch string, draft []byte) (FormDraftView, error)
	// PublishForm is publishEventForm (events.form.publish): the draft
	// becomes the next immutable version and the current one.
	PublishForm(ctx context.Context, a authz.Actor, eventID uuid.UUID, overrideReason string) (FormVersionView, error)

	// ListLintRules is adminListFormLintRules (events.lint_rule.manage).
	ListLintRules(ctx context.Context, a authz.Actor) ([]LintRule, error)
	// CreateLintRule is adminCreateFormLintRule (events.lint_rule.manage).
	CreateLintRule(ctx context.Context, a authz.Actor, in LintRuleInput) (LintRule, error)
	// UpdateLintRule is adminUpdateFormLintRule (events.lint_rule.manage).
	UpdateLintRule(ctx context.Context, a authz.Actor, id uuid.UUID, p LintRulePatch) (LintRule, error)

	// CreateRegistration is createRegistration (events.registration.create;
	// Idempotency-Key required): the registration chain of 01 §6.3 in one
	// transaction with the key (06 H-5).
	CreateRegistration(ctx context.Context, a authz.Actor, eventID uuid.UUID, in RegistrationInput) (RegistrationView, error)
	// ListMyRegistrations is listMyRegistrations (events.registration.list_own).
	ListMyRegistrations(ctx context.Context, a authz.Actor, q MyRegistrationQuery) (RegistrationPage, error)
	// GetMyRegistration is getMyRegistration (events.registration.read_own):
	// the answers too, the sensitive ones opened for the owner (audited).
	GetMyRegistration(ctx context.Context, a authz.Actor, id uuid.UUID) (RegistrationView, error)
	// CancelMyRegistration is cancelMyRegistration
	// (events.registration.cancel_own): a confirmed registration is
	// cancelled (until cancel_deadline_at) and the waiting list moves up; a
	// pending or waitlisted one is withdrawn.
	CancelMyRegistration(ctx context.Context, a authz.Actor, id uuid.UUID, reason string) (RegistrationView, error)
	// ListEventRegistrations is listEventRegistrations
	// (events.registration.list; support and platform_admin by
	// break-glass): never the sensitive answers; always audited.
	ListEventRegistrations(ctx context.Context, a authz.Actor, eventID uuid.UUID, q EventRegistrationQuery, access Access) (RegistrationPage, error)
	// GetEventRegistration is getEventRegistration (events.registration.read):
	// the sensitive answers for the organiser's managers only (audited),
	// never by break-glass.
	GetEventRegistration(ctx context.Context, a authz.Actor, eventID, id uuid.UUID, access Access) (RegistrationView, error)
	// DecideRegistration is decideEventRegistration
	// (events.registration.decide): a pending registration of a manual
	// event becomes confirmed, waitlisted or rejected.
	DecideRegistration(ctx context.Context, a authz.Actor, eventID, id uuid.UUID, d Decision) (RegistrationView, error)
	// CancelEventRegistration is cancelEventRegistration
	// (events.registration.cancel): cancelled_by_kind = org.
	CancelEventRegistration(ctx context.Context, a authz.Actor, eventID, id uuid.UUID, reason string) (RegistrationView, error)
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
	// Notices is consent's current consent texts. nil → a form with a
	// consent field cannot be published (503) and getEventForm lists no
	// consent_notices.
	Notices Notices

	// Consents records the consent fields of a registration (BE-028). nil →
	// a registration granting a consent field is refused (503).
	Consents Recorder
	// Sealer encrypts the warn_special answers (BE-028). nil → a
	// registration with such an answer is refused (503) and stored ones are
	// not opened.
	Sealer Sealer
	// Tickets derives the ticket tokens of confirmed registrations
	// (CRYPTO_TOKEN_KEYS). nil → a registration cannot be confirmed (503).
	Tickets TicketKeys
	// Sender sends the in-app notices of the registration events (worker).
	// nil → the notice tasks fail and are retried.
	Sender Sender
	// Students adds university, department, class year and avatar to the
	// organiser's registration views. nil → those fields are absent.
	Students Students
	// Retention returns a retention period of kvkk.retention_policies
	// (event_form_answers, event_form_answers_sensitive) for the purge
	// dates of a registration; nil or not found → 90 and 30 days.
	Retention func(ctx context.Context, code string) (time.Duration, bool, error)
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
	// Privacy is the KVKK provider (event staff assignments, registrations
	// with their answers and tickets; the events rows hold no personal
	// data, 02 §11.1).
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
