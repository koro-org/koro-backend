package kvkk

import "koro/internal/platform/problem"

// Errors of the kvkk module (05 §4.2; 04 §2.22). Messages and field errors
// never carry a request's content: details, names and addresses are
// personal data.
var (
	// ErrNotFound: the request does not exist or is not the caller's.
	ErrNotFound = problem.ErrNotFound

	// ErrDuplicateRequest: the subject already has an open request of
	// this type (one at a time; the open one answers it).
	ErrDuplicateRequest = problem.New(problem.Conflict, "kvkk.duplicate_request", "An open request of this type exists")
	// ErrState: the request is not in a state that allows the step.
	ErrState = problem.New(problem.Conflict, "kvkk.invalid_state", "Request is not in a state that allows this")
	// ErrIdentityNotVerified: completing a request whose applicant's
	// identity has not been verified (KVKK md. 13; 02 §16.1).
	ErrIdentityNotVerified = problem.New(problem.Conflict, "kvkk.identity_not_verified", "Applicant identity is not verified")
	// ErrSecondApprovalRequired: a profiling report needs two approvers
	// (03 §6.1) before it is released.
	ErrSecondApprovalRequired = problem.New(problem.Conflict, "kvkk.second_approval_required", "A second approval is required")
	// ErrApprovalNotApplicable: only profiling_access takes approvals.
	ErrApprovalNotApplicable = problem.New(problem.Conflict, "kvkk.approval_not_applicable", "This request type takes no approval")
	// ErrOwnRequest: staff never handle a request about themselves.
	ErrOwnRequest = problem.New(problem.Forbidden, "kvkk.own_request", "Staff cannot handle their own request")

	// ErrInvalidRequest: a field breaks the request rules.
	ErrInvalidRequest = problem.New(problem.Invalid, "kvkk.invalid_request", "Request is invalid")
	// ErrUnknownUser: the DPO entered a user ID that does not exist.
	ErrUnknownUser = problem.New(problem.Invalid, "kvkk.unknown_user", "Unknown user")

	// ErrExportNotReady: no export file exists (yet) for this request.
	ErrExportNotReady = problem.New(problem.NotFound, "kvkk.export_not_ready", "No export file for this request")
	// ErrExportExpired: the export file was deleted after its retention
	// period (T-83: 7 days).
	ErrExportExpired = problem.New(problem.Gone, "kvkk.export_expired", "The export file has expired")

	// ErrReauthUnavailable: an R step while no re-authentication verifier
	// is bound. Fail closed.
	ErrReauthUnavailable = problem.New(problem.Unavailable, "kvkk.reauth_unavailable", "Re-authentication is not available")
)
