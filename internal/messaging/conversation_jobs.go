package messaging

import (
	"context"
	"errors"
	"uuid"

	"koro/internal/authz"
	"koro/internal/notify"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/queue"
)

// tmplMessage is the notice of a new conversation message
// (internal/notify/templates/messaging/conversation_message.*; BE-162):
// in-app and push, fixed texts, the data the conversation's ID, the deep
// link the conversation on the recipient's side, preference
// contact_requests. The message itself never enters a notice.
const tmplMessage = "messaging.conversation_message"

// messageChannels are the channels of a message notice: the conversation
// lives in the app, so no e-mail (an e-mail digest is BE-162b's).
var messageChannels = []notify.Channel{notify.ChannelInApp, notify.ChannelPush}

// handleMessageSent tells the other side of the conversation that a
// message arrived. A recipient who muted the conversation or already read
// past the message is skipped; an unread run is told once: the dedupe key
// is the recipient's read marker (tmpl:<marker, or the conversation before
// the first read>:user), so notify drops every later notice of the run
// until the recipient reads. A conversation or message gone meanwhile
// (erased, swept) is skipped.
func (s *service) handleMessageSent(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.ConversationMessageSent](e)
	if err != nil {
		return queue.Permanent(err)
	}
	c, err := s.conversations().ConversationOfOrg(ctx, p.OrgID, p.ConversationID)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil
	case err != nil:
		return err
	}
	m, err := s.conversations().MessageIn(ctx, c.ID, p.MessageID)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil
	case err != nil:
		return err
	}
	var to []uuid.UUID
	n := notice{tmpl: tmplMessage, data: map[string]any{"conversation_id": c.ID.String()}, channels: messageChannels}
	if m.SenderSide == SideCompany {
		to = []uuid.UUID{c.StudentUserID}
		org := c.OrgID
		n.link, n.source = notify.MyConversationLink(c.ID), &org
	} else {
		if to, err = s.conversationCompany(ctx, c); err != nil {
			return err
		}
		n.link = notify.OrgConversationLink(c.OrgID, c.ID)
	}
	var errs []error
	for _, u := range to {
		run, ok, err := s.unreadRun(ctx, c.ID, u, m)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		n.ref = run
		errs = append(errs, s.deliver(ctx, n, []uuid.UUID{u}))
	}
	return errors.Join(errs...)
}

// unreadRun returns the key of u's unread run that m belongs to (u's read
// marker, or the conversation before u's first read); ok=false when u
// muted the conversation or already read past m.
func (s *service) unreadRun(ctx context.Context, conversationID, u uuid.UUID, m Message) (uuid.UUID, bool, error) {
	st, found, err := s.conversations().State(ctx, conversationID, u)
	switch {
	case err != nil:
		return uuid.Nil(), false, err
	case !found || st.LastReadMessageID == nil:
		return conversationID, !found || st.MutedAt == nil, nil
	case st.MutedAt != nil, compareID(*st.LastReadMessageID, m.ID) >= 0:
		return uuid.Nil(), false, nil
	}
	return *st.LastReadMessageID, true, nil
}

// conversationCompany is the company side told of the student's message:
// the requester and the members who wrote in the conversation while they
// still are its recruiters, admins or owner; without one of them the
// company's owner and admins.
func (s *service) conversationCompany(ctx context.Context, c Conversation) ([]uuid.UUID, error) {
	senders, err := s.conversations().CompanySenders(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	out, err := s.withRole(ctx, c.OrgID, append([]uuid.UUID{c.CompanyUserID}, senders...),
		authz.MemberRecruiter, authz.MemberCompanyAdmin, authz.MemberCompanyOwner)
	if err != nil || len(out) > 0 {
		return out, err
	}
	ids, err := s.d.Orgs.ActiveMemberIDs(ctx, c.OrgID)
	if err != nil {
		return nil, err
	}
	return s.withRole(ctx, c.OrgID, ids, authz.MemberCompanyOwner, authz.MemberCompanyAdmin)
}
