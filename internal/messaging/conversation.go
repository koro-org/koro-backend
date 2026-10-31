package messaging

import (
	"context"
	"errors"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/orgs"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/outbox"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
)

// ---- conversations (BE-162, Q-40)

// Limits of a conversation.
const (
	// MinMessageText and MaxMessageText bound a message (the char_length
	// CHECK of conversation_messages.body).
	MinMessageText, MaxMessageText = 1, 2000
	// MaxCompanyUnanswered is how many messages the company may send in a
	// row while the student has not answered (an accepted request is not
	// a mailing list: 409 messaging.awaiting_reply after that).
	MaxCompanyUnanswered = 5
)

// Party is the side a caller acts on: the company's path names its org
// (/orgs/{org_id}/conversations…), the student's path is /me.
type Party struct {
	Side  Side
	OrgID uuid.UUID // the company's path only
}

// CompanyParty is the company orgID's path.
func CompanyParty(orgID uuid.UUID) Party { return Party{Side: SideCompany, OrgID: orgID} }

// StudentParty is the student's path (/me).
func StudentParty() Party { return Party{Side: SideStudent} }

// ConversationListQuery narrows listOrgConversations / listMyConversations.
type ConversationListQuery struct {
	Status ConversationStatus // "" = any
	After  *ConversationKey
	Limit  int
}

// ConversationView is a conversation as its viewer sees it: the company's
// card on the student's side, the viewer's unread count, mute and read
// marker, the newest message and whether the viewer may write now.
type ConversationView struct {
	Conversation Conversation
	Side         Side            // the viewer's side
	Company      orgs.OrgSummary // the student's view; zero when the org is gone
	Muted        bool
	Unread       int
	LastRead     *uuid.UUID
	Last         *Message
	CanSend      bool
}

// ConversationPage is a page of a conversation list.
type ConversationPage struct {
	Items []ConversationView
	Next  *ConversationKey
}

// MessagePage is a page of a conversation's messages, newest first.
type MessagePage struct {
	Items []Message
	Next  *uuid.UUID // the oldest message of the page
}

// Conversations is the conversation part of Service (BE-162): one method per
// use case, the company's and the student's path through Party. Every
// method calls authz.Can first (05 §5.3).
type Conversations interface {
	// ListConversations is listOrgConversations
	// (messaging.conversation.list_org) and listMyConversations
	// (messaging.conversation.list_own).
	ListConversations(ctx context.Context, a authz.Actor, p Party, q ConversationListQuery) (ConversationPage, error)
	// GetConversation is getOrgConversation / getMyConversation
	// (messaging.conversation.read).
	GetConversation(ctx context.Context, a authz.Actor, p Party, id uuid.UUID) (ConversationView, error)
	// ListMessages is listOrgConversationMessages /
	// listMyConversationMessages (messaging.conversation.read).
	ListMessages(ctx context.Context, a authz.Actor, p Party, id uuid.UUID, before *uuid.UUID, limit int) (MessagePage, error)
	// SendMessage is sendOrgConversationMessage /
	// sendMyConversationMessage (messaging.message.send).
	SendMessage(ctx context.Context, a authz.Actor, p Party, id uuid.UUID, text string) (Message, error)
	// MarkRead is markOrgConversationRead / markMyConversationRead
	// (messaging.conversation.mark_read): up to upTo (nil: the newest).
	MarkRead(ctx context.Context, a authz.Actor, p Party, id uuid.UUID, upTo *uuid.UUID) error
	// CloseConversation is closeOrgConversation / closeMyConversation
	// (messaging.conversation.close): idempotent.
	CloseConversation(ctx context.Context, a authz.Actor, p Party, id uuid.UUID) (ConversationView, error)
	// SetMuted is mute/unmute{Org,My}Conversation
	// (messaging.conversation.mute): idempotent.
	SetMuted(ctx context.Context, a authz.Actor, p Party, id uuid.UUID, muted bool) error
}

// surface is the authz view of p's path before a conversation is read:
// the company's org (owner not known yet: the nil UUID, H-27) or the
// caller's own conversations.
func (p Party) surface(a authz.Actor) authz.Resource {
	r := authz.Resource{Type: authz.TypeConversation, Attrs: map[string]string{authz.AttrStatus: ""}}
	if p.Side == SideCompany {
		org, nobody := p.OrgID, uuid.Nil()
		r.OrgID, r.OwnerUserID = &org, &nobody
		return r
	}
	me := a.UserID
	r.OwnerUserID = &me
	return r
}

// resource is the authz view of c on p's path (03 §3.5; H-27): the
// student is the owner; the company's path carries its org.
func (p Party) resource(c Conversation) authz.Resource {
	student := c.StudentUserID
	r := authz.Resource{Type: authz.TypeConversation, ID: c.ID, OwnerUserID: &student,
		Attrs: map[string]string{authz.AttrStatus: string(c.Status)}}
	if p.Side == SideCompany {
		org := c.OrgID
		r.OrgID = &org
	}
	return r
}

// scope is the store query of p's conversations for the viewer a.
func (p Party) scope(a authz.Actor) ConversationQuery {
	q := ConversationQuery{Viewer: a.UserID, ViewerSide: p.Side}
	if p.Side == SideCompany {
		org := p.OrgID
		q.OrgID = &org
	} else {
		me := a.UserID
		q.StudentUserID = &me
	}
	return q
}

// open reads conversation id on p's path for act: authz on the surface
// (403 for staff and members without a company role, 404 for outsiders),
// the row of that company or student (another one's conversation is 404),
// authz on the conversation.
func (s *service) open(ctx context.Context, a authz.Actor, act authz.Action, p Party, id uuid.UUID) (Conversation, error) {
	if err := authz.Can(ctx, a, act, p.surface(a)); err != nil {
		return Conversation{}, err
	}
	var c Conversation
	var err error
	if p.Side == SideCompany {
		c, err = s.conversations().ConversationOfOrg(ctx, p.OrgID, id)
	} else {
		c, err = s.conversations().ConversationOfStudent(ctx, a.UserID, id)
	}
	if err != nil {
		return Conversation{}, err
	}
	if err := authz.Can(ctx, a, act, p.resource(c)); err != nil {
		return Conversation{}, err
	}
	return c, nil
}

// ListConversations implements Service: last activity first.
func (s *service) ListConversations(ctx context.Context, a authz.Actor, p Party, q ConversationListQuery) (ConversationPage, error) {
	act := authz.MessagingConversationListOwn
	if p.Side == SideCompany {
		act = authz.MessagingConversationListOrg
	}
	if err := authz.Can(ctx, a, act, p.surface(a)); err != nil {
		return ConversationPage{}, err
	}
	if p.Side == SideCompany {
		if _, err := s.company(ctx, p.OrgID, false, false); err != nil {
			return ConversationPage{}, err
		}
	}
	if q.Status != "" && !q.Status.Valid() {
		return ConversationPage{}, problem.Field("query.status", "must be open or closed")
	}
	limit := page.ClampLimit(q.Limit)
	sq := p.scope(a)
	sq.Status, sq.After, sq.Limit = q.Status, q.After, limit+1
	rows, err := s.conversations().ConversationRows(ctx, sq)
	if err != nil {
		return ConversationPage{}, err
	}
	rows, more := page.Cut(rows, limit)
	views, err := s.conversationViews(ctx, p, rows)
	if err != nil {
		return ConversationPage{}, err
	}
	out := ConversationPage{Items: views}
	if more {
		last := rows[len(rows)-1].Conversation
		out.Next = &ConversationKey{At: last.LastActivityAt, ID: last.ID}
	}
	return out, nil
}

// conversationViews adds the company (its card on the student's side, its
// status for CanSend) to rows.
func (s *service) conversationViews(ctx context.Context, p Party, rows []ConversationRow) ([]ConversationView, error) {
	ids := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.Conversation.OrgID)
	}
	companies, err := s.d.Orgs.GetMany(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]ConversationView, len(rows))
	for i, r := range rows {
		c := r.Conversation
		v := ConversationView{Conversation: c, Side: p.Side, Muted: r.Muted, Unread: r.Unread, LastRead: r.LastReadMessageID,
			Last: r.Last}
		company, found := companies[c.OrgID]
		if p.Side == SideStudent {
			v.Company = company
		}
		v.CanSend = canSend(c, p.Side, company, found) == nil
		out[i] = v
	}
	return out, nil
}

// canSend is why p's side may not write in c now (nil: it may): a closed
// conversation or a company gone or closed is 409
// messaging.conversation_closed, a suspended company 403
// orgs.org_suspended (both sides: it cannot answer), the company's
// unanswered limit 409 messaging.awaiting_reply.
func canSend(c Conversation, side Side, company orgs.OrgSummary, found bool) error {
	switch {
	case c.Status != ConversationOpen:
		return ErrConversationClosed
	case !found:
		return ErrConversationClosed
	}
	switch company.Status {
	case orgs.StatusActive:
	case orgs.StatusSuspended:
		return orgs.ErrOrgSuspended
	default: // closed (pending_review cannot hold an accepted request)
		return ErrConversationClosed
	}
	if side == SideCompany && c.CompanyUnanswered >= MaxCompanyUnanswered {
		return ErrAwaitingReply
	}
	return nil
}

// view is the viewer's view of one conversation.
func (s *service) view(ctx context.Context, a authz.Actor, p Party, c Conversation) (ConversationView, error) {
	q := p.scope(a)
	q.ID = &c.ID
	rows, err := s.conversations().ConversationRows(ctx, q)
	switch {
	case err != nil:
		return ConversationView{}, err
	case len(rows) == 0:
		return ConversationView{}, ErrNotFound
	}
	views, err := s.conversationViews(ctx, p, rows[:1])
	if err != nil {
		return ConversationView{}, err
	}
	return views[0], nil
}

// GetConversation implements Service.
func (s *service) GetConversation(ctx context.Context, a authz.Actor, p Party, id uuid.UUID) (ConversationView, error) {
	c, err := s.open(ctx, a, authz.MessagingConversationRead, p, id)
	if err != nil {
		return ConversationView{}, err
	}
	return s.view(ctx, a, p, c)
}

// ListMessages implements Service: newest first; before pages back.
func (s *service) ListMessages(ctx context.Context, a authz.Actor, p Party, id uuid.UUID, before *uuid.UUID, limit int) (MessagePage, error) {
	c, err := s.open(ctx, a, authz.MessagingConversationRead, p, id)
	if err != nil {
		return MessagePage{}, err
	}
	limit = page.ClampLimit(limit)
	rows, err := s.conversations().Messages(ctx, c.ID, before, limit+1)
	if err != nil {
		return MessagePage{}, err
	}
	rows, more := page.Cut(rows, limit)
	out := MessagePage{Items: rows}
	if more {
		last := rows[len(rows)-1].ID
		out.Next = &last
	}
	return out, nil
}

// SendMessage implements Service. Order: authz on the path and the
// conversation (404 for another one's) → the text (422) → the company's
// path writes for an active company (403 orgs.org_suspended; closed 404) →
// in one transaction with the optional Idempotency-Key: the row lock, an
// open conversation (409 messaging.conversation_closed), a company that
// can answer (the student's path: 403 orgs.org_suspended, 409 when it is
// gone or closed), the company's unanswered limit (409
// messaging.awaiting_reply) → the message (the conversation's activity and
// unanswered count with it) → the sender's read marker (writing reads the
// conversation) → conversation.message_sent (O) → the audit record (IDs
// only, never the text).
func (s *service) SendMessage(ctx context.Context, a authz.Actor, p Party, id uuid.UUID, text string) (Message, error) {
	c, err := s.open(ctx, a, authz.MessagingMessageSend, p, id)
	if err != nil {
		return Message{}, err
	}
	body, err := cleanText("body.text", text, MinMessageText, MaxMessageText)
	if err != nil {
		return Message{}, err
	}
	if p.Side == SideCompany {
		if _, err := s.company(ctx, p.OrgID, false, true); err != nil {
			return Message{}, err
		}
	}
	hash, err := idempotency.Hash(struct {
		Op   string
		ID   uuid.UUID
		Side Side
		Text string
	}{"send_conversation_message", c.ID, p.Side, body})
	if err != nil {
		return Message{}, err
	}
	mid, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		return s.send(ctx, a, p, c.ID, body)
	})
	if err != nil {
		return Message{}, err
	}
	return s.conversations().MessageIn(ctx, c.ID, mid)
}

// send is SendMessage's transaction.
func (s *service) send(ctx context.Context, a authz.Actor, p Party, id uuid.UUID, body string) (uuid.UUID, error) {
	cur, err := s.conversations().LockConversation(ctx, id)
	if err != nil {
		return uuid.Nil(), err
	}
	company, err := s.d.Orgs.Get(ctx, cur.OrgID)
	found := err == nil
	if err != nil && !errors.Is(err, orgs.ErrNotFound) {
		return uuid.Nil(), err
	}
	if err := canSend(cur, p.Side, company, found); err != nil {
		return uuid.Nil(), err
	}
	now := s.now()
	m, err := s.conversations().InsertMessage(ctx, Message{
		ID: uuid.NewV7(), ConversationID: cur.ID, SenderSide: p.Side, SenderUserID: a.UserID, Body: body, CreatedAt: now,
	})
	if err != nil {
		return uuid.Nil(), err
	}
	if _, err := s.conversations().MarkRead(ctx, cur.ID, a.UserID, m.ID, now); err != nil {
		return uuid.Nil(), err
	}
	// The event carries IDs only: the text never leaves the conversation.
	if err := outbox.Emit(ctx, s.d.Outbox, domainevent.ConversationMessageSent{
		ConversationID: cur.ID, MessageID: m.ID, OrgID: cur.OrgID, SenderSide: string(p.Side),
	}, "conversation", cur.ID); err != nil {
		return uuid.Nil(), err
	}
	ev := conversationEvent(authz.MessagingMessageSend, cur, p, map[string]any{"message_id": m.ID.String()})
	return m.ID, s.d.Audit.Log(ctx, ev.By(a))
}

// MarkRead implements Service: the caller's marker moves forward to upTo
// (a message of the conversation; 422 otherwise) or to the newest message;
// marking again, or an empty conversation, changes nothing. The other side
// is not told (no read receipts).
func (s *service) MarkRead(ctx context.Context, a authz.Actor, p Party, id uuid.UUID, upTo *uuid.UUID) error {
	c, err := s.open(ctx, a, authz.MessagingConversationMarkRead, p, id)
	if err != nil {
		return err
	}
	var target uuid.UUID
	if upTo != nil {
		m, err := s.conversations().MessageIn(ctx, c.ID, *upTo)
		switch {
		case errors.Is(err, ErrNotFound):
			return problem.Field("body.message_id", "must be a message of this conversation")
		case err != nil:
			return err
		}
		target = m.ID
	} else {
		newest, err := s.conversations().Messages(ctx, c.ID, nil, 1)
		if err != nil || len(newest) == 0 {
			return err
		}
		target = newest[0].ID
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		moved, err := s.conversations().MarkRead(ctx, c.ID, a.UserID, target, s.now())
		if err != nil || !moved {
			return err
		}
		return s.d.Audit.Log(ctx, conversationEvent(authz.MessagingConversationMarkRead, c, p,
			map[string]any{"message_id": target.String()}).By(a))
	})
}

// CloseConversation implements Service: either side closes an open
// conversation for good (a new accepted request starts a new one); the
// company's path needs an active company (403 orgs.org_suspended).
// Closing a closed conversation changes nothing. The other side sees the
// status and closed_by; nobody is notified.
func (s *service) CloseConversation(ctx context.Context, a authz.Actor, p Party, id uuid.UUID) (ConversationView, error) {
	c, err := s.open(ctx, a, authz.MessagingConversationClose, p, id)
	if err != nil {
		return ConversationView{}, err
	}
	if p.Side == SideCompany {
		if _, err := s.company(ctx, p.OrgID, false, true); err != nil {
			return ConversationView{}, err
		}
	}
	hash, err := idempotency.Hash(struct {
		Op   string
		ID   uuid.UUID
		Side Side
	}{"close_conversation", c.ID, p.Side})
	if err != nil {
		return ConversationView{}, err
	}
	if _, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		next, ok, err := s.conversations().CloseConversation(ctx, c.ID, p.Side, s.now())
		if err != nil || !ok {
			return c.ID, err
		}
		return next.ID, s.d.Audit.Log(ctx, conversationEvent(authz.MessagingConversationClose, next, p, nil).By(a))
	}); err != nil {
		return ConversationView{}, err
	}
	return s.view(ctx, a, p, c)
}

// SetMuted implements Service: no notice of the conversation reaches the
// caller while it is muted (the other participants are not affected).
func (s *service) SetMuted(ctx context.Context, a authz.Actor, p Party, id uuid.UUID, muted bool) error {
	c, err := s.open(ctx, a, authz.MessagingConversationMute, p, id)
	if err != nil {
		return err
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		changed, err := s.conversations().SetMuted(ctx, c.ID, a.UserID, muted, s.now())
		if err != nil || !changed {
			return err
		}
		return s.d.Audit.Log(ctx, conversationEvent(authz.MessagingConversationMute, c, p,
			map[string]any{"muted": muted}).By(a))
	})
}

// openConversation starts the conversation of an accepted request in the
// acceptance's transaction: the parties and the company come from the
// request (06 H-38).
func (s *service) openConversation(ctx context.Context, r ContactRequest) (Conversation, error) {
	req := r.ID
	return s.conversations().InsertConversation(ctx, Conversation{
		ID: uuid.NewV7(), ContactRequestID: &req, OrgID: r.OrgID, StudentUserID: r.StudentUserID, CompanyUserID: r.SenderUserID,
		Status: ConversationOpen, LastActivityAt: s.now(),
	})
}

// closeOnBlock closes the open conversations of the company with the
// student in the block's transaction (closed_by student: the company never
// learns of the block). It returns how many it closed.
func (s *service) closeOnBlock(ctx context.Context, orgID, studentID uuid.UUID) (int, error) {
	open, err := s.conversations().OpenConversationsOf(ctx, orgID, studentID)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, c := range open {
		_, ok, err := s.conversations().CloseConversation(ctx, c.ID, SideStudent, s.now())
		if err != nil {
			return 0, err
		}
		if ok {
			n++
		}
	}
	return n, nil
}

// conversationEvent is the audit record of a conversation change: the
// company's org scope for the company's own actions (audit.event.list_org),
// none for the student's (the company sees neither who reads nor who
// mutes). The student is named in the metadata (admin view); the text of a
// message never is.
func conversationEvent(action authz.Action, c Conversation, p Party, meta map[string]any) audit.Event {
	id := c.ID
	if meta == nil {
		meta = map[string]any{}
	}
	meta[audit.MetaSubjectUserID] = c.StudentUserID.String()
	ev := audit.Event{Action: string(action), Outcome: audit.Success, ResourceType: authz.TypeConversation, ResourceID: &id,
		Metadata: meta}
	if p.Side == SideCompany {
		org := c.OrgID
		ev.OrgID, ev.ActorOrgID = &org, &org
	} else {
		meta["company_org_id"] = c.OrgID.String()
	}
	return ev
}
