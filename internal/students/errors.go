package students

import (
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

	// ErrVerificationRequired: an action for verified students by an
	// account without an active student verification (03 §2.3, k-uv). The
	// code is registered once, by orgs (06 H-52, T-207).
	ErrVerificationRequired = orgs.ErrVerificationRequired

	// ErrNotFound: the record does not exist or is not the caller's.
	ErrNotFound = problem.ErrNotFound

	// errUnavailable: a dependency is not wired (503; fail closed).
	errUnavailable = problem.ErrUnavailable
)
