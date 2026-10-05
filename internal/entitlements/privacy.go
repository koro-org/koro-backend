package entitlements

import (
	"context"
	"uuid"

	"koro/internal/platform/privacy"
)

// retainedReason explains why Erase keeps the usages.
const retainedReason = "entitlement usages are the company's append-only record of its quota use (02 §10.2); " +
	"actor_user_id points to the account, which identity turns into a tombstone without name or address, so a usage " +
	"no longer identifies the person; they go with their right (entitlement_other, 2 years) or their order " +
	"(financial_records, 10 years)"

// privacyProvider is the KVKK provider of entitlements (01 §3.3; T-196,
// 06 H-49, BE-057). What it knows about a person: the usages they made on
// behalf of their company (entitlement_usages.actor_user_id, personal per
// 02 §10.2). Export lists them; Erase keeps them (append-only commercial
// record, the reference is anonymised by identity's tombstone) and reports
// them as retained. The rights themselves belong to the org (granted_by is
// the staff member's, 02 staff).
type privacyProvider struct{ s *service }

var _ privacy.Provider = privacyProvider{}

// Name implements privacy.Provider.
func (privacyProvider) Name() string { return "entitlements" }

// Export implements privacy.Provider.
func (p privacyProvider) Export(ctx context.Context, userID uuid.UUID) (privacy.ExportSection, error) {
	rows, err := p.s.d.Store.UsagesOfActor(ctx, userID)
	if err != nil {
		return privacy.ExportSection{}, err
	}
	out := privacy.ExportSection{Module: "entitlements", Items: make([]map[string]any, 0, len(rows))}
	for _, u := range rows {
		item := map[string]any{
			"type": "entitlement_usage", "usage_id": u.ID.String(), "entitlement_id": u.EntitlementID.String(),
			"org_id": u.OrgID.String(), "quantity": u.Quantity, "ref_type": string(u.RefType), "ref_id": u.RefID.String(),
			"created_at": u.CreatedAt,
		}
		if u.ReversalOf != nil {
			item["reversal_of"] = u.ReversalOf.String()
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}

// Erase implements privacy.Provider; idempotent (it changes nothing).
func (p privacyProvider) Erase(ctx context.Context, userID uuid.UUID) (privacy.EraseResult, error) {
	rows, err := p.s.d.Store.UsagesOfActor(ctx, userID)
	if err != nil {
		return privacy.EraseResult{}, err
	}
	res := privacy.EraseResult{Module: "entitlements", Retained: len(rows)}
	if res.Retained > 0 {
		res.RetainedReason = retainedReason
	}
	return res, nil
}
