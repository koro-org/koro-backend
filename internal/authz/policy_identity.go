package authz

// identityPolicy is the policy of the identity module (03 §5.1). Only tickets
// of the identity module edit this file (BE-001 R5). Written by BE-014 for
// its 14 actions:
//
//	identity.account.register              P
//	identity.session.create                P
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
//
// Resources (03 §3.5): user (OwnerUserID = the account; the public actions
// use a user resource without an owner: the target must stay unknown) and
// session (OwnerUserID = the session's user).
//
// The remaining identity actions (MFA, student verification, admin user
// and platform role operations) belong to BE-016 and BE-018: until their
// tickets write them, they are visible and granted to no role path, so
// every caller gets 403 (default deny).
func identityPolicy() Policy {
	return Policy{
		Visible: func(a Actor, act Action, r Resource) bool {
			switch act {
			case IdentityAccountRegister, IdentitySessionCreate, IdentityEmailVerify, IdentityEmailResendVerification,
				IdentityPasswordResetRequest, IdentityPasswordReset, IdentityEmailChangeConfirm:
				return r.Type == TypeUser
			case IdentitySessionDeleteCurrent, IdentitySessionListOwn, IdentitySessionRevokeOwn:
				return r.Type == TypeSession && a.Owns(r)
			case IdentityAccountReadOwn, IdentityAccountUpdateOwn, IdentityPasswordChange, IdentityEmailChangeRequest:
				return r.Type == TypeUser && a.Owns(r)
			}
			return true // not written yet: no grant below → 403
		},
		Grants: func(_ Actor, act Action, _ Resource) []Grant {
			switch act {
			case IdentityAccountRegister, IdentitySessionCreate, IdentityEmailVerify, IdentityEmailResendVerification,
				IdentityPasswordResetRequest, IdentityPasswordReset, IdentityEmailChangeConfirm:
				return []Grant{{Role: RolePublic}}
			case IdentitySessionDeleteCurrent, IdentitySessionListOwn, IdentitySessionRevokeOwn,
				IdentityAccountReadOwn, IdentityAccountUpdateOwn, IdentityPasswordChange, IdentityEmailChangeRequest:
				return []Grant{{Role: RoleSelf}}
			}
			return nil
		},
	}
}
