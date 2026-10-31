// Action constants transcribed from docs/design/03-yetki-matrisi.md by BE-004 (BE-001 R5).
// Module tickets never edit this file: a new action is added to 03 first (orchestrator),
// then here. TestActions_CountMatchesDesign and TestActions_MatchDesignDoc pin the content.

package authz

// HTTP actions of the messaging module (03 §5.9), in table order.
const (
	MessagingContactRequestCreate       Action = "messaging.contact_request.create"
	MessagingContactRequestListSent     Action = "messaging.contact_request.list_sent"
	MessagingContactRequestRead         Action = "messaging.contact_request.read"
	MessagingContactRequestWithdraw     Action = "messaging.contact_request.withdraw"
	MessagingContactRequestListReceived Action = "messaging.contact_request.list_received"
	MessagingContactRequestRespond      Action = "messaging.contact_request.respond"
	MessagingBlockManage                Action = "messaging.block.manage"
	MessagingBlockListOwn               Action = "messaging.block.list_own"
	// Conversations of an accepted contact request (BE-162, Q-40; 03 §5.9 doc patch).
	MessagingConversationListOrg  Action = "messaging.conversation.list_org"
	MessagingConversationListOwn  Action = "messaging.conversation.list_own"
	MessagingConversationRead     Action = "messaging.conversation.read"
	MessagingConversationMarkRead Action = "messaging.conversation.mark_read"
	MessagingConversationClose    Action = "messaging.conversation.close"
	MessagingConversationMute     Action = "messaging.conversation.mute"
	MessagingMessageSend          Action = "messaging.message.send"
)

// System-only actions of the messaging module (03 §3.7): only SystemActor may perform them.
const (
	MessagingContactRequestExpire Action = "messaging.contact_request.expire"
)

// messagingActions lists the HTTP actions of messaging in 03 §5.9 order.
var messagingActions = []Action{
	MessagingContactRequestCreate,
	MessagingContactRequestListSent,
	MessagingContactRequestRead,
	MessagingContactRequestWithdraw,
	MessagingContactRequestListReceived,
	MessagingContactRequestRespond,
	MessagingBlockManage,
	MessagingBlockListOwn,
	MessagingConversationListOrg,
	MessagingConversationListOwn,
	MessagingConversationRead,
	MessagingConversationMarkRead,
	MessagingConversationClose,
	MessagingConversationMute,
	MessagingMessageSend,
}
