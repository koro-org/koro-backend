package identity

import (
	"koro/internal/authz"
	"koro/internal/platform/problem"
)

// Errors of the identity module (05 §4.2; 04 §1.10, §2.1). Field errors
// name the offending request member, never its value: passwords, tokens and
// addresses are never echoed.
var (
	// ErrInvalidCredentials: login failed. Unknown e-mail, wrong password,
	// locked, suspended or erased account all give this one answer, after
	// the same amount of work (05 §14.1, ASVS 6.3.8).
	ErrInvalidCredentials = problem.New(problem.Unauthenticated, "identity.invalid_credentials", "Invalid e-mail or password")
	// ErrEmailNotVerified: a pending_email account with the right password
	// (03 §2.2-3: such an account cannot open a session).
	ErrEmailNotVerified = problem.New(problem.Forbidden, "identity.email_not_verified", "E-mail address not verified")
	// ErrReauthRequired: an R operation without its proof (03 §6.2).
	ErrReauthRequired = problem.New(problem.Forbidden, "identity.reauth_required", "Re-authentication required")
	// ErrReauthFailed: a wrong proof. 403, not 401, so the client keeps
	// its session (03 §6.2).
	ErrReauthFailed = problem.New(problem.Forbidden, "identity.reauth_failed", "Re-authentication failed")
	// ErrInvalidToken: an e-mail token that is unknown, used or expired
	// (one answer for all three).
	ErrInvalidToken = problem.New(problem.Invalid, "identity.invalid_token", "Invalid or expired token")
	// ErrWeakPassword: the password breaks the policy (length, common or
	// breached list, derived from the account; 05 §14.1).
	ErrWeakPassword = problem.New(problem.Invalid, "identity.weak_password", "Password does not meet the policy")
	// ErrPasswordUnchanged: the new password equals the current one.
	ErrPasswordUnchanged = problem.New(problem.Invalid, "identity.password_unchanged", "New password equals the current one")
	// ErrInvalidEmail: not a plain e-mail address.
	ErrInvalidEmail = problem.New(problem.Invalid, "identity.invalid_email", "Invalid e-mail address")
	// ErrBirthDateRequired: a student registration without birth_date (age
	// gate, 02 §5.1).
	ErrBirthDateRequired = problem.New(problem.Invalid, "identity.birth_date_required", "Birth date is required")
	// ErrInvalidBirthDate: a birth date in the future or before 1900.
	ErrInvalidBirthDate = problem.New(problem.Invalid, "identity.invalid_birth_date", "Invalid birth date")
	// ErrInvalidName: a first or last name that is empty, too long or
	// contains control or bidi characters.
	ErrInvalidName = problem.New(problem.Invalid, "identity.invalid_name", "Invalid name")
	// ErrPrivacyNoticeInvalid: privacy_notice_version_id is not the current
	// privacy notice for the persona (02 §4.2: shown at registration).
	ErrPrivacyNoticeInvalid = problem.New(problem.Invalid, "identity.privacy_notice_invalid", "Privacy notice is not the current one")
	// ErrDuplicateConsent: the same purpose twice in one registration.
	ErrDuplicateConsent = problem.New(problem.Invalid, "identity.duplicate_consent", "Consent purpose listed twice")
	// ErrEmailTaken: an e-mail change confirmed for an address that another
	// account took meanwhile (the confirmer proved control of the address).
	ErrEmailTaken = problem.New(problem.Conflict, "identity.email_taken", "E-mail address is already in use")
	// ErrEmptyPatch: updateMe without a field to change.
	ErrEmptyPatch = problem.New(problem.Invalid, "identity.empty_patch", "Nothing to update")
	// ErrMFAUnavailable: an MFA operation runs where the TOTP secret
	// envelope or the challenge store is not wired (the worker). Fail closed.
	ErrMFAUnavailable = problem.New(problem.Unavailable, "identity.mfa_unavailable", "Multi-factor authentication is not available")
	// ErrMFARequiredByRole: the caller holds an MFA role and tries to turn
	// TOTP off (03 §5.1 k1: a state conflict on the own account, not 403).
	ErrMFARequiredByRole = problem.New(problem.Conflict, "identity.mfa_required_by_role", "Multi-factor authentication is required by your role")
	// ErrMFAAlreadyEnabled: enrollTotp on an account whose TOTP is on.
	ErrMFAAlreadyEnabled = problem.New(problem.Conflict, "identity.mfa_already_enabled", "Multi-factor authentication is already enabled")
	// ErrMFANotEnrolled: confirmTotp without an open enrollment.
	ErrMFANotEnrolled = problem.New(problem.Conflict, "identity.mfa_not_enrolled", "No TOTP enrollment to confirm")
	// ErrMFANotEnabled: verifyMfa, disableTotp or regenerateRecoveryCodes
	// on an account without TOTP.
	ErrMFANotEnabled = problem.New(problem.Conflict, "identity.mfa_not_enabled", "Multi-factor authentication is not enabled")
	// ErrInvalidMFACode: a wrong, expired or replayed TOTP code on
	// confirmTotp or verifyMfa (the session is not touched).
	ErrInvalidMFACode = problem.New(problem.Invalid, "identity.invalid_mfa_code", "Invalid or already used code")
	// ErrMFALoginFailed: completeMfaLogin with an unknown, expired or used
	// challenge, or a wrong code. One answer for all (no oracle).
	ErrMFALoginFailed = problem.New(problem.Unauthenticated, "identity.mfa_login_failed", "Multi-factor sign-in failed")
	// ErrRoleAssignmentOpen: the user already has an open (pending, active
	// or revoke_pending) assignment of the role (platform_role_assignments_open_uq).
	ErrRoleAssignmentOpen = problem.New(problem.Conflict, "identity.platform_role_open", "The user already has this role or a pending request for it")
	// ErrRoleInvalidState: approve/revoke on an assignment whose status
	// does not allow it.
	ErrRoleInvalidState = problem.New(problem.Conflict, "identity.invalid_state", "The role assignment is not in a state that allows this")
	// ErrLastPlatformAdmin: revoking would leave no platform_admin in effect
	// (03 §5.1 k3).
	ErrLastPlatformAdmin = problem.New(problem.Conflict, "identity.last_platform_admin", "The last platform admin cannot be revoked")
	// ErrTalentAuditorLimit: talent_auditor is held (or requested) by two
	// users already (03 §1.1, k3: at most 2).
	ErrTalentAuditorLimit = problem.New(problem.Conflict, "identity.talent_auditor_limit", "At most two talent auditors are allowed")
	// ErrUserInvalidState: suspend/unsuspend on an account whose status does
	// not allow it.
	ErrUserInvalidState = problem.New(problem.Conflict, "identity.user_invalid_state", "The account is not in a state that allows this")
	// ErrLastPrivilegedHolder: suspending the account would leave a
	// privileged role (platform_admin, finance_admin, talent_auditor) with
	// no active holder (06 H-47).
	ErrLastPrivilegedHolder = problem.New(problem.Conflict, "identity.last_privileged_holder", "The last active holder of a privileged role cannot be suspended")
	// ErrAccountActionOpen: a second pending four-eyes request of the same
	// action against the same account (privileged_account_actions_open_uq).
	ErrAccountActionOpen = problem.New(problem.Conflict, "identity.account_action_open", "A request for this action is already pending")
	// ErrInvalidReason: a reason outside 5–500 characters of plain text.
	ErrInvalidReason = problem.New(problem.Invalid, "identity.invalid_reason", "Invalid reason")
	// ErrBootstrap: `koro seed bootstrap-admin` refuses (wrong admin count,
	// another admin already exists, unusable account).
	ErrBootstrap = problem.New(problem.Conflict, "identity.bootstrap_refused", "Bootstrap refused")

	// ErrFourEyes: the DB four-eyes CHECKs of platform_role_assignments map
	// to the authz answer (03 §6.1: same problem code in both layers).
	ErrFourEyes = authz.ErrFourEyes
	// ErrForbidden: the DB self-assignment CHECK (03 Y6) maps to 403.
	ErrForbidden = problem.ErrForbidden

	// ErrNotFound: the record does not exist or is not the caller's.
	ErrNotFound = problem.ErrNotFound
	// ErrUnauthenticated: every rejected credential (03 §2.2-2).
	ErrUnauthenticated = problem.ErrUnauthenticated
	// ErrUnavailable: a dependency the operation needs is not wired
	// (password hasher outside the API, token keys).
	ErrUnavailable = problem.ErrUnavailable
)
