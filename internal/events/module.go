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
//
// BE-031 adds the ticket, the door and the attendance (ADR-0013, 01 §6.3
// "Check-in", 02 §11.7–11.8, 04 §E.3, §E.5, R-003 §2.5.4–2.5.6):
//
//   - The QR payload is "KORO1:<token>", the token derived by HMAC from
//     the registration and the ticket version under a key of
//     CRYPTO_TOKEN_KEYS (no PII, no URL); only its SHA-256 is stored, so
//     the app and every mail show the same re-derived QR. A missing key
//     stops the process (wiring). A reissue moves to version + 1 and
//     revokes the old QR.
//   - The ticket e-mail (worker, outbox): the QR as an inline CID PNG
//     (QRRenderer), the manual code as text and an .ics (REQUEST; CANCEL
//     on cancellation; updated on a schedule change), deduplicated per
//     ticket version; the address is never logged.
//   - Check-in (event staff and the organiser's managers): one transaction
//     with the Idempotency-Key; the conditional UPDATE confirmed →
//     checked_in lets exactly one of concurrent scans of a ticket pass,
//     the partial unique index of event_checkins is the second layer; a
//     client_scan_id retry returns the first answer (looked up with the
//     event and the staff member, T-137). Outcomes are 200 with result
//     (T-70); another event's ticket is wrong_event without the attendee.
//   - Attendance: closing it (after the end; the system 24 hours later)
//     turns the confirmed registrations without check-in into no_show in
//     batches; a check-in and a no_show keep confirmed_count consistent
//     (checked_in holds its seat, no_show releases it).
//
// BE-050 adds the registration exports (export.go, export_build.go; 02
// §11.9, R-003 §2.6-5, T-37, T-69): a reasoned, field-chosen, re-
// authenticated request of the organiser's managers, built by the worker
// as CSV or XLSX (no e-mail, never a special-category answer, formula-safe
// cells, a personal-data warning row), downloaded only by its requester
// while ready (counted and audited), deleted after 7 days by
// events.purge_exports; the row stays 2 years (event_export_log).
//
// BE-054 adds the live check-in board (live.go, live_handler.go; 04 §W): a
// WebSocket over platform/realtime, authorised like getCheckinStats when it
// opens and re-checked at least every minute; check-ins and voids are
// published after the commit.
package events

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/outbox"
	"koro/internal/platform/privacy"
	"koro/internal/platform/queue"
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
// use cases of BE-024, BE-026, BE-028, BE-031, BE-050 and BE-054 (04 §2.6, §W). Every method that takes an actor calls
// authz.Can first (05 §5.3).
type Service interface {
	Events
	Registrations
	Participation

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

	// GetMyTicket is getMyTicket (events.ticket.read_own): the QR payload
	// and manual code of a seat holder's active ticket (409 otherwise).
	GetMyTicket(ctx context.Context, a authz.Actor, id uuid.UUID) (TicketView, error)
	// ResendMyTicket is resendMyTicket (events.ticket.resend): the same
	// ticket mailed again from the worker (503 when the queue is down).
	ResendMyTicket(ctx context.Context, a authz.Actor, id uuid.UUID) error
	// ReissueMyTicket is reissueMyTicket (events.ticket.reissue, O): version
	// + 1, the old QR revoked, the new one mailed.
	ReissueMyTicket(ctx context.Context, a authz.Actor, id uuid.UUID) (TicketView, error)
	// ReissueRegistrationTicket is reissueRegistrationTicket
	// (events.ticket.reissue, S): the organiser's managers reissue a
	// registration's ticket; it returns the new version (never the token).
	ReissueRegistrationTicket(ctx context.Context, a authz.Actor, eventID, id uuid.UUID) (int, error)
	// CreateCheckin is createCheckin (events.checkin.create; Idempotency-Key
	// required): the business outcome is in the result (T-70).
	CreateCheckin(ctx context.Context, a authz.Actor, eventID uuid.UUID, in CheckinInput) (CheckinResult, error)
	// VoidCheckin is voidCheckin (events.checkin.void): the registration is
	// confirmed again.
	VoidCheckin(ctx context.Context, a authz.Actor, eventID, id uuid.UUID, reason string) (CheckinVoidView, error)
	// ListCheckins is listCheckins (events.checkin.list): the latest scan
	// first, attendees as "Ayşe K.".
	ListCheckins(ctx context.Context, a authz.Actor, eventID uuid.UUID, limit int, after *After) (CheckinPage, error)
	// GetCheckinStats is getCheckinStats (events.checkin.stats).
	GetCheckinStats(ctx context.Context, a authz.Actor, eventID uuid.UUID) (CheckinStats, error)
	// OpenLiveBoard authorises the live board's WebSocket (events.checkin.stats,
	// 04 §W; BE-054) when it opens and at every re-check.
	OpenLiveBoard(ctx context.Context, a authz.Actor, eventID uuid.UUID) (LiveBoard, error)
	// LiveSnapshot is the board's first message (the counters).
	LiveSnapshot(ctx context.Context, b LiveBoard) ([]byte, error)
	// FinalizeEventAttendance is finalizeEventAttendance
	// (events.attendance.finalize): after the end, once (409 otherwise); it
	// returns the number of no-shows the worker marks.
	FinalizeEventAttendance(ctx context.Context, a authz.Actor, eventID uuid.UUID) (int, error)

	// CreateExport is createEventExport (events.export.create, R; BE-050):
	// a queued export of the event's registrations with the reason and the
	// allowed fields; the worker builds the file (export.go).
	CreateExport(ctx context.Context, a authz.Actor, eventID uuid.UUID, in ExportInput, proof identity.ReauthProof) (Export, error)
	// ListExports is listEventExports (events.export.list): newest first.
	ListExports(ctx context.Context, a authz.Actor, eventID uuid.UUID, limit int, after *After) (ExportPage, error)
	// DownloadExport is downloadEventExport (events.export.download, S·O):
	// a presigned URL of a ready export's file, counted and audited.
	DownloadExport(ctx context.Context, a authz.Actor, eventID, exportID uuid.UUID) (ExportDownload, error)
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

	// QR renders the ticket QR of the e-mails (BE-031; NewQRRenderer). nil → the
	// ticket mail tasks fail and are retried.
	QR QRRenderer
	// Contacts is identity's address lookup of the ticket mails (worker
	// only). nil → the mail tasks fail and are retried.
	Contacts Contacts
	// Queue enqueues the resend task (mechanism A). nil → resendMyTicket
	// answers 503.
	Queue queue.Enqueuer
	// LinkBaseURL is the web origin of the links in the mails (the ticket
	// page; IDENTITY_LINK_BASE_URL).
	LinkBaseURL string
	// ICSOrganizer is the ORGANIZER address of the .ics files (SMTP_FROM);
	// empty → the files carry no ORGANIZER.
	ICSOrganizer string

	// Reauth verifies the re-authentication proof of createEventExport
	// (BE-050; identity). nil → createEventExport answers 503 (as without
	// Files or Queue, which the exports also need: the file is stored by
	// files, the build is a task).
	Reauth Reauthenticator

	// Live publishes the live board's messages after a check-in or a void
	// commits (BE-054; realtime.Hub). nil → nothing is published.
	Live LivePublisher
	// Destructions records each export file events.purge_exports deletes
	// in destruction_logs (T-318, BE-083; kvkk through hooks.destruction).
	// nil → the purge refuses (a destruction must not go unrecorded) and
	// the files wait for the next run.
	Destructions Destructions
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
	// with their answers and tickets, the exports a person requested; the
	// events rows hold no personal data, 02 §11.1).
	Privacy privacy.Provider
	// Sweeper ends the retention of form answers (special category ones
	// sooner) and staff assignments after the event, and of the export log
	// 2 years after the request (02 §24.2; BE-057, BE-050).
	Sweeper privacy.RetentionSweeper

	svc *service
}

// New builds the events module; a missing dependency is a wiring bug that
// stops the process at start.
func New(d Deps) (Module, error) {
	if err := d.validate(); err != nil {
		return Module{}, err
	}
	s := &service{d: d, batch: jobBatch}
	return Module{Service: s, Privacy: privacyProvider{s: s}, Sweeper: sweeper{s: s}, svc: s}, nil
}

type service struct {
	d Deps
	// batch is the rows per transaction of the jobs and consumers (01 §7:
	// 1000; tests make it smaller).
	batch int
}

var _ Service = (*service)(nil)

// now is the service clock in the database's precision (microseconds).
func (s *service) now() time.Time { return s.d.Clock.Now().UTC().Truncate(time.Microsecond) }
