package messaging

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/db"
)

// The conversations' realtime signal (BE-162b; 04 §W, BE-054's hub): a
// WebSocket per party tells its clients that a conversation changed, so an
// open list or conversation reloads at once instead of polling. It carries
// no content: the type, the conversation's ID and the time; the client
// reads the rest through the REST operations (their authorisation, their
// unread counts).
//
// Who listens: the student on /me/conversations/live (topic
// user:<id>:conversations, messaging.conversation.list_own), the
// company's recruiters and admins on /orgs/{org_id}/conversations/live
// (topic org:<id>:conversations, messaging.conversation.list_org; the
// surface rules of listOrgConversations: 403 for the org's other members
// and the staff, 404 for everyone else). The hub re-authorises every
// connection every minute: a member who lost the role is closed (4403).
//
// What travels: {"type":"message"|"closed","conversation_id","at"} on both
// parties' topics after the commit of a message or a closing (a block's
// closings included: the company sees closed_by student anyway). A muted
// conversation still signals (mute is about notices). Publishing runs
// after the commit (db.AfterCommit); a failure is logged and never fails
// the write; a replayed Idempotency-Key publishes nothing.

// LivePath and OrgLivePath are the WebSocket routes under /api/v1 (gin
// routes outside OpenAPI; authz matrix: non_api_routes, websocket: true).
const (
	LivePath    = "/me/conversations/live"
	OrgLivePath = "/orgs/{org_id}/conversations/live"
)

// Signal types.
const (
	liveMessage = "message"
	liveClosed  = "closed"
)

// liveTimeout bounds the publication after a commit.
const liveTimeout = 3 * time.Second

// StudentLiveTopic is the student's topic (rt:user:<id>:conversations on
// the bus).
func StudentLiveTopic(userID uuid.UUID) string { return "user:" + userID.String() + ":conversations" }

// OrgLiveTopic is the company's topic (rt:org:<id>:conversations).
func OrgLiveTopic(orgID uuid.UUID) string { return "org:" + orgID.String() + ":conversations" }

// liveSignal is one signal: never a message's text or a user.
type liveSignal struct {
	Type           string    `json:"type"`
	ConversationID string    `json:"conversation_id"`
	At             time.Time `json:"at"`
}

// OpenLive authorises a on p's path for the WebSocket (the list action of
// the path, as listOrgConversations / listMyConversations) and returns its
// topic; the hub calls it again to re-authorise the connection.
func (s *service) OpenLive(ctx context.Context, a authz.Actor, p Party) (string, error) {
	act := authz.MessagingConversationListOwn
	if p.Side == SideCompany {
		act = authz.MessagingConversationListOrg
	}
	if err := authz.Can(ctx, a, act, p.surface(a)); err != nil {
		return "", err
	}
	if p.Side == SideCompany {
		if _, err := s.company(ctx, p.OrgID, false, false); err != nil {
			return "", err
		}
		return OrgLiveTopic(p.OrgID), nil
	}
	return StudentLiveTopic(a.UserID), nil
}

// signal publishes typ of conversation c on both parties' topics once the
// transaction of ctx commits.
func (s *service) signal(ctx context.Context, c Conversation, typ string, at time.Time) {
	if s.d.Live == nil {
		return
	}
	db.AfterCommit(ctx, func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), liveTimeout)
		defer cancel()
		msg, err := json.Marshal(liveSignal{Type: typ, ConversationID: c.ID.String(), At: at.UTC()})
		if err == nil {
			err = s.d.Live.Publish(ctx, StudentLiveTopic(c.StudentUserID), msg)
		}
		if err == nil {
			err = s.d.Live.Publish(ctx, OrgLiveTopic(c.OrgID), msg)
		}
		if err != nil {
			s.d.Log.LogAttrs(ctx, slog.LevelWarn, "conversation signal not published",
				slog.String("conversation_id", c.ID.String()), slog.Any("err", err))
		}
	})
}
