package identity

import "koro/internal/platform/problem"

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
	// ErrMFAUnavailable: an MFA account signs in or re-authenticates while
	// the MFA challenge/TOTP verifier is not wired (BE-016). Fail closed.
	ErrMFAUnavailable = problem.New(problem.Unavailable, "identity.mfa_unavailable", "Multi-factor authentication is not available")
	// ErrBootstrap: `koro seed bootstrap-admin` refuses (wrong admin count,
	// another admin already exists, unusable account).
	ErrBootstrap = problem.New(problem.Conflict, "identity.bootstrap_refused", "Bootstrap refused")

	// ErrNotFound: the record does not exist or is not the caller's.
	ErrNotFound = problem.ErrNotFound
	// ErrUnauthenticated: every rejected credential (03 §2.2-2).
	ErrUnauthenticated = problem.ErrUnauthenticated
	// ErrUnavailable: a dependency the operation needs is not wired
	// (password hasher outside the API, token keys).
	ErrUnavailable = problem.ErrUnavailable
)
