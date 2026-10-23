package orders

import (
	"context"
	"time"
	"uuid"

	"koro/internal/platform/privacy"
)

// retainedReason explains why Erase keeps what it finds (02 §19: orders
// and their lines are financial records, "silinmez; süre sonunda hard
// delete").
const retainedReason = "orders and order lines are financial records kept for 10 years (financial_records; tax and " +
	"commercial law): they are neither deleted nor anonymised on account erasure; placed_by_user_id and " +
	"fulfillment_marked_by stay as references to the anonymised account, the delivery evidence files stay with " +
	"their lines, and the rows are destroyed when the retention period ends"

// privacyProvider is the KVKK provider of orders (01 §3.3). An order is a
// company's (a legal entity's) purchase; the personal data are the user
// references: who placed an order (orders.placed_by_user_id) and who marked
// a line delivered (order_lines.fulfillment_marked_by, with the delivery
// evidence file it attached). Export lists them with the order's reference
// and state (no buyer snapshot: it is the
// company's invoice data, not the person's); Erase retains everything (a
// legal obligation, recorded in the DSR answer).
type privacyProvider struct{ s *service }

var _ privacy.Provider = privacyProvider{}

// Name implements privacy.Provider.
func (privacyProvider) Name() string { return "orders" }

// Export implements privacy.Provider.
func (p privacyProvider) Export(ctx context.Context, userID uuid.UUID) (privacy.ExportSection, error) {
	os, err := p.s.d.Store.OrdersPlacedBy(ctx, userID)
	if err != nil {
		return privacy.ExportSection{}, err
	}
	ls, err := p.s.d.Store.LinesMarkedBy(ctx, userID)
	if err != nil {
		return privacy.ExportSection{}, err
	}
	out := privacy.ExportSection{Module: "orders", Items: make([]map[string]any, 0, len(os)+len(ls))}
	for _, o := range os {
		out.Items = append(out.Items, map[string]any{
			"record": "order_placed", "order_id": o.ID.String(), "order_no": o.OrderNo, "org_id": o.OrgID.String(),
			"status": string(o.Status), "payable_amount": o.PayableAmount, "currency": o.Currency,
			"placed_at": o.PlacedAt.UTC().Format(time.RFC3339),
		})
	}
	for _, l := range ls {
		item := map[string]any{
			"record": "order_line_marked_fulfilled", "line_id": l.ID.String(), "order_id": l.OrderID.String(),
			"seller_org_id": l.SellerOrgID.String(), "title": l.TitleSnapshot, "fulfillment_status": string(l.FulfillmentStatus),
		}
		if l.FulfillmentMarkedAt != nil {
			item["marked_at"] = l.FulfillmentMarkedAt.UTC().Format(time.RFC3339)
		}
		if l.FulfillmentEvidenceFileID != nil {
			// The file itself is the seller org's record (BE-093); its ID lets the
			// DPO hand it out with the answer.
			item["evidence_file_id"] = l.FulfillmentEvidenceFileID.String()
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}

// Erase implements privacy.Provider: nothing is deleted or anonymised (see
// retainedReason). It is idempotent.
func (p privacyProvider) Erase(ctx context.Context, userID uuid.UUID) (privacy.EraseResult, error) {
	os, err := p.s.d.Store.OrdersPlacedBy(ctx, userID)
	if err != nil {
		return privacy.EraseResult{}, err
	}
	ls, err := p.s.d.Store.LinesMarkedBy(ctx, userID)
	if err != nil {
		return privacy.EraseResult{}, err
	}
	res := privacy.EraseResult{Module: "orders", Retained: len(os) + len(ls)}
	if res.Retained > 0 {
		res.RetainedReason = retainedReason
	}
	return res, nil
}
