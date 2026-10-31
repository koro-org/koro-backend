package messaging

import (
	"context"
	"net/http"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
)

// sortConversationNewest is the cursor sort key of the conversation lists
// (the messages use sortNewest, "-id").
const sortConversationNewest = "-last_activity_at"

// conversationSurface is one path of the conversations (BE-162): the
// company's (/orgs/{org_id}/conversations…) or the student's
// (/me/conversations…). The two share every use case through Party.
type conversationSurface struct {
	prefix  string     // the path before /conversations
	opName  string     // "Org" or "My" in the operationIds
	surface op.Surface // org or self
	list    authz.Action
	whose   string // for the summaries
}

var (
	orgConversations = conversationSurface{prefix: "/orgs/{org_id}", opName: "Org", surface: op.SurfaceOrg,
		list: authz.MessagingConversationListOrg, whose: "the company's"}
	myConversations = conversationSurface{prefix: "/me", opName: "My", surface: op.SurfaceSelf,
		list: authz.MessagingConversationListOwn, whose: "my"}
)

// registerConversations adds the 16 conversation operations (8 per path).
func registerConversations(api huma.API, h handler) {
	o, m := orgConversations, myConversations
	listOp := func(s conversationSurface) huma.Operation {
		return huma.Operation{OperationID: "list" + s.opName + "Conversations", Method: http.MethodGet,
			Path: s.prefix + "/conversations", Summary: "List " + s.whose + " conversations",
			Description: "The conversations of accepted contact requests, last activity first, with the caller's unread " +
				"count and the newest message."}
	}
	op.Register(api, "messaging", listOp(o), op.Meta{Action: string(o.list), Surface: o.surface}, h.listOrgConversations)
	op.Register(api, "messaging", listOp(m), op.Meta{Action: string(m.list), Surface: m.surface}, h.listMyConversations)

	getOp := func(s conversationSurface) huma.Operation {
		return huma.Operation{OperationID: "get" + s.opName + "Conversation", Method: http.MethodGet,
			Path: s.prefix + "/conversations/{conversation_id}", Summary: "Get a conversation"}
	}
	read := string(authz.MessagingConversationRead)
	op.Register(api, "messaging", getOp(o), op.Meta{Action: read, Surface: o.surface}, getConversation[orgConversationInput](h))
	op.Register(api, "messaging", getOp(m), op.Meta{Action: read, Surface: m.surface}, getConversation[myConversationInput](h))

	messagesOp := func(s conversationSurface) huma.Operation {
		return huma.Operation{OperationID: "list" + s.opName + "ConversationMessages", Method: http.MethodGet,
			Path: s.prefix + "/conversations/{conversation_id}/messages", Summary: "List the messages of a conversation",
			Description: "Newest first; next_cursor pages back to older messages."}
	}
	op.Register(api, "messaging", messagesOp(o), op.Meta{Action: read, Surface: o.surface}, listMessages[orgMessagesInput](h))
	op.Register(api, "messaging", messagesOp(m), op.Meta{Action: read, Surface: m.surface}, listMessages[myMessagesInput](h))

	sendOp := func(s conversationSurface) huma.Operation {
		return huma.Operation{OperationID: "send" + s.opName + "ConversationMessage", Method: http.MethodPost,
			Path: s.prefix + "/conversations/{conversation_id}/messages", DefaultStatus: http.StatusCreated,
			Summary: "Send a message",
			Description: "Text only, 1–2000 characters (line breaks kept; control and bidi characters refused). A closed " +
				"conversation, or one whose company is closed, is 409 messaging.conversation_closed; a suspended company " +
				"is 403 orgs.org_suspended. The company may send 5 messages in a row before the student answers (409 " +
				"messaging.awaiting_reply). The other side is notified in the app and by push (never the text: at " +
				"most one notice per unread run, none while muted). Sending marks the conversation read for the sender."}
	}
	send := op.Meta{Action: string(authz.MessagingMessageSend), RateLimit: op.RateMessageSend, Idempotency: op.IdempotencyOptional}
	so, sm := send, send
	so.Surface, sm.Surface = o.surface, m.surface
	op.Register(api, "messaging", sendOp(o), so, sendMessage[orgSendInput](h))
	op.Register(api, "messaging", sendOp(m), sm, sendMessage[mySendInput](h))

	readOp := func(s conversationSurface) huma.Operation {
		return huma.Operation{OperationID: "mark" + s.opName + "ConversationRead", Method: http.MethodPost,
			Path: s.prefix + "/conversations/{conversation_id}/read", DefaultStatus: http.StatusNoContent,
			Summary: "Mark a conversation read",
			Description: "Moves the caller's read marker to message_id (a message of this conversation; 422 otherwise) or, " +
				"without it, to the newest message; it never moves back. The other side is not told."}
	}
	mark := string(authz.MessagingConversationMarkRead)
	op.Register(api, "messaging", readOp(o), op.Meta{Action: mark, Surface: o.surface, Idempotency: op.IdempotencyOptional},
		markRead[orgMarkReadInput](h))
	op.Register(api, "messaging", readOp(m), op.Meta{Action: mark, Surface: m.surface, Idempotency: op.IdempotencyOptional},
		markRead[myMarkReadInput](h))

	closeOp := func(s conversationSurface) huma.Operation {
		return huma.Operation{OperationID: "close" + s.opName + "Conversation", Method: http.MethodPost,
			Path: s.prefix + "/conversations/{conversation_id}/close", Summary: "Close a conversation",
			Description: "For good: nobody can write in it any more (a new accepted contact request starts a new " +
				"conversation). Both sides keep reading it until its retention ends. Closing a closed conversation changes nothing."}
	}
	cl := string(authz.MessagingConversationClose)
	op.Register(api, "messaging", closeOp(o), op.Meta{Action: cl, Surface: o.surface, Idempotency: op.IdempotencyOptional},
		closeConversation[orgConversationInput](h))
	op.Register(api, "messaging", closeOp(m), op.Meta{Action: cl, Surface: m.surface, Idempotency: op.IdempotencyOptional},
		closeConversation[myConversationInput](h))

	muteOp := func(s conversationSurface, method, verb string) huma.Operation {
		return huma.Operation{OperationID: verb + s.opName + "Conversation", Method: method,
			Path: s.prefix + "/conversations/{conversation_id}/mute", DefaultStatus: http.StatusNoContent,
			Summary: "Mute a conversation's notices", Description: "For the caller only; idempotent."}
	}
	mu := string(authz.MessagingConversationMute)
	unmute := func(s conversationSurface) huma.Operation {
		u := muteOp(s, http.MethodDelete, "unmute")
		u.Summary = "Unmute a conversation's notices"
		return u
	}
	op.Register(api, "messaging", muteOp(o, http.MethodPut, "mute"), op.Meta{Action: mu, Surface: o.surface},
		setMuted[orgConversationInput](h, true))
	op.Register(api, "messaging", muteOp(m, http.MethodPut, "mute"), op.Meta{Action: mu, Surface: m.surface},
		setMuted[myConversationInput](h, true))
	op.Register(api, "messaging", unmute(o), op.Meta{Action: mu, Surface: o.surface}, setMuted[orgConversationInput](h, false))
	op.Register(api, "messaging", unmute(m), op.Meta{Action: mu, Surface: m.surface}, setMuted[myConversationInput](h, false))
}

// ---- paths

// OrgConversationPath is the org_id and conversation_id path parameters.
type OrgConversationPath struct {
	OrgID          string `path:"org_id" doc:"Company org ID (an unknown or malformed ID is 404)"`
	ConversationID string `path:"conversation_id" doc:"Conversation ID (an unknown, malformed or other org's ID is 404)"`
}

func (p OrgConversationPath) target() (Party, uuid.UUID) {
	return CompanyParty(httpx.ParseID(p.OrgID)), httpx.ParseID(p.ConversationID)
}

// MyConversationPath is the conversation_id path parameter of /me.
type MyConversationPath struct {
	ConversationID string `path:"conversation_id" doc:"Conversation ID (an unknown, malformed or another user's ID is 404)"`
}

func (p MyConversationPath) target() (Party, uuid.UUID) {
	return StudentParty(), httpx.ParseID(p.ConversationID)
}

// targeted is an input naming a conversation on one of the two paths.
type targeted interface {
	target() (Party, uuid.UUID)
}

type orgConversationInput struct{ OrgConversationPath }

type myConversationInput struct{ MyConversationPath }

type conversationOutput struct{ Body ConversationDTO }

// ---- list{Org,My}Conversations

type conversationListOutput struct{ Body ConversationList }

type listOrgConversationsInput struct {
	OrgPath
	Status string `query:"status" enum:"open,closed"`
	page.Params
}

type listMyConversationsInput struct {
	Status string `query:"status" enum:"open,closed"`
	page.Params
}

func (h handler) listOrgConversations(ctx context.Context, in *listOrgConversationsInput) (*conversationListOutput, error) {
	return h.listConversations(ctx, CompanyParty(httpx.ParseID(in.OrgID)), in.Status, in.Params)
}

func (h handler) listMyConversations(ctx context.Context, in *listMyConversationsInput) (*conversationListOutput, error) {
	return h.listConversations(ctx, StudentParty(), in.Status, in.Params)
}

func (h handler) listConversations(ctx context.Context, p Party, status string, pp page.Params) (*conversationListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	scope := a.UserID.String()
	if p.Side == SideCompany {
		scope = p.OrgID.String() + ":" + scope // the per-viewer fields differ between colleagues
	}
	fp := page.Fingerprint("list"+string(p.Side)+"Conversations", scope, status)
	var after *ConversationKey
	c, err := page.Decode(pp.Cursor, sortConversationNewest, fp, 2)
	if err != nil {
		return nil, err
	}
	if c != nil {
		at, err := c.Time(0)
		if err != nil {
			return nil, err
		}
		id, err := c.UUID(1)
		if err != nil {
			return nil, err
		}
		after = &ConversationKey{At: at, ID: id}
	}
	res, err := h.svc.ListConversations(ctx, a, p, ConversationListQuery{Status: ConversationStatus(status), After: after, Limit: pp.Limit})
	if err != nil {
		return nil, err
	}
	out := &conversationListOutput{Body: ConversationList{Items: make([]ConversationDTO, len(res.Items))}}
	for i, v := range res.Items {
		out.Body.Items[i] = ToConversationDTO(v, a.UserID.String())
	}
	if res.Next != nil {
		out.Body.NextCursor = page.Next(sortConversationNewest, fp, page.TimeVal(res.Next.At), page.UUIDVal(res.Next.ID))
	}
	return out, nil
}

// ---- get{Org,My}Conversation, close{Org,My}Conversation, (un)mute{Org,My}Conversation

func getConversation[I targeted](h handler) func(context.Context, *I) (*conversationOutput, error) {
	return func(ctx context.Context, in *I) (*conversationOutput, error) {
		a, err := authz.ActorFrom(ctx)
		if err != nil {
			return nil, err
		}
		p, id := (*in).target()
		v, err := h.svc.GetConversation(ctx, a, p, id)
		if err != nil {
			return nil, err
		}
		return &conversationOutput{Body: ToConversationDTO(v, a.UserID.String())}, nil
	}
}

func closeConversation[I targeted](h handler) func(context.Context, *I) (*conversationOutput, error) {
	return func(ctx context.Context, in *I) (*conversationOutput, error) {
		a, err := authz.ActorFrom(ctx)
		if err != nil {
			return nil, err
		}
		p, id := (*in).target()
		v, err := h.svc.CloseConversation(ctx, a, p, id)
		if err != nil {
			return nil, err
		}
		return &conversationOutput{Body: ToConversationDTO(v, a.UserID.String())}, nil
	}
}

func setMuted[I targeted](h handler, muted bool) func(context.Context, *I) (*struct{}, error) {
	return func(ctx context.Context, in *I) (*struct{}, error) {
		a, err := authz.ActorFrom(ctx)
		if err != nil {
			return nil, err
		}
		p, id := (*in).target()
		return nil, h.svc.SetMuted(ctx, a, p, id, muted)
	}
}

// ---- list{Org,My}ConversationMessages

type messageListOutput struct{ Body MessageList }

type orgMessagesInput struct {
	OrgConversationPath
	page.Params
}

func (in orgMessagesInput) params() page.Params { return in.Params }

type myMessagesInput struct {
	MyConversationPath
	page.Params
}

func (in myMessagesInput) params() page.Params { return in.Params }

func listMessages[I interface {
	targeted
	params() page.Params
}](h handler) func(context.Context, *I) (*messageListOutput, error) {
	return func(ctx context.Context, in *I) (*messageListOutput, error) {
		a, err := authz.ActorFrom(ctx)
		if err != nil {
			return nil, err
		}
		p, id := (*in).target()
		pp := (*in).params()
		fp := page.Fingerprint("listConversationMessages", id.String())
		before, err := afterID(pp.Cursor, fp)
		if err != nil {
			return nil, err
		}
		res, err := h.svc.ListMessages(ctx, a, p, id, before, pp.Limit)
		if err != nil {
			return nil, err
		}
		out := &messageListOutput{Body: MessageList{Items: make([]MessageDTO, len(res.Items))}}
		for i, m := range res.Items {
			out.Body.Items[i] = ToMessageDTO(m, p.Side, a.UserID.String())
		}
		if res.Next != nil {
			out.Body.NextCursor = page.Next(sortNewest, fp, page.UUIDVal(*res.Next))
		}
		return out, nil
	}
}

// ---- send{Org,My}ConversationMessage

// SendMessageBody is the request of send{Org,My}ConversationMessage.
type SendMessageBody struct {
	Text string `json:"text" minLength:"1" maxLength:"2000" doc:"The message; shown to the conversation's parties only, never in a notification"`
}

type messageOutput struct{ Body MessageDTO }

type orgSendInput struct {
	idempotency.OptionalHeader
	OrgConversationPath
	Body SendMessageBody
}

func (in orgSendInput) text() string { return in.Body.Text }

type mySendInput struct {
	idempotency.OptionalHeader
	MyConversationPath
	Body SendMessageBody
}

func (in mySendInput) text() string { return in.Body.Text }

func sendMessage[I interface {
	targeted
	text() string
}](h handler) func(context.Context, *I) (*messageOutput, error) {
	return func(ctx context.Context, in *I) (*messageOutput, error) {
		a, err := authz.ActorFrom(ctx)
		if err != nil {
			return nil, err
		}
		p, id := (*in).target()
		m, err := h.svc.SendMessage(ctx, a, p, id, (*in).text())
		if err != nil {
			return nil, err
		}
		return &messageOutput{Body: ToMessageDTO(m, p.Side, a.UserID.String())}, nil
	}
}

// ---- mark{Org,My}ConversationRead

// MarkReadBody is the optional request of mark{Org,My}ConversationRead.
type MarkReadBody struct {
	MessageID string `json:"message_id,omitempty" format:"uuid" doc:"The newest message the caller saw (default: the newest message)"`
}

type orgMarkReadInput struct {
	idempotency.OptionalHeader
	OrgConversationPath
	Body *MarkReadBody
}

func (in orgMarkReadInput) upTo() string { return markReadUpTo(in.Body) }

type myMarkReadInput struct {
	idempotency.OptionalHeader
	MyConversationPath
	Body *MarkReadBody
}

func (in myMarkReadInput) upTo() string { return markReadUpTo(in.Body) }

func markReadUpTo(b *MarkReadBody) string {
	if b == nil {
		return ""
	}
	return b.MessageID
}

func markRead[I interface {
	targeted
	upTo() string
}](h handler) func(context.Context, *I) (*struct{}, error) {
	return func(ctx context.Context, in *I) (*struct{}, error) {
		a, err := authz.ActorFrom(ctx)
		if err != nil {
			return nil, err
		}
		p, id := (*in).target()
		var upTo *uuid.UUID
		if raw := (*in).upTo(); raw != "" {
			mid := httpx.ParseID(raw)
			upTo = &mid
		}
		return nil, h.svc.MarkRead(ctx, a, p, id, upTo)
	}
}
