package events

import (
	"time"
	"uuid"
)

// RegStatus is event_registrations.status (02 §11.6).
type RegStatus string

// Registration statuses and the state machine of R-003 §2.5.3 (02 §11.6):
//
//	auto:   → confirmed | waitlisted
//	manual: → pending → confirmed | waitlisted | rejected | withdrawn
//	waitlisted → confirmed (FIFO promotion) | expired | withdrawn
//	confirmed  → cancelled | checked_in | no_show
//	checked_in → confirmed (check-in voided; BE-031)
//
// The organiser and the system also cancel pending and waitlisted
// registrations (cancelEventRegistration, event.cancelled; 04 §E.6 "tüm
// aktif kayıtlar cancelled"); the registrant withdraws them.
const (
	RegPending    RegStatus = "pending"
	RegConfirmed  RegStatus = "confirmed"
	RegWaitlisted RegStatus = "waitlisted"
	RegRejected   RegStatus = "rejected"
	RegCancelled  RegStatus = "cancelled"
	RegCheckedIn  RegStatus = "checked_in"
	RegNoShow     RegStatus = "no_show"
	RegExpired    RegStatus = "expired"
	RegWithdrawn  RegStatus = "withdrawn"
)

// RegStatuses lists every status (OpenAPI enums).
func RegStatuses() []RegStatus {
	return []RegStatus{RegPending, RegConfirmed, RegWaitlisted, RegRejected, RegCancelled, RegCheckedIn, RegNoShow,
		RegExpired, RegWithdrawn}
}

// Valid reports whether s is a status of the CHECK constraint.
func (s RegStatus) Valid() bool {
	for _, x := range RegStatuses() {
		if s == x {
			return true
		}
	}
	return false
}

// Active reports whether a registration in s holds the user's one live
// registration of the event (event_registrations_active_uq).
func (s RegStatus) Active() bool {
	switch s {
	case RegCancelled, RegRejected, RegExpired, RegWithdrawn:
		return false
	}
	return true
}

// Counted reports whether a registration in s holds a seat
// (events.confirmed_count = confirmed + checked_in, 02 §11.1).
func (s RegStatus) Counted() bool { return s == RegConfirmed || s == RegCheckedIn }

// CancelledBy is event_registrations.cancelled_by_kind.
type CancelledBy string

// Who ended a registration.
const (
	ByUser   CancelledBy = "user"
	ByOrg    CancelledBy = "org"
	BySystem CancelledBy = "system"
)

// Source is event_registrations.source (X-Koro-Client; admin is unused in v1).
type Source string

// Sources.
const (
	SourceWeb    Source = "web"
	SourceMobile Source = "mobile"
	SourceAdmin  Source = "admin"
)

// Valid reports whether s is a source of the CHECK constraint.
func (s Source) Valid() bool { return s == SourceWeb || s == SourceMobile || s == SourceAdmin }

// Registration is one event_registrations row (02 §11.6): personal data;
// AnswersSensitiveEnc is special-category data, envelope encrypted under
// SensitiveAnswersPurpose. event_id and org_id come from the events row
// (06 H-38). Answers holds the cleaned answers of the form version without
// the warn_special fields and without the consent fields (those are in
// ConsentSnapshot, the record of truth is consents).
type Registration struct {
	ID                  uuid.UUID    `gorm:"type:uuid;column:id;primaryKey"`
	EventID             uuid.UUID    `gorm:"type:uuid;column:event_id"`
	OrgID               uuid.UUID    `gorm:"type:uuid;column:org_id"`
	UserID              uuid.UUID    `gorm:"type:uuid;column:user_id"`
	Status              RegStatus    `gorm:"column:status"`
	FormVersionID       *uuid.UUID   `gorm:"type:uuid;column:form_version_id"`
	Answers             JSON         `gorm:"column:answers;type:jsonb"`
	AnswersSensitiveEnc []byte       `gorm:"column:answers_sensitive_enc"`
	ConsentSnapshot     JSON         `gorm:"column:consent_snapshot;type:jsonb"`
	Source              Source       `gorm:"column:source"`
	WaitlistedAt        *time.Time   `gorm:"column:waitlisted_at"`
	ConfirmedAt         *time.Time   `gorm:"column:confirmed_at"`
	ApprovedBy          *uuid.UUID   `gorm:"type:uuid;column:approved_by"`
	ApprovedAt          *time.Time   `gorm:"column:approved_at"`
	DecisionNote        *string      `gorm:"column:decision_note"`
	CancelledAt         *time.Time   `gorm:"column:cancelled_at"`
	CancelledByKind     *CancelledBy `gorm:"column:cancelled_by_kind"`
	CancelReason        *string      `gorm:"column:cancel_reason"`
	CheckedInAt         *time.Time   `gorm:"column:checked_in_at"`
	AnswersPurgeAt      time.Time    `gorm:"column:answers_purge_at"`
	SensitivePurgeAt    *time.Time   `gorm:"column:sensitive_purge_at"`
	AnswersPurgedAt     *time.Time   `gorm:"column:answers_purged_at"`
	CreatedAt           time.Time    `gorm:"column:created_at"`
	UpdatedAt           time.Time    `gorm:"column:updated_at"`
}

// TableName implements gorm's Tabler.
func (Registration) TableName() string { return "event_registrations" }

// TicketStatus is registration_tickets.status.
type TicketStatus string

// Ticket statuses.
const (
	TicketActive  TicketStatus = "active"
	TicketRevoked TicketStatus = "revoked"
)

// RevokeReason is registration_tickets.revoke_reason.
type RevokeReason string

// Why a ticket was revoked (02 §11.7).
const (
	RevokeReissued              RevokeReason = "reissued"
	RevokeRegistrationCancelled RevokeReason = "registration_cancelled"
	RevokeEventCancelled        RevokeReason = "event_cancelled"
	RevokeAdmin                 RevokeReason = "admin"
)

// Ticket is one registration_tickets row (02 §11.7): only the SHA-256 of
// the derived token is stored (TicketToken re-derives it from the
// registration and the version; the QR and the e-mail are BE-031's).
// event_id comes from the registration row (06 H-38).
type Ticket struct {
	ID             uuid.UUID     `gorm:"type:uuid;column:id;primaryKey"`
	RegistrationID uuid.UUID     `gorm:"type:uuid;column:registration_id"`
	EventID        uuid.UUID     `gorm:"type:uuid;column:event_id"`
	Version        int           `gorm:"column:version"`
	KeyID          string        `gorm:"column:key_id"`
	TokenLookup    []byte        `gorm:"column:token_lookup"`
	ManualCode     string        `gorm:"column:manual_code"`
	Status         TicketStatus  `gorm:"column:status"`
	IssuedAt       time.Time     `gorm:"column:issued_at"`
	RevokedAt      *time.Time    `gorm:"column:revoked_at"`
	RevokeReason   *RevokeReason `gorm:"column:revoke_reason"`
}

// TableName implements gorm's Tabler.
func (Ticket) TableName() string { return "registration_tickets" }

// RegistrationSummary is one registration of a user without its answers
// (students' admin view, T-241; 04 §V.5 registrations[]).
type RegistrationSummary struct {
	RegistrationID uuid.UUID
	EventID        uuid.UUID
	EventTitle     string
	Status         RegStatus
	RegisteredAt   time.Time
}
