// Action constants transcribed from docs/design/03-yetki-matrisi.md by BE-004 (BE-001 R5).
// Module tickets never edit this file: a new action is added to 03 first (orchestrator),
// then here. TestActions_CountMatchesDesign and TestActions_MatchDesignDoc pin the content.

package authz

// HTTP actions of the identity module (03 §5.1), in table order.
const (
	IdentityAccountRegister               Action = "identity.account.register"
	IdentitySessionCreate                 Action = "identity.session.create"
	IdentitySessionDeleteCurrent          Action = "identity.session.delete_current"
	IdentitySessionListOwn                Action = "identity.session.list_own"
	IdentitySessionRevokeOwn              Action = "identity.session.revoke_own"
	IdentityEmailVerify                   Action = "identity.email.verify"
	IdentityEmailResendVerification       Action = "identity.email.resend_verification"
	IdentityPasswordResetRequest          Action = "identity.password.reset_request"
	IdentityPasswordReset                 Action = "identity.password.reset"
	IdentityPasswordChange                Action = "identity.password.change"
	IdentityEmailChangeRequest            Action = "identity.email_change.request"
	IdentityEmailChangeConfirm            Action = "identity.email_change.confirm"
	IdentityAccountReadOwn                Action = "identity.account.read_own"
	IdentityAccountUpdateOwn              Action = "identity.account.update_own"
	IdentityMfaEnroll                     Action = "identity.mfa.enroll"
	IdentityMfaConfirm                    Action = "identity.mfa.confirm"
	IdentityMfaVerify                     Action = "identity.mfa.verify"
	IdentityMfaDisable                    Action = "identity.mfa.disable"
	IdentityMfaRecoveryRegenerate         Action = "identity.mfa_recovery.regenerate"
	IdentityEduVerificationStart          Action = "identity.edu_verification.start"
	IdentityEduVerificationConfirm        Action = "identity.edu_verification.confirm"
	IdentityEduVerificationSubmitDocument Action = "identity.edu_verification.submit_document"
	IdentityEduVerificationReadOwn        Action = "identity.edu_verification.read_own"
	IdentityEduVerificationList           Action = "identity.edu_verification.list"
	IdentityEduVerificationReview         Action = "identity.edu_verification.review"
	IdentityUserSearch                    Action = "identity.user.search"
	IdentityUserRead                      Action = "identity.user.read"
	IdentityUserSuspend                   Action = "identity.user.suspend"
	IdentityUserRevokeSessions            Action = "identity.user.revoke_sessions"
	IdentityUserResetMfa                  Action = "identity.user.reset_mfa"
	IdentityPlatformRoleList              Action = "identity.platform_role.list"
	IdentityPlatformRoleGrant             Action = "identity.platform_role.grant"
	IdentityPlatformRoleRevoke            Action = "identity.platform_role.revoke"
)

// System-only actions of the identity module (03 §3.7): only SystemActor may perform them.
const (
	IdentityEduVerificationExpire Action = "identity.edu_verification.expire"
)

// identityActions lists the HTTP actions of identity in 03 §5.1 order.
var identityActions = []Action{
	IdentityAccountRegister,
	IdentitySessionCreate,
	IdentitySessionDeleteCurrent,
	IdentitySessionListOwn,
	IdentitySessionRevokeOwn,
	IdentityEmailVerify,
	IdentityEmailResendVerification,
	IdentityPasswordResetRequest,
	IdentityPasswordReset,
	IdentityPasswordChange,
	IdentityEmailChangeRequest,
	IdentityEmailChangeConfirm,
	IdentityAccountReadOwn,
	IdentityAccountUpdateOwn,
	IdentityMfaEnroll,
	IdentityMfaConfirm,
	IdentityMfaVerify,
	IdentityMfaDisable,
	IdentityMfaRecoveryRegenerate,
	IdentityEduVerificationStart,
	IdentityEduVerificationConfirm,
	IdentityEduVerificationSubmitDocument,
	IdentityEduVerificationReadOwn,
	IdentityEduVerificationList,
	IdentityEduVerificationReview,
	IdentityUserSearch,
	IdentityUserRead,
	IdentityUserSuspend,
	IdentityUserRevokeSessions,
	IdentityUserResetMfa,
	IdentityPlatformRoleList,
	IdentityPlatformRoleGrant,
	IdentityPlatformRoleRevoke,
}
