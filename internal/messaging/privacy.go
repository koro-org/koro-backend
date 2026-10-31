package messaging

import (
	"context"
	"fmt"
	"time"
	"uuid"

	"koro/internal/platform/privacy"
)

// privacyRetainedReason explains the requests a user sent for a company
// and the messages a company member wrote: they are the company's record
// (02 §14.1, §14.4: org scope), kept until their own retention
// (contact_request_closed, conversation_closed); sender_user_id points at
// the account identity anonymises last, so they no longer name their sender.
const privacyRetainedReason = "contact_requests sent and conversation_messages written for a company are the company's " +
	"record (02 §14.1, §14.4); sender_user_id references the anonymised account; deleted with their retention " +
	"(contact_request_closed: 12 months after closing; conversation_closed: 12 months after the conversation closed or went quiet)"

// privacyProvider is the KVKK provider of messaging (01 §3.3): the requests
// a user received (02 §14.1: hard delete with the account), the requests a
// user sent for a company (exported, kept), the user's blocks (02 §14.2:
// hard delete), the student's conversations with every message (hard
// delete), the messages a company member wrote (exported, kept) and the
// user's read/mute states (hard delete; BE-162).
type privacyProvider struct{ s *service }

var _ privacy.Provider = privacyProvider{}

// Name implements privacy.Provider.
func (privacyProvider) Name() string { return "messaging" }

// Export implements privacy.Provider. A sent request leaves out the student
// (the sender's export never names who was asked).
func (p privacyProvider) Export(ctx context.Context, userID uuid.UUID) (privacy.ExportSection, error) {
	st := p.s.d.Store
	out := privacy.ExportSection{Module: "messaging", Items: []map[string]any{}}
	received, err := st.ReceivedBy(ctx, userID)
	if err != nil {
		return privacy.ExportSection{}, err
	}
	now := p.s.now()
	for _, r := range received {
		item := exportRequest("contact_request_received", r, now)
		item["company_org_id"] = r.OrgID.String()
		out.Items = append(out.Items, item)
	}
	sent, err := st.SentBy(ctx, userID)
	if err != nil {
		return privacy.ExportSection{}, err
	}
	for _, r := range sent {
		item := exportRequest("contact_request_sent", r, now)
		item["org_id"] = r.OrgID.String()
		out.Items = append(out.Items, item)
	}
	for after := (*BlockKey)(nil); ; {
		blocks, err := st.Blocks(ctx, userID, after, exportBatch)
		if err != nil {
			return privacy.ExportSection{}, err
		}
		for _, b := range blocks {
			out.Items = append(out.Items, map[string]any{
				"record": "company_block", "company_org_id": b.CompanyOrgID.String(),
				"created_at": b.CreatedAt.UTC().Format(time.RFC3339),
			})
		}
		if len(blocks) < exportBatch {
			break
		}
		last := blocks[len(blocks)-1]
		after = &BlockKey{CreatedAt: last.CreatedAt, OrgID: last.CompanyOrgID}
	}
	items, err := p.exportConversations(ctx, userID)
	if err != nil {
		return privacy.ExportSection{}, err
	}
	out.Items = append(out.Items, items...)
	return out, nil
}

// exportConversations is the conversation part of the export (BE-162): the
// student's conversations with every message (a company message without the
// member who wrote it), the messages the user wrote in a company's
// conversations (without the student) and the user's read/mute states.
func (p privacyProvider) exportConversations(ctx context.Context, userID uuid.UUID) ([]map[string]any, error) {
	st := p.s.d.Store
	var out []map[string]any
	convs, err := st.StudentConversations(ctx, userID)
	if err != nil {
		return nil, err
	}
	for _, c := range convs {
		item := map[string]any{
			"record": "conversation", "conversation_id": c.ID.String(), "company_org_id": c.OrgID.String(),
			"status": string(c.Status), "created_at": c.CreatedAt.UTC().Format(time.RFC3339),
			"last_activity_at": c.LastActivityAt.UTC().Format(time.RFC3339),
		}
		if c.ClosedAt != nil && c.ClosedBy != nil {
			item["closed_at"], item["closed_by"] = c.ClosedAt.UTC().Format(time.RFC3339), string(*c.ClosedBy)
		}
		out = append(out, item)
		msgs, err := st.AllMessages(ctx, c.ID)
		if err != nil {
			return nil, err
		}
		for _, m := range msgs {
			out = append(out, exportMessage("conversation_message", m, m.SenderUserID == userID))
		}
	}
	sent, err := st.MessagesSentBy(ctx, userID)
	if err != nil {
		return nil, err
	}
	for _, m := range sent {
		if m.SenderSide == SideCompany {
			out = append(out, exportMessage("conversation_message_sent", m, true))
		}
	}
	states, err := st.UserStates(ctx, userID)
	if err != nil {
		return nil, err
	}
	for _, s := range states {
		item := map[string]any{"record": "conversation_state", "conversation_id": s.ConversationID.String(), "muted": s.MutedAt != nil}
		if s.ReadAt != nil {
			item["read_at"] = s.ReadAt.UTC().Format(time.RFC3339)
		}
		out = append(out, item)
	}
	return out, nil
}

// exportMessage is one message of the export: the text, the side and
// whether the data subject wrote it (never another person's user ID).
func exportMessage(record string, m Message, mine bool) map[string]any {
	return map[string]any{
		"record": record, "conversation_id": m.ConversationID.String(), "message_id": m.ID.String(),
		"sender_side": string(m.SenderSide), "mine": mine, "text": m.Body, "created_at": m.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// exportBatch is how many blocks one export read takes.
const exportBatch = 500

func exportRequest(record string, r ContactRequest, now time.Time) map[string]any {
	item := map[string]any{
		"record": record, "contact_request_id": r.ID.String(), "via": string(r.Via), "subject": r.Subject,
		"message": r.Message, "status": string(r.Effective(now)), "expires_at": r.ExpiresAt.UTC().Format(time.RFC3339),
		"created_at": r.CreatedAt.UTC().Format(time.RFC3339),
	}
	if r.OpportunityPostID != nil {
		item["opportunity_post_id"] = r.OpportunityPostID.String()
	}
	if r.RespondedAt != nil {
		item["responded_at"] = r.RespondedAt.UTC().Format(time.RFC3339)
	}
	return item
}

// Erase implements privacy.Provider: the student's conversations (their
// messages and states with them), the received requests, the blocks and
// the user's read/mute states go in one transaction (idempotent: a second
// run finds nothing); the sent requests and a company member's messages
// stay (the company's record, see privacyRetainedReason). The company's
// level 3 grant of an accepted request is students' (its provider erases
// it).
func (p privacyProvider) Erase(ctx context.Context, userID uuid.UUID) (privacy.EraseResult, error) {
	res := privacy.EraseResult{Module: "messaging"}
	st := p.s.d.Store
	err := p.s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		c, err := st.DeleteStudentConversations(ctx, userID)
		if err != nil {
			return fmt.Errorf("messaging: erase conversations: %w", err)
		}
		n, err := st.DeleteReceived(ctx, userID)
		if err != nil {
			return fmt.Errorf("messaging: erase requests: %w", err)
		}
		b, err := st.DeleteBlocks(ctx, userID)
		if err != nil {
			return fmt.Errorf("messaging: erase blocks: %w", err)
		}
		u, err := st.DeleteUserStates(ctx, userID)
		if err != nil {
			return fmt.Errorf("messaging: erase conversation states: %w", err)
		}
		sent, err := st.SentBy(ctx, userID)
		if err != nil {
			return err
		}
		written, err := st.MessagesSentBy(ctx, userID)
		if err != nil {
			return err
		}
		res.Deleted, res.Retained = c+n+b+u, len(sent)+len(written)
		return nil
	})
	if err != nil {
		return privacy.EraseResult{}, err
	}
	if res.Retained > 0 {
		res.RetainedReason = privacyRetainedReason
	}
	return res, nil
}
