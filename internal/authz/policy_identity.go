package authz

// identityPolicy is the policy of the identity module (03 §5.1). Only tickets
// of the identity module edit this file (BE-001 R5). Written by BE-014 for
// its 14 actions and extended by BE-016 (MFA, admin user operations,
// platform roles):
//
//	identity.account.register              P
//	identity.session.create                P (password step and completeMfaLogin)
//	identity.email.verify                  P
//	identity.email.resend_verification     P (no enumeration: 202)
//	identity.password.reset_request        P (no enumeration: 202)
//	identity.password.reset                P
//	identity.email_change.confirm          P
//	identity.session.delete_current        O
//	identity.session.list_own              O
//	identity.session.revoke_own            O (another user's session → 404)
//	identity.account.read_own              O
//	identity.account.update_own            O
//	identity.password.change               O · R (re-authentication in the service)
//	identity.email_change.request          O · R
//	identity.mfa.enroll                    O · R (06 H-46: the password; service)
//	identity.mfa.confirm                   O · R (06 H-46)
//	identity.mfa.verify                    O (step-up of the current session)
//	identity.mfa.disable                   O · R; k1 (MFA role → 409 in the service)
//	identity.mfa_recovery.regenerate       O · R
//	identity.user.search                   sup, dpo, pad
//	identity.user.read                     sup, dpo, pad
//	identity.user.suspend                  pad              ┐ 06 H-47: against a privileged
//	identity.user.revoke_sessions          sup, pad         │ role holder the service opens a
//	identity.user.reset_mfa                pad · R; k2      ┘ request; its approval carries
//	                                       Attrs requested_by (four eyes) and only pad may give it
//	                                       (own MFA: Y6 in the core)
//	identity.platform_role.list            dpo, pad
//	identity.platform_role.grant           pad · R; k3 (also adminApprovePlatformRole,
//	                                       the second eye of 06 H-17: requested_by)
//	identity.platform_role.revoke          pad · R; k3 (the second eye of a privileged
//	                                       revocation: requested_by = revoke requester)
//	identity.edu_verification.start       O (BE-018; 202 without enumeration)
//	identity.edu_verification.confirm     P (the token names the account)
//	identity.edu_verification.submit_document O
//	identity.edu_verification.read_own    O
//	identity.edu_verification.list        sup, pad (the document queue: admin_surface)
//	identity.edu_verification.review      sup, pad (download and decision; Y6 in the
//	                                       core: never the reviewer's own document)
//
// MFA of the staff columns (k-mfa) follows from the role paths. The O cells
// of the MFA actions are RoleSelf: an admin without TOTP must be able to
// enroll (03 §6.4-2), so they never require an MFA session themselves.
//
// Resources (03 §3.5): user (OwnerUserID = the account: the caller's own one
// on the O cells, the target on the admin cells; the public actions use a
// user without an owner: the target must stay unknown), session (owner = the
// session's user), admin_surface (search and list: nothing is read before
// the decision) and platform_role_assignment (OwnerUserID = the target user,
// Attrs role and requested_by: the user whose request the actor would
// complete, "" on a first step) and edu_verification (OwnerUserID = the
// applicant, Attrs status; the nil UUID before the row is read). Y6 (own
// role, own MFA reset, own student document) and the four eyes of 06 H-17
// are enforced by the core (absolute.go) from these fields.
func identityPolicy() Policy {
	return Policy{
		Visible: func(a Actor, act Action, r Resource) bool {
			switch act {
			case IdentityAccountRegister, IdentitySessionCreate, IdentityEmailVerify, IdentityEmailResendVerification,
				IdentityPasswordResetRequest, IdentityPasswordReset, IdentityEmailChangeConfirm:
				return r.Type == TypeUser
			case IdentitySessionDeleteCurrent, IdentitySessionListOwn, IdentitySessionRevokeOwn:
				return r.Type == TypeSession && a.Owns(r)
			case IdentityAccountReadOwn, IdentityAccountUpdateOwn, IdentityPasswordChange, IdentityEmailChangeRequest,
				IdentityMfaEnroll, IdentityMfaConfirm, IdentityMfaVerify, IdentityMfaDisable, IdentityMfaRecoveryRegenerate:
				return r.Type == TypeUser && a.Owns(r)
			case IdentityUserSearch, IdentityPlatformRoleList:
				return r.Type == TypeAdminSurface && AdminSurfaceVisible(a)
			case IdentityUserRead, IdentityUserSuspend, IdentityUserRevokeSessions, IdentityUserResetMfa:
				return r.Type == TypeUser && r.OwnerUserID != nil && AdminSurfaceVisible(a)
			case IdentityPlatformRoleGrant, IdentityPlatformRoleRevoke:
				return r.Type == TypePlatformRoleAssignment && r.OwnerUserID != nil && AdminSurfaceVisible(a)
			case IdentityEduVerificationConfirm:
				return r.Type == TypeUser
			case IdentityEduVerificationStart, IdentityEduVerificationSubmitDocument, IdentityEduVerificationReadOwn:
				return r.Type == TypeUser && a.Owns(r)
			case IdentityEduVerificationList:
				return r.Type == TypeAdminSurface && AdminSurfaceVisible(a)
			case IdentityEduVerificationReview:
				return r.Type == TypeEduVerification && r.OwnerUserID != nil && AdminSurfaceVisible(a)
			}
			return true // no grant below → 403
		},
		Grants: func(_ Actor, act Action, r Resource) []Grant {
			sup, dpo, pad := Grant{Role: RoleSupport}, Grant{Role: RoleDPO}, Grant{Role: RolePlatformAdmin}
			switch act {
			case IdentityAccountRegister, IdentitySessionCreate, IdentityEmailVerify, IdentityEmailResendVerification,
				IdentityPasswordResetRequest, IdentityPasswordReset, IdentityEmailChangeConfirm, IdentityEduVerificationConfirm:
				return []Grant{{Role: RolePublic}}
			case IdentitySessionDeleteCurrent, IdentitySessionListOwn, IdentitySessionRevokeOwn,
				IdentityAccountReadOwn, IdentityAccountUpdateOwn, IdentityPasswordChange, IdentityEmailChangeRequest,
				IdentityMfaEnroll, IdentityMfaConfirm, IdentityMfaVerify, IdentityMfaDisable, IdentityMfaRecoveryRegenerate,
				IdentityEduVerificationStart, IdentityEduVerificationSubmitDocument, IdentityEduVerificationReadOwn:
				return []Grant{{Role: RoleSelf}}
			case IdentityEduVerificationList, IdentityEduVerificationReview:
				return []Grant{sup, pad}
			case IdentityUserSearch, IdentityUserRead:
				return []Grant{sup, dpo, pad}
			case IdentityUserSuspend, IdentityUserRevokeSessions, IdentityUserResetMfa:
				return accountActionGrants(act, r)
			case IdentityPlatformRoleList:
				return []Grant{dpo, pad}
			case IdentityPlatformRoleGrant, IdentityPlatformRoleRevoke:
				return []Grant{pad}
			}
			return nil
		},
	}
}

// accountActionGrants are the grants of the admin account actions (03 §5.1)
// with the four eyes of 06 H-47: Attrs requested_by is always passed (""
// on a first step); a non-empty value is the approval of a pending request
// against a privileged role holder, which only a platform_admin other than
// the requester may give.
func accountActionGrants(act Action, r Resource) []Grant {
	eyes := []string{AttrRequestedBy}
	pad := Grant{Role: RolePlatformAdmin, FourEyes: eyes}
	if r.Attr(AttrRequestedBy) != "" {
		return []Grant{pad}
	}
	if act == IdentityUserRevokeSessions {
		return []Grant{{Role: RoleSupport, FourEyes: eyes}, pad}
	}
	return []Grant{pad}
}
