package events

import "koro/internal/platform/problem"

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

	// ErrNotFound: the event (or staff member) does not exist or is not
	// visible to the caller (03 §3.6).
	ErrNotFound = problem.ErrNotFound
	// ErrPreconditionRequired / ErrPreconditionFailed: If-Match missing or
	// stale on updateEvent (04 §1.13).
	ErrPreconditionRequired = problem.ErrPreconditionRequired
	ErrPreconditionFailed   = problem.ErrPreconditionFailed
)
