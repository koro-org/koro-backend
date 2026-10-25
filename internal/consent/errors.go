package consent

import "koro/internal/platform/problem"

// Errors of the consent module (05 §4.2; 04 §2.5). Field errors name the
// offending request member, never a value.
var (
	// ErrUnknownPurpose: the purpose code is not in consent_purposes.
	ErrUnknownPurpose = problem.New(problem.Invalid, "consent.unknown_purpose", "Unknown consent purpose")
	// ErrNotConsentBased: the purpose rests on another legal basis
	// (contract, legal obligation, legitimate interest); it is not consented
	// to, so consent is never bundled with the terms of service.
	ErrNotConsentBased = problem.New(problem.Invalid, "consent.not_consent_based", "Purpose does not rest on explicit consent")
	// ErrPurposeInactive: granting a purpose that is switched off.
	ErrPurposeInactive = problem.New(problem.Invalid, "consent.purpose_inactive", "Consent purpose is not active")
	// ErrMinorRestricted: 03 k12, a minor grants a restricted purpose.
	ErrMinorRestricted = problem.New(problem.Invalid, "consent.minor_restricted", "Not available to users under 18")
	// ErrChannelNotAllowed: a grant through a channel that cannot show the
	// consent text (email links are withdrawal-only).
	ErrChannelNotAllowed = problem.New(problem.Invalid, "consent.channel_not_allowed", "Consent cannot be granted through this channel")
	// ErrProofRequired: a grant without the shown consent text
	// (notice_version_id and text_sha256).
	ErrProofRequired = problem.New(problem.Invalid, "consent.proof_required", "The shown consent text is required")
	// ErrNoticeMismatch: the notice is not the consent text of this purpose
	// (a privacy notice or the terms of use are never consent proof).
	ErrNoticeMismatch = problem.New(problem.Invalid, "consent.notice_mismatch", "Notice is not the consent text of this purpose")
	// ErrTextMismatch: text_sha256 differs from the notice body digest.
	ErrTextMismatch = problem.New(problem.Invalid, "consent.text_mismatch", "Text digest does not match the notice")
	// ErrNoticeNotCurrent: a grant on a draft or retired consent text.
	ErrNoticeNotCurrent = problem.New(problem.Conflict, "consent.notice_not_current", "Consent text is not the current version")
	// ErrIYSUnavailable: a grant of a commercial purpose (requires_iys)
	// while no İYS adapter is configured (CONSENT_IYS_PROVIDER=none; BE-105,
	// T-364). A consent taken outside İYS is valid only once İYS has it,
	// within three business days (Yönetmelik m.7/11–12); none can be
	// delivered, so none is taken. Withdrawals are always accepted.
	ErrIYSUnavailable = problem.New(problem.Conflict, "consent.iys_unavailable",
		"Commercial consent cannot be collected while İYS is not connected")
	// ErrScopeRequired: a per-registration purpose without scope_id.
	ErrScopeRequired = problem.New(problem.Invalid, "consent.scope_required", "Purpose needs a scope")
	// ErrScopeNotAllowed: scope_id on a purpose without scope.
	ErrScopeNotAllowed = problem.New(problem.Invalid, "consent.scope_not_allowed", "Purpose takes no scope")

	// ErrTermsNotCurrent: an acceptance (registerAccount's
	// terms_notice_version_id, acceptTerms) names something other than the
	// current terms of use of the reader's audience: unknown, a draft, a
	// retired version, another kind of notice or another audience (BE-118,
	// Q-17). The client reads the current one (listCurrentNotices,
	// kind=terms_of_use) and shows it again.
	ErrTermsNotCurrent = problem.New(problem.Invalid, "consent.terms_not_current", "The current terms of use must be accepted")
	// ErrTermsAcceptanceRequired: a signed-in user who has not accepted the
	// terms of use in force for them (a new version, or an account older
	// than the ledger) calls a write operation the terms gate covers
	// (BE-124, Q-17; httpx x-koro-terms-gate: enforced). Reads, the
	// acceptance itself and the KVKK and account security operations stay
	// open. The client shows the current terms (getMe terms) and calls
	// acceptTerms, then repeats the request.
	ErrTermsAcceptanceRequired = problem.New(problem.Forbidden, "consent.terms_acceptance_required",
		"Accept the current terms of use to continue")

	// ErrInvalidNotice: a draft breaks the notice rules (kind/purpose,
	// version format, title, body).
	ErrInvalidNotice = problem.New(problem.Invalid, "consent.invalid_notice", "Notice is invalid")
	// ErrNoticeVersionTaken: the version already exists for this key.
	ErrNoticeVersionTaken = problem.New(problem.Conflict, "consent.notice_version_taken", "Notice version already exists")
	// ErrNoticeState: publish of a published notice, retire of a draft or
	// of a retired notice, or a concurrent publication of the same key.
	ErrNoticeState = problem.New(problem.Conflict, "consent.notice_state", "Notice is not in a state that allows this")
	// ErrNoNotice: no published notice matches (Notices.Current).
	ErrNoNotice = problem.New(problem.NotFound, "consent.no_notice", "No published notice")

	// ErrEmptyPatch: adminUpdateConsentPurpose without a field to change.
	ErrEmptyPatch = problem.New(problem.Invalid, "consent.empty_patch", "Nothing to update")

	// ErrReauthUnavailable: an R operation while no re-authentication
	// verifier is bound (identity, BE-014). Fail closed.
	ErrReauthUnavailable = problem.New(problem.Unavailable, "consent.reauth_unavailable", "Re-authentication is not available")

	// ErrInvalidUnsubscribeLink: an unsubscribe link that is malformed,
	// tampered with, signed by an unknown key or expired (BE-062). Every
	// reason is the same answer: the link reveals nothing about its subject.
	ErrInvalidUnsubscribeLink = problem.New(problem.Invalid, "consent.invalid_unsubscribe_link", "Invalid or expired unsubscribe link")

	// ErrNotFound: the record does not exist (or is not visible).
	ErrNotFound = problem.ErrNotFound
)
