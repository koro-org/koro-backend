package events

import (
	"errors"

	"koro/internal/platform/problem"
)

var errClientScan = errors.New("events: client_scan_id belongs to another check-in")

// Errors of the events module (05 §4.2; 04 §1.10, §2.6). The org status
// rules of 03 §2.3 reuse the orgs codes (orgs.ErrOrgSuspended,
// orgs.ErrInvalidState): a problem code is registered once (06 H-52).
var (
	// ErrInvalidState: a transition outside the state machine (04 §1.10):
	// publishing a non-draft, cancelling a draft or a cancelled event,
	// deleting a non-draft, editing a cancelled or completed event.
	ErrInvalidState = problem.New(problem.Conflict, "events.invalid_state", "Event status does not allow this change")
	// ErrFormRequired: publishing an event with requires_registration but
	// no published form version (BE-024 ticket; versions are BE-026).
	ErrFormRequired = problem.New(problem.Invalid, "events.form_required",
		"A published registration form is required to publish this event")
	// ErrSlugTaken: the requested slug belongs to another event of the org
	// (events_org_slug_uq).
	ErrSlugTaken = problem.New(problem.Conflict, "events.slug_taken", "Slug is already in use by another event of the organization")
	// ErrEmptyPatch: updateEvent or updateEventStaff without a field to change.
	ErrEmptyPatch = problem.New(problem.Invalid, "events.empty_patch", "Nothing to update")
	// ErrStaffExists: the user is already a staff member of the event
	// (event_staff primary key).
	ErrStaffExists = problem.New(problem.Conflict, "events.staff_exists", "User is already a staff member of this event")

	// ErrFormBlockedField: the form draft has a field of the lint class
	// blocked (ID, card or account numbers, passwords, special categories
	// 02 does not allow); there is no override (02 §11.4, 04 §1.10).
	ErrFormBlockedField = problem.New(problem.Invalid, "events.form_blocked_field",
		"The form asks for data that may not be collected")
	// ErrLintOverrideRequired: the draft has a warn_special or
	// review_identifier field and no lint_override_reason (04 §1.10).
	ErrLintOverrideRequired = problem.New(problem.Invalid, "events.lint_override_required",
		"A reason is required to publish a form with sensitive or identifier fields")
	// ErrFormUnchanged: the draft equals the current version (R-003 §2.5.2:
	// the schema hash prevents a duplicate publication).
	ErrFormUnchanged = problem.New(problem.Conflict, "events.form_unchanged", "The draft equals the current form version")
	// ErrFormIncompatible: the draft reuses an earlier key with another
	// type, or adds a required field after the first registration (R-003
	// §2.5.2).
	ErrFormIncompatible = problem.New(problem.Invalid, "events.form_incompatible",
		"The draft changes earlier fields in a way existing registrations cannot follow")

	// ErrCapacityFull: the event has no seat left and no open waiting list
	// (04 §E.2; the conditional counter UPDATE took no seat).
	ErrCapacityFull = problem.New(problem.Conflict, "events.capacity_full", "The event is full")
	// ErrAlreadyRegistered: the user has a live registration of the event
	// (event_registrations_active_uq; 04 §E.2).
	ErrAlreadyRegistered = problem.New(problem.Conflict, "events.already_registered",
		"You are already registered for this event")
	// ErrFormVersionStale: the answers were given to a form version that is
	// no longer the current one (04 §E.2): reload the form.
	ErrFormVersionStale = problem.New(problem.Conflict, "events.form_version_stale",
		"The registration form has changed; reload it")
	// ErrRegistrationClosed: registration is not open (window, a past,
	// cancelled or completed event), or a confirmed registration is
	// cancelled after cancel_deadline_at (04 §E.6).
	ErrRegistrationClosed = problem.New(problem.Conflict, "events.registration_closed",
		"Registration for this event is closed")

	// ErrClientScanTaken: the client_scan_id of a check-in belongs to
	// another check-in (another event's, another staff member's or another
	// ticket's scan; event_checkins_client_scan_id_uq). 409 conflict, never
	// that scan's answer (T-137). BE-031.
	ErrClientScanTaken = problem.ErrConflict.Wrap(errClientScan)

	// ErrExportNotReady: downloadEventExport of an export whose file is
	// not ready: still queued, failed, or deleted after its 7 days (04
	// §2.6: only a ready export is downloaded). BE-050.
	ErrExportNotReady = problem.New(problem.Conflict, "events.export_not_ready", "The export file is not available")

	// ErrNotFound: the event (or staff member) does not exist or is not
	// visible to the caller (03 §3.6).
	ErrNotFound = problem.ErrNotFound
	// ErrPreconditionRequired / ErrPreconditionFailed: If-Match missing or
	// stale on updateEvent and putEventFormDraft (04 §1.13).
	ErrPreconditionRequired = problem.ErrPreconditionRequired
	ErrPreconditionFailed   = problem.ErrPreconditionFailed
)
