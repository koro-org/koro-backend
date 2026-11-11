package domainevent

import (
	"time"
	"uuid"

	"koro/internal/platform/civil"
)

// Topics of the identity module (01 §6.2). Owner: identity tickets.
const (
	TopicIdentityEmailTokenIssued  = "identity.email_token_issued"
	TopicIdentityNewSignInDetected = "identity.new_sign_in_detected"
	TopicUserEduVerified           = "user.edu_verified"
	TopicUserEduVerificationLapsed = "user.edu_verification_lapsed"
	TopicUserActiveDay             = "user.active_day"
)

var _ = register(
	spec("identity", Outbox, IdentityEmailTokenIssued{}),
	spec("identity", Outbox, IdentityNewSignInDetected{}),
	spec("identity", Outbox, UserEduVerified{}),
	spec("identity", Outbox, UserEduVerificationLapsed{}),
	spec("identity", AfterCommit, UserActiveDay{}),
)

// IdentityEmailTokenIssued: the worker sends the verification or reset
// e-mail; the token itself is re-derived there (DerivedToken("email_token",
// token_id)) and never travels in the event.
type IdentityEmailTokenIssued struct {
	TokenID uuid.UUID `json:"token_id"`
	UserID  uuid.UUID `json:"user_id"`
	Purpose string    `json:"purpose"`
}

// Topic implements Payload.
func (IdentityEmailTokenIssued) Topic() string { return TopicIdentityEmailTokenIssued }

// IdentityNewSignInDetected: a session opened from a device or a network
// the account had not used (BE-209). Written in the sign-in's transaction,
// so no later request of that session can hold the alert back; the worker
// sends it by e-mail, in the app and by push (identity.send_new_sign_in_alert).
type IdentityNewSignInDetected struct {
	UserID    uuid.UUID `json:"user_id"`
	SessionID uuid.UUID `json:"session_id"`
}

// Topic implements Payload.
func (IdentityNewSignInDetected) Topic() string { return TopicIdentityNewSignInDetected }

// UserEduVerified: a student verification was confirmed (or renewed).
type UserEduVerified struct {
	UserID       uuid.UUID `json:"user_id"`
	UniversityID uuid.UUID `json:"university_id"`
	ExpiresAt    time.Time `json:"expires_at"`
	Renewal      bool      `json:"renewal"`
}

// Topic implements Payload.
func (UserEduVerified) Topic() string { return TopicUserEduVerified }

// UserEduVerificationLapsed: a verification expired (identity.edu_expire).
type UserEduVerificationLapsed struct {
	UserID         uuid.UUID `json:"user_id"`
	VerificationID uuid.UUID `json:"verification_id"`
}

// Topic implements Payload.
func (UserEduVerificationLapsed) Topic() string { return TopicUserEduVerificationLapsed }

// UserActiveDay: first activity of a user on a calendar day (Europe/Istanbul).
type UserActiveDay struct {
	UserID uuid.UUID  `json:"user_id"`
	Day    civil.Date `json:"day"`
}

// Topic implements Payload.
func (UserActiveDay) Topic() string { return TopicUserActiveDay }
