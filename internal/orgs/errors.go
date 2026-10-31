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
	// ErrTaxIDRequired: a company application without a VKN or TCKN and
	// its tax office, or an updateOrg that would clear them (BE-165, Q-45).
	// Its field errors carry the code too (FieldError.code).
	ErrTaxIDRequired = problem.New(problem.Invalid, "orgs.tax_id_required",
		"A tax number (VKN) or a sole trader's national ID (TCKN) and the tax office are required")

	// ErrLastAdmin: the last club_admin of a club or the company_owner would
	// leave, be removed or be demoted (03 k6; 04 §1.10). An org never stays
	// without an admin through the HTTP operations.
	ErrLastAdmin = problem.New(problem.Conflict, "orgs.last_admin", "The organization's last admin cannot leave")
	// ErrClubAdminNeedsAdultStudent: club_admin goes only to a verified
	// student who is 18 or older (03 k5, T-55).
	ErrClubAdminNeedsAdultStudent = problem.New(problem.Invalid, "orgs.club_admin_requires_adult_student",
		"Club admin must be a verified adult student")
	// ErrRoleNotAllowed: the role does not exist for the org's kind, or is
	// company_owner (only orgs.ownership.transfer moves it; 03 k5).
	ErrRoleNotAllowed = problem.New(problem.Invalid, "orgs.role_not_allowed", "Role is not allowed for this organization")
	// ErrRoleUnchanged: changeOrgMemberRole to the member's current role.
	ErrRoleUnchanged = problem.New(problem.Invalid, "orgs.role_unchanged", "Member already has this role")
	// ErrMemberState: the membership's status does not allow the change
	// (approving an active membership, removing a pending request).
	ErrMemberState = problem.New(problem.Conflict, "orgs.member_state", "Membership status does not allow this change")
	// ErrAlreadyMember: the user already has a pending or active membership
	// of the org (org_members_open_uq).
	ErrAlreadyMember = problem.New(problem.Conflict, "orgs.already_member", "Already a member or a request is pending")
	// ErrInvitationExists: an open, unexpired invitation to the same address
	// exists (org_invitations_open_uq).
	ErrInvitationExists = problem.New(problem.Conflict, "orgs.invitation_exists", "An open invitation to this address exists")
	// ErrInvitationExpired: the invitation's 7 days are over (shown only to
	// the invited account itself: the e-mail matched).
	ErrInvitationExpired = problem.New(problem.Conflict, "orgs.invitation_expired", "Invitation has expired")
	// ErrVerificationRequired: an action for verified students (joinClub)
	// by an account without an active student verification (03 §2.3, k-uv).
	// The code is the cross-module one of 04 §1.10; other modules that need
	// it reuse this value (problem codes are registered once; T-207).
	ErrVerificationRequired = problem.New(problem.Forbidden, "students.verification_required",
		"Student verification required")

	// ErrNotFound: the org does not exist or is not visible to the caller.
	ErrNotFound = problem.ErrNotFound
	// ErrPreconditionRequired / ErrPreconditionFailed: If-Match missing or
	// stale on updateOrg (04 §1.13).
	ErrPreconditionRequired = problem.ErrPreconditionRequired
	ErrPreconditionFailed   = problem.ErrPreconditionFailed
)
