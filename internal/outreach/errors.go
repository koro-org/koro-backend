package outreach

import "koro/internal/platform/problem"

// Errors of the outreach module (05 §4.2; 04 §1.10, §2.8). The org status
// rules of 03 §2.3 reuse the orgs codes (orgs.ErrOrgSuspended,
// orgs.ErrOrgNotVerified, orgs.ErrInvalidState): a code is registered once
// (06 H-52). students.ErrAudienceTooSmall is a cross-module sentinel, not an
// HTTP problem; outreach answers it with its own ErrAudienceTooSmall.
var (
	// ErrAudienceTooSmall: the targeting reaches fewer than 10 students
	// (k-anonymity, 04 §V.2). It carries no number, not even "below 10 by
	// how much": an inline definition and a saved segment fail the same way.
	ErrAudienceTooSmall = problem.New(problem.Invalid, "outreach.audience_too_small",
		"The audience is too small to target")
	// ErrSensitiveTarget: a company (or the platform) targets a sensitive
	// club (03 Y2, §3.4; 02 §13.1). Only the club's own club_admin may
	// address its members.
	ErrSensitiveTarget = problem.New(problem.Invalid, "outreach.sensitive_target",
		"Sensitive clubs cannot be targeted")
	// ErrTargetOutOfScope: a key outside the actor's targeting scope (03
	// k16): a club_admin beyond club:<own club>, a university_rep without
	// uni:<own university> or with a club, an actor without a targeting
	// role in the org.
	ErrTargetOutOfScope = problem.New(problem.Invalid, "outreach.target_out_of_scope",
		"The audience is outside the organization's targeting scope")
	// ErrInvalidState: a change the segment's state does not allow
	// (updating or compiling an archived segment).
	ErrInvalidState = problem.New(problem.Conflict, "outreach.invalid_state",
		"Segment status does not allow this change")
	// ErrEmptyPatch: updateSegment without a field to change.
	ErrEmptyPatch = problem.New(problem.Invalid, "outreach.empty_patch", "Nothing to update")

	// ErrNotFound: the segment (or org) does not exist or is not the
	// caller's org's (IDOR: another org's segment is 404).
	ErrNotFound = problem.ErrNotFound
	// ErrPreconditionRequired / ErrPreconditionFailed: If-Match missing or
	// stale on updateSegment (04 §1.13).
	ErrPreconditionRequired = problem.ErrPreconditionRequired
	ErrPreconditionFailed   = problem.ErrPreconditionFailed
	// ErrRateLimited: the org's daily audience count budget is spent
	// (difference attacks, see Targeting).
	ErrRateLimited = problem.ErrRateLimited
)
