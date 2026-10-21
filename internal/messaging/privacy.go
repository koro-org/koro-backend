package messaging

import (
	"context"
	"fmt"
	"time"
	"uuid"

	"koro/internal/platform/privacy"
)

// privacyRetainedReason explains the requests a user sent for a company:
// they are the company's record (02 §14.1: org scope), kept until their own
// retention (contact_request_closed); sender_user_id points at the account
// identity anonymises last, so a request no longer names its sender.
const privacyRetainedReason = "contact_requests sent for a company are the company's record (02 §14.1); " +
	"sender_user_id references the anonymised account; deleted 12 months after closing (contact_request_closed)"

// privacyProvider is the KVKK provider of messaging (01 §3.3): the requests
// a user received (02 §14.1: hard delete with the account), the requests a
// user sent for a company (exported, kept) and the user's blocks (02 §14.2:
// hard delete).
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
	return out, nil
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

// Erase implements privacy.Provider: the received requests and the blocks
// go in one transaction (idempotent: a second run finds nothing); the sent
// requests stay (the company's record, see privacyRetainedReason). The
// company's level 3 grant of an accepted request is students' (its
// provider erases it).
func (p privacyProvider) Erase(ctx context.Context, userID uuid.UUID) (privacy.EraseResult, error) {
	res := privacy.EraseResult{Module: "messaging"}
	err := p.s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		n, err := p.s.d.Store.DeleteReceived(ctx, userID)
		if err != nil {
			return fmt.Errorf("messaging: erase requests: %w", err)
		}
		b, err := p.s.d.Store.DeleteBlocks(ctx, userID)
		if err != nil {
			return fmt.Errorf("messaging: erase blocks: %w", err)
		}
		sent, err := p.s.d.Store.SentBy(ctx, userID)
		if err != nil {
			return err
		}
		res.Deleted, res.Retained = n+b, len(sent)
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
