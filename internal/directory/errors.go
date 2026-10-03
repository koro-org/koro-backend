package directory

import "koro/internal/platform/problem"

// Errors of the directory module (05 §4.2; 04 §1.10, §2.3).
var (
	// ErrSensitivityRequired: a religious, political or identity category
	// without the sensitive flag (03 k7; clubs_sensitive_category_chk). Only
	// a platform admin sets the flag (adminSetClubSensitivity).
	ErrSensitivityRequired = problem.New(problem.Invalid, "directory.sensitivity_required",
		"Category requires the sensitive flag")
	// ErrInvalidState: the club's org is closed, or the change does not fit
	// the club's current state (e.g. a sensitivity request for a club that
	// is already sensitive).
	ErrInvalidState = problem.New(problem.Conflict, "directory.invalid_state", "Club state does not allow this change")
	// ErrYokCodeTaken: another university has this YÖK code.
	ErrYokCodeTaken = problem.New(problem.Conflict, "directory.yok_code_taken", "YÖK code is already in use")
	// ErrDomainTaken: the e-mail domain belongs to another university.
	ErrDomainTaken = problem.New(problem.Conflict, "directory.domain_taken", "E-mail domain belongs to another university")
	// ErrDepartmentExists: the university already has a department with
	// this YÖK program code.
	ErrDepartmentExists = problem.New(problem.Conflict, "directory.department_exists", "Department already exists")
	// ErrSkillSlugTaken: another skill has this slug.
	ErrSkillSlugTaken = problem.New(problem.Conflict, "directory.skill_slug_taken", "Skill slug is already in use")
	// ErrUnknownSkill: SkillsExist found an unknown or inactive skill.
	ErrUnknownSkill = problem.New(problem.Invalid, "directory.unknown_skill", "Unknown or inactive skill")
	// ErrEmptyPatch: a PATCH without a field to change.
	ErrEmptyPatch = problem.New(problem.Invalid, "directory.empty_patch", "Nothing to update")

	// ErrNotFound: the record does not exist or is not visible.
	ErrNotFound = problem.ErrNotFound
	// ErrPreconditionRequired / ErrPreconditionFailed: If-Match missing or
	// stale on updateClub (04 §1.13).
	ErrPreconditionRequired = problem.ErrPreconditionRequired
	ErrPreconditionFailed   = problem.ErrPreconditionFailed

	// errUnavailable: a dependency is not wired (503; fail closed).
	errUnavailable = problem.ErrUnavailable
)
