package messaging

import "koro/internal/platform/problem"

// Errors of the messaging module (05 §4.2; 04 §1.10, §2.9). The org status
// rules of 03 §2.3 reuse the orgs codes (orgs.ErrOrgSuspended,
// orgs.ErrOrgNotVerified), the quota entitlements.ErrNoEntitlement and the
// acceptance of a student without an active verification
// students.ErrVerificationRequired: a code is registered once (06 H-52).
var (
	// ErrNotFound: everything a company may not tell apart (03 k19: a
	// pseudonym that resolves to nobody, a student who is not discoverable,
	// a minor, a student who blocked the company or declined it within 90
	// days) and another org's or another student's request (IDOR).
	ErrNotFound = problem.ErrNotFound
	// ErrInvalidState: the request is no longer pending (answered,
	// withdrawn or past its 14 days) and the change needs a pending one.
	ErrInvalidState = problem.New(problem.Conflict, "messaging.invalid_state",
		"The current status does not allow this change")
	// ErrRequestPending: the company already has a pending request to the
	// student (contact_requests_pending_uq); it waits for the answer, is
	// withdrawn or expires first.
	ErrRequestPending = problem.New(problem.Conflict, "messaging.request_pending",
		"A pending contact request to this student exists")
	// ErrConversationClosed: the conversation is closed (by either side,
	// or because the student blocked the company) or its company is gone;
	// it takes no message (BE-162).
	ErrConversationClosed = problem.New(problem.Conflict, "messaging.conversation_closed",
		"The conversation is closed")
	// ErrAwaitingReply: the company sent MaxCompanyUnanswered messages
	// since the student's last one and waits for the reply (BE-162).
	ErrAwaitingReply = problem.New(problem.Conflict, "messaging.awaiting_reply",
		"Wait for the student's reply before sending more messages")
)
