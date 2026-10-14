package ledger

import (
	"context"
	"time"
	"uuid"

	"koro/internal/platform/privacy"
)

// payoutsRetainedReason explains why Erase keeps the payouts (02 §21.4:
// "silinmez; süre sonunda hard delete").
const payoutsRetainedReason = "payouts are financial records kept for 10 years (financial_records; tax and commercial " +
	"law): they are neither deleted nor anonymised on account erasure; created_by, approved_by and paid_by stay as " +
	"references to the anonymised account and the rows are destroyed when the retention period ends"

// privacyProvider is the KVKK provider of the ledger (01 §3.3). The payee
// of a payout is a legal entity in the MVP (06 H-54: no natural-person
// payee), so the personal data are the staff references: who created,
// approved and marked a payout paid. Export lists them with the payout's
// state and amount (never the IBAN); Erase retains everything (a legal
// obligation, recorded in the DSR answer). ledger_transactions.created_by
// is a staff reference of the same kind (classified staff).
type privacyProvider struct{ s *service }

var _ privacy.Provider = privacyProvider{}

// Name implements privacy.Provider.
func (privacyProvider) Name() string { return "ledger" }

// Export implements privacy.Provider.
func (p privacyProvider) Export(ctx context.Context, userID uuid.UUID) (privacy.ExportSection, error) {
	out := privacy.ExportSection{Module: "ledger", Items: []map[string]any{}}
	if p.s.d.Payouts == nil {
		return out, nil
	}
	rows, err := p.s.d.Payouts.PayoutsBy(ctx, userID)
	if err != nil {
		return privacy.ExportSection{}, err
	}
	for _, po := range rows {
		item := func(record string, at time.Time) map[string]any {
			return map[string]any{
				"record": record, "payout_id": po.ID.String(), "payee_id": po.PayeeID.String(), "org_id": po.OrgID.String(),
				"status": string(po.Status), "amount": po.Amount, "currency": po.Currency, "at": at.UTC().Format(time.RFC3339),
			}
		}
		if po.CreatedBy == userID {
			out.Items = append(out.Items, item("payout_created", po.CreatedAt))
		}
		if po.ApprovedBy != nil && *po.ApprovedBy == userID && po.ApprovedAt != nil {
			out.Items = append(out.Items, item("payout_approved", *po.ApprovedAt))
		}
		if po.PaidBy != nil && *po.PaidBy == userID && po.PaidAt != nil {
			out.Items = append(out.Items, item("payout_marked_paid", *po.PaidAt))
		}
	}
	return out, nil
}

// Erase implements privacy.Provider: nothing is deleted or anonymised (see
// payoutsRetainedReason). It is idempotent.
func (p privacyProvider) Erase(ctx context.Context, userID uuid.UUID) (privacy.EraseResult, error) {
	res := privacy.EraseResult{Module: "ledger"}
	if p.s.d.Payouts == nil {
		return res, nil
	}
	rows, err := p.s.d.Payouts.PayoutsBy(ctx, userID)
	if err != nil {
		return privacy.EraseResult{}, err
	}
	if res.Retained = len(rows); res.Retained > 0 {
		res.RetainedReason = payoutsRetainedReason
	}
	return res, nil
}
