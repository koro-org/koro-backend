package domainevent

import "uuid"

// Topics of the messaging module (01 §6.2). Owner: messaging tickets.
const (
	TopicContactRequestCreated = "contact_request.created"
	TopicContactRequestClosed  = "contact_request.closed"
	// TopicConversationMessageSent: a message in the conversation of an
	// accepted contact request (BE-162).
	TopicConversationMessageSent = "conversation.message_sent"
)

var _ = register(
	spec("messaging", Outbox, ContactRequestCreated{}),
	spec("messaging", Outbox, ContactRequestClosed{}),
	spec("messaging", Outbox, ConversationMessageSent{}),
)

// ContactRequestCreated: the student is notified (the message body never
// enters the notification or the event).
type ContactRequestCreated struct {
	ContactRequestID uuid.UUID `json:"contact_request_id"`
	OrgID            uuid.UUID `json:"org_id"`
	StudentUserID    uuid.UUID `json:"student_user_id"`
}

// Topic implements Payload.
func (ContactRequestCreated) Topic() string { return TopicContactRequestCreated }

// ContactRequestClosed: Outcome is accepted | declined | expired (a status of
// contact_requests; a decline carries no reason; a withdrawal emits nothing).
type ContactRequestClosed struct {
	ContactRequestID uuid.UUID `json:"contact_request_id"`
	OrgID            uuid.UUID `json:"org_id"`
	Outcome          string    `json:"outcome"`
}

// Topic implements Payload.
func (ContactRequestClosed) Topic() string { return TopicContactRequestClosed }

// ConversationMessageSent: the other side of the conversation is notified
// (BE-162). SenderSide is student | company; the message text never enters
// the event or the notification.
type ConversationMessageSent struct {
	ConversationID uuid.UUID `json:"conversation_id"`
	MessageID      uuid.UUID `json:"message_id"`
	OrgID          uuid.UUID `json:"org_id"`
	SenderSide     string    `json:"sender_side"`
}

// Topic implements Payload.
func (ConversationMessageSent) Topic() string { return TopicConversationMessageSent }
