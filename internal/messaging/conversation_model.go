package messaging

import (
	"time"
	"uuid"
)

// ConversationStatus is conversations.status (BE-162; 02 §14.3 doc patch).
type ConversationStatus string

// Conversation statuses. A closed conversation takes no message and never
// opens again: a new contact request, once accepted, starts a new one.
const (
	ConversationOpen   ConversationStatus = "open"
	ConversationClosed ConversationStatus = "closed"
)

// Valid reports whether s is a status of the CHECK.
func (s ConversationStatus) Valid() bool { return s == ConversationOpen || s == ConversationClosed }

// Side is a party of a conversation: the student or the company (any of
// its recruiters and admins).
type Side string

// Sides (conversation_messages.sender_side, conversations.closed_by).
const (
	SideStudent Side = "student"
	SideCompany Side = "company"
)

// Conversation is a conversations row (BE-162): the in-platform conversation
// that starts when the student accepts a company's contact request. OrgID,
// StudentUserID and CompanyUserID (the member who sent the request) come
// from the request (06 H-38); ContactRequestID is nil once the request's
// own retention (contact_request_closed) deleted it. LastActivityAt is the
// last message or the closing moment (the list order, the retention
// anchor). created_at and updated_at come from the column defaults and the
// touch trigger.
type Conversation struct {
	ID                uuid.UUID          `gorm:"type:uuid;column:id;primaryKey"`
	ContactRequestID  *uuid.UUID         `gorm:"type:uuid;column:contact_request_id"`
	OrgID             uuid.UUID          `gorm:"type:uuid;column:org_id"`
	StudentUserID     uuid.UUID          `gorm:"type:uuid;column:student_user_id"`
	CompanyUserID     uuid.UUID          `gorm:"type:uuid;column:company_user_id"`
	Status            ConversationStatus `gorm:"column:status"`
	ClosedBy          *Side              `gorm:"column:closed_by"`
	ClosedAt          *time.Time         `gorm:"column:closed_at"`
	CompanyUnanswered int                `gorm:"column:company_unanswered"`
	LastActivityAt    time.Time          `gorm:"column:last_activity_at"`
	CreatedAt         time.Time          `gorm:"column:created_at;->"`
	UpdatedAt         time.Time          `gorm:"column:updated_at;->"`
}

// TableName implements gorm's Tabler.
func (Conversation) TableName() string { return "conversations" }

// Message is a conversation_messages row: immutable once written.
type Message struct {
	ID             uuid.UUID `gorm:"type:uuid;column:id;primaryKey"`
	ConversationID uuid.UUID `gorm:"type:uuid;column:conversation_id"`
	SenderSide     Side      `gorm:"column:sender_side"`
	SenderUserID   uuid.UUID `gorm:"type:uuid;column:sender_user_id"`
	Body           string    `gorm:"column:body"`
	CreatedAt      time.Time `gorm:"column:created_at"`
}

// TableName implements gorm's Tabler.
func (Message) TableName() string { return "conversation_messages" }

// UserState is a conversation_user_states row: one participant's read
// marker (the newest message read) and mute.
type UserState struct {
	ConversationID    uuid.UUID  `gorm:"type:uuid;column:conversation_id;primaryKey"`
	UserID            uuid.UUID  `gorm:"type:uuid;column:user_id;primaryKey"`
	LastReadMessageID *uuid.UUID `gorm:"type:uuid;column:last_read_message_id"`
	ReadAt            *time.Time `gorm:"column:read_at"`
	MutedAt           *time.Time `gorm:"column:muted_at"`
	UpdatedAt         time.Time  `gorm:"column:updated_at;->"`
}

// TableName implements gorm's Tabler.
func (UserState) TableName() string { return "conversation_user_states" }

// ConversationKey is the keyset position of a conversation list:
// last_activity_at DESC, id DESC.
type ConversationKey struct {
	At time.Time
	ID uuid.UUID
}

// ConversationQuery selects conversations for one viewer: exactly one of
// OrgID (the company's) and StudentUserID (the student's) scopes them; ID
// narrows to one; Status filters ("" = any); After and Limit page them.
// Viewer and ViewerSide shape the per-viewer fields (unread count, mute,
// read marker): unread counts the other side's messages after the viewer's
// marker.
type ConversationQuery struct {
	OrgID         *uuid.UUID
	StudentUserID *uuid.UUID
	ID            *uuid.UUID
	Viewer        uuid.UUID
	ViewerSide    Side
	Status        ConversationStatus
	After         *ConversationKey
	Limit         int
}

// ConversationRow is a conversation as one viewer sees it in the store:
// the row, the viewer's state and the newest message.
type ConversationRow struct {
	Conversation      Conversation
	Muted             bool
	LastReadMessageID *uuid.UUID
	Unread            int
	Last              *Message
}
