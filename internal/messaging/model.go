package messaging

import (
	"time"
	"uuid"
)

// Status is contact_requests.status (02 §14.1).
type Status string

// Statuses. pending is the only open one; the others are closed and the
// row is hard deleted 12 months after it closed (contact_request_closed).
const (
	StatusPending   Status = "pending"
	StatusAccepted  Status = "accepted"
	StatusDeclined  Status = "declined"
	StatusExpired   Status = "expired"
	StatusWithdrawn Status = "withdrawn"
)

// Statuses lists every status in 02 §14.1 order.
func Statuses() []Status {
	return []Status{StatusPending, StatusAccepted, StatusDeclined, StatusExpired, StatusWithdrawn}
}

// Valid reports whether s is a status of the CHECK.
func (s Status) Valid() bool {
	switch s {
	case StatusPending, StatusAccepted, StatusDeclined, StatusExpired, StatusWithdrawn:
		return true
	}
	return false
}

// Via is contact_requests.via: where the company found the student.
type Via string

// Channels of a request (02 §14.1). opportunity_post names the company's
// opportunity call the request is about; the student is still addressed by
// the company's pseudonym.
const (
	ViaDiscovery       Via = "discovery"
	ViaOpportunityPost Via = "opportunity_post"
)

// Decision is the student's answer (respondMyContactRequest).
type Decision string

// Decisions. A decline carries no reason (04 §2.9: "ret gerekçesiz").
const (
	DecisionAccepted Decision = "accepted"
	DecisionDeclined Decision = "declined"
)

// ContactRequest is a contact_requests row (02 §14.1). org_kind is the
// constant 'company' of the composite FK and is not mapped. created_at and
// updated_at come from the column defaults and the touch trigger (02 §0.1
// [ts]); updated_at of a closed row is the moment it closed (nothing
// updates a closed request), the anchor of contact_request_closed.
type ContactRequest struct {
	ID                 uuid.UUID  `gorm:"type:uuid;column:id;primaryKey"`
	OrgID              uuid.UUID  `gorm:"type:uuid;column:org_id"`
	SenderUserID       uuid.UUID  `gorm:"type:uuid;column:sender_user_id"`
	StudentUserID      uuid.UUID  `gorm:"type:uuid;column:student_user_id"`
	Via                Via        `gorm:"column:via"`
	OpportunityPostID  *uuid.UUID `gorm:"type:uuid;column:opportunity_post_id"`
	Subject            string     `gorm:"column:subject"`
	Message            string     `gorm:"column:message"`
	Status             Status     `gorm:"column:status"`
	RespondedAt        *time.Time `gorm:"column:responded_at"`
	ExpiresAt          time.Time  `gorm:"column:expires_at"`
	EntitlementUsageID uuid.UUID  `gorm:"type:uuid;column:entitlement_usage_id"`
	CreatedAt          time.Time  `gorm:"column:created_at;->"`
	UpdatedAt          time.Time  `gorm:"column:updated_at;->"`
}

// TableName implements gorm's Tabler.
func (ContactRequest) TableName() string { return "contact_requests" }

// Effective is the status as of now: a pending request whose 14 days ran
// out is expired even before messaging.expire_requests (hourly) closes it.
func (r ContactRequest) Effective(now time.Time) Status {
	if r.Status == StatusPending && !now.Before(r.ExpiresAt) {
		return StatusExpired
	}
	return r.Status
}

// Open reports whether the student may still answer (and the company
// withdraw) the request at now.
func (r ContactRequest) Open(now time.Time) bool { return r.Effective(now) == StatusPending }

// Block is a contact_blocks row (02 §14.2): the student refuses every
// contact request of the company. It belongs to the student; the company
// never learns that it exists.
type Block struct {
	StudentUserID uuid.UUID `gorm:"type:uuid;column:student_user_id;primaryKey"`
	CompanyOrgID  uuid.UUID `gorm:"type:uuid;column:company_org_id;primaryKey"`
	CreatedAt     time.Time `gorm:"column:created_at;->"`
}

// TableName implements gorm's Tabler.
func (Block) TableName() string { return "contact_blocks" }

// BlockKey is the keyset position of a block list: created_at DESC,
// company_org_id DESC (the newest block first).
type BlockKey struct {
	CreatedAt time.Time
	OrgID     uuid.UUID
}

// ListQuery narrows a request list (listSentContactRequests,
// listMyContactRequests): Status filters by the effective status at Now
// ("" = any), After is the keyset position (id DESC: uuidv7, newest
// first), Limit the page size.
type ListQuery struct {
	Status Status
	Now    time.Time
	After  *uuid.UUID
	Limit  int
}
