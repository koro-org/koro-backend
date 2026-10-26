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
	// ErrInvalidState: a change the state of a segment or a dispatch does
	// not allow (updating or compiling an archived segment; cancelling a
	// dispatch that is no longer scheduled; dispatching a hidden or removed
	// post).
	ErrInvalidState = problem.New(problem.Conflict, "outreach.invalid_state",
		"The current status does not allow this change")
	// ErrEmptyPatch: updateSegment without a field to change.
	ErrEmptyPatch = problem.New(problem.Invalid, "outreach.empty_patch", "Nothing to update")
	// ErrPostNotTargeted: createDispatch on a post that is not a targeted
	// (segment) post: only a targeted post has an audience to notify (04
	// §2.8).
	ErrPostNotTargeted = problem.New(problem.Invalid, "outreach.post_not_targeted",
		"Only a targeted post can be dispatched")
	// ErrDispatchExists: the post already has a dispatch (at most one per
	// post, outreach_dispatches_post_id_uq).
	ErrDispatchExists = problem.New(problem.Conflict, "outreach.dispatch_exists",
		"The post already has a dispatch")
	// ErrPostGone: a dispatch's post was deleted or removed before the run
	// (DispatchDTO failure_reason only; no request answers it).
	ErrPostGone = problem.New(problem.Gone, "outreach.post_gone", "The post of the dispatch is gone")
	// ErrPostNotPublished: a dispatch's post is not published (any more) at
	// the run (DispatchDTO failure_reason only).
	ErrPostNotPublished = problem.New(problem.Conflict, "outreach.post_not_published",
		"The post of the dispatch is not published")

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
