package messaging

import (
	"context"
	"errors"
	"time"
	"uuid"
)

// errMissingScope: a ConversationQuery names neither or both scopes (a
// programming error; never a client's).
var errMissingScope = errors.New("messaging: a conversation query needs exactly one scope (org or student)")

// conversationStore is the conversations part of Store (BE-162; 02 §14.3–14.5
// doc patch): conversations, their messages and the participants' states.
type conversationStore interface {
	// InsertConversation writes a new conversation (one per contact
	// request: conversations_contact_request_uq, a 409).
	InsertConversation(ctx context.Context, c Conversation) (Conversation, error)
	// ConversationOfOrg returns the conversation id of the company orgID or
	// ErrNotFound.
	ConversationOfOrg(ctx context.Context, orgID, id uuid.UUID) (Conversation, error)
	// ConversationOfStudent returns the conversation id of the student or
	// ErrNotFound.
	ConversationOfStudent(ctx context.Context, studentID, id uuid.UUID) (Conversation, error)
	// LockConversation reads and row-locks the conversation id (a message
	// or a close follows).
	LockConversation(ctx context.Context, id uuid.UUID) (Conversation, error)
	// ConversationRows returns the conversations of q's scope as q.Viewer
	// sees them, last activity first.
	ConversationRows(ctx context.Context, q ConversationQuery) ([]ConversationRow, error)
	// OpenConversationsOf row-locks the open conversations of the company
	// with the student (a block closes them).
	OpenConversationsOf(ctx context.Context, orgID, studentID uuid.UUID) ([]Conversation, error)
	// CloseConversation moves an open conversation to closed by side at at
	// (last_activity_at with it); ok=false when it is no longer open.
	CloseConversation(ctx context.Context, id uuid.UUID, by Side, at time.Time) (Conversation, bool, error)

	// InsertMessage writes a message and moves its conversation: the last
	// activity to the message's time, company_unanswered up by one for a
	// company message, back to 0 for the student's.
	InsertMessage(ctx context.Context, m Message) (Message, error)
	// MessageIn returns the message id of the conversation or ErrNotFound.
	MessageIn(ctx context.Context, conversationID, id uuid.UUID) (Message, error)
	// Messages returns a page of the conversation's messages, newest first,
	// older than before when set.
	Messages(ctx context.Context, conversationID uuid.UUID, before *uuid.UUID, limit int) ([]Message, error)
	// CompanySenders returns the company members who wrote in the
	// conversation.
	CompanySenders(ctx context.Context, conversationID uuid.UUID) ([]uuid.UUID, error)

	// State returns the user's state in the conversation; found=false
	// without one.
	State(ctx context.Context, conversationID, userID uuid.UUID) (UserState, bool, error)
	// MarkRead moves the user's read marker forward to messageID (never
	// back); moved=false when it was already there or further.
	MarkRead(ctx context.Context, conversationID, userID, messageID uuid.UUID, at time.Time) (moved bool, err error)
	// SetMuted mutes (at) or unmutes the conversation for the user;
	// changed=false when it already was.
	SetMuted(ctx context.Context, conversationID, userID uuid.UUID, muted bool, at time.Time) (changed bool, err error)

	// StudentConversations returns every conversation of the student (KVKK
	// export).
	StudentConversations(ctx context.Context, studentID uuid.UUID) ([]Conversation, error)
	// AllMessages returns every message of the conversation, oldest first
	// (KVKK export).
	AllMessages(ctx context.Context, conversationID uuid.UUID) ([]Message, error)
	// MessagesSentBy returns every message the user wrote, oldest first
	// (KVKK export; a company member's are kept on erasure).
	MessagesSentBy(ctx context.Context, userID uuid.UUID) ([]Message, error)
	// UserStates returns every state of the user (KVKK export).
	UserStates(ctx context.Context, userID uuid.UUID) ([]UserState, error)
	// DeleteStudentConversations hard deletes the student's conversations
	// with their messages and states (account erasure).
	DeleteStudentConversations(ctx context.Context, studentID uuid.UUID) (int, error)
	// DeleteUserStates hard deletes the user's states (account erasure).
	DeleteUserStates(ctx context.Context, userID uuid.UUID) (int, error)

	// CountInactiveBefore counts the conversations whose last activity is
	// before cutoff (conversation_closed, dry run).
	CountInactiveBefore(ctx context.Context, cutoff time.Time) (int, error)
	// PurgeInactive hard deletes up to limit of them (messages and states
	// with them), oldest first.
	PurgeInactive(ctx context.Context, cutoff time.Time, limit int) (int, error)
}
