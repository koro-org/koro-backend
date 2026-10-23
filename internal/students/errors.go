package students

import (
	"errors"

	"koro/internal/orgs"
	"koro/internal/platform/problem"
)

// Errors of the students module (05 §4.2; 04 §1.10, §2.4).
var (
	// ErrMinorRestricted: a user under 18 turns on discoverability (level 2)
	// or contact sharing at level 3 (03 k9, Y4).
	ErrMinorRestricted = problem.New(problem.Invalid, "students.minor_restricted", "Not available to users under 18")
	// ErrUnknownDepartment: the department does not exist, is inactive or
	// belongs to another university than the student's verified one.
	ErrUnknownDepartment = problem.New(problem.Invalid, "students.unknown_department",
		"Unknown department for the student's university")
	// ErrEmptyPatch: a PATCH without a field to change.
	ErrEmptyPatch = problem.New(problem.Invalid, "students.empty_patch", "Nothing to update")
	// ErrCVLimit: a fourth CV (02 §9.6: at most 3; 04 §2.4 createMyCv 409).
	ErrCVLimit = problem.New(problem.Conflict, "students.cv_limit", "At most 3 CVs")

	// ErrAudienceTooSmall: the audience is below k = 10 (04 §V.2): no number
	// is returned and nobody is streamed. It is a cross-module sentinel,
	// not an HTTP problem: outreach answers it with its own 422
	// outreach.audience_too_small (04 §1.10; 06 H-52: one code, one module).
	ErrAudienceTooSmall = errors.New("students: audience below the k-anonymity threshold")
	// ErrInvalidAudience: an audience spec with no key or a malformed key
	// (422 validation_failed if it ever reaches HTTP).
	ErrInvalidAudience = problem.ErrValidation.Wrap(errors.New("students: invalid audience spec"))

	// ErrVerificationRequired: an action for verified students by an
	// account without an active student verification (03 §2.3, k-uv). The
	// code is registered once, by orgs (06 H-52, T-207).
	ErrVerificationRequired = orgs.ErrVerificationRequired

	// ErrInvalidState: resuming a level 3 grant that is not suspended (an
	// active or a revoked one; BE-088).
	ErrInvalidState = problem.New(problem.Conflict, "students.invalid_state", "The grant's state does not allow this change")
	// ErrResumeExpired: resuming a suspended level 3 grant after its
	// suspension window (BE-088, T-331/T-332): only a new contact request of
	// the company, accepted, gives access again.
	ErrResumeExpired = problem.New(problem.Conflict, "students.resume_window_expired",
		"The grant can no longer be resumed")

	// ErrNotFound: the record does not exist or is not the caller's.
	ErrNotFound = problem.ErrNotFound

	// errUnavailable: a dependency is not wired (503; fail closed).
	errUnavailable = problem.ErrUnavailable
)
