package orgs

import "koro/internal/platform/problem"

// Errors of the orgs module (05 §4.2; 04 §1.10, §2.2). Other modules that
// act on behalf of an org read its status with OrgDirectory.Get and return
// ErrOrgSuspended / ErrOrgNotVerified themselves (03 §2.3).
var (
	// ErrOrgSuspended: a write on behalf of a suspended org (03 §2.3); reads
	// stay allowed.
	ErrOrgSuspended = problem.New(problem.Forbidden, "orgs.org_suspended", "Organization is suspended")
	// ErrOrgNotVerified: an action a pending_review company may not take
	// yet (posts, discovery, contact requests; 03 §2.3).
	ErrOrgNotVerified = problem.New(problem.Forbidden, "orgs.org_not_verified", "Organization is not verified yet")
	// ErrInvalidState: the org's status or kind does not allow the change
	// (verifying an active org, closing a closed one, suspending the
	// platform org, transferring a club).
	ErrInvalidState = problem.New(problem.Conflict, "orgs.invalid_state", "Organization status does not allow this change")
	// ErrSlugTaken: the requested slug belongs to another org.
	ErrSlugTaken = problem.New(problem.Conflict, "orgs.slug_taken", "Slug is already in use")
	// ErrOpenCommerce: closing an org that still has open orders or payouts
	// (hooks.openCommerce; 04 §2.2 closeOrg).
	ErrOpenCommerce = problem.New(problem.Conflict, "orgs.open_commerce", "Organization has open orders or payouts")
	// ErrTransferTarget: the new owner is not an active company_admin of
	// the company (04 §2.2 transferOrgOwnership).
	ErrTransferTarget = problem.New(problem.Invalid, "orgs.transfer_target_invalid", "New owner must be an active company admin")
	// ErrNoOwner: the company has no active company_owner row (data repair
	// needed; a platform admin restores it).
	ErrNoOwner = problem.New(problem.Conflict, "orgs.no_owner", "Company has no active owner")
	// ErrEmptyPatch: updateOrg without a field to change.
	ErrEmptyPatch = problem.New(problem.Invalid, "orgs.empty_patch", "Nothing to update")
	// ErrCompanyFieldOnly: a company field sent for an org that is not a
	// company.
	ErrCompanyFieldOnly = problem.New(problem.Invalid, "orgs.company_field", "Company fields apply to companies only")

	// ErrNotFound: the org does not exist or is not visible to the caller.
	ErrNotFound = problem.ErrNotFound
	// ErrPreconditionRequired / ErrPreconditionFailed: If-Match missing or
	// stale on updateOrg (04 §1.13).
	ErrPreconditionRequired = problem.ErrPreconditionRequired
	ErrPreconditionFailed   = problem.ErrPreconditionFailed
)
