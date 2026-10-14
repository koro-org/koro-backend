package billing

import (
	"context"
	"time"
	"uuid"

	"koro/internal/platform/privacy"
)

// invoicesRetainedReason explains why Erase keeps the invoices (02 §22.1:
// "silinmez (iptal = kredi notu); süre sonunda hard delete").
const invoicesRetainedReason = "invoices are financial records kept for 10 years (financial_records; tax and commercial " +
	"law, VUK/TTK): they are neither deleted nor anonymised on account erasure; uploaded_by and verified_by stay as " +
	"references to the anonymised account and the rows are destroyed when the retention period ends"

// privacyProvider is the KVKK provider of billing (01 §3.3). The parties of
// an invoice are legal entities in the MVP (06 H-54; their snapshots are
// the parties' own data), so a user's personal data here are the references
// of who uploaded or verified an invoice. Export lists them with the
// invoice's kind, state and amount; Erase retains everything (a legal
// obligation, recorded in the DSR answer).
type privacyProvider struct{ s *service }

var _ privacy.Provider = privacyProvider{}

// Name implements privacy.Provider.
func (privacyProvider) Name() string { return "billing" }

// Export implements privacy.Provider.
func (p privacyProvider) Export(ctx context.Context, userID uuid.UUID) (privacy.ExportSection, error) {
	out := privacy.ExportSection{Module: "billing", Items: []map[string]any{}}
	rows, err := p.s.d.Store.InvoicesBy(ctx, userID)
	if err != nil {
		return privacy.ExportSection{}, err
	}
	for _, inv := range rows {
		item := func(record string, at time.Time) map[string]any {
			return map[string]any{
				"record": record, "invoice_id": inv.ID.String(), "kind": string(inv.Kind), "status": string(inv.Status),
				"total_amount": inv.TotalAmount, "currency": inv.Currency, "at": at.UTC().Format(time.RFC3339),
			}
		}
		if inv.UploadedBy != nil && *inv.UploadedBy == userID {
			out.Items = append(out.Items, item("invoice_uploaded", inv.UpdatedAt))
		}
		if inv.VerifiedBy != nil && *inv.VerifiedBy == userID && inv.VerifiedAt != nil {
			out.Items = append(out.Items, item("invoice_verified", *inv.VerifiedAt))
		}
	}
	return out, nil
}

// Erase implements privacy.Provider: nothing is deleted or anonymised (see
// invoicesRetainedReason). It is idempotent.
func (p privacyProvider) Erase(ctx context.Context, userID uuid.UUID) (privacy.EraseResult, error) {
	res := privacy.EraseResult{Module: "billing"}
	rows, err := p.s.d.Store.InvoicesBy(ctx, userID)
	if err != nil {
		return privacy.EraseResult{}, err
	}
	if res.Retained = len(rows); res.Retained > 0 {
		res.RetainedReason = invoicesRetainedReason
	}
	return res, nil
}
