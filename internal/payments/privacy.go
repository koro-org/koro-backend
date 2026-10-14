package payments

import (
	"context"
	"time"
	"uuid"

	"koro/internal/platform/privacy"
)

// retainedReason explains why Erase keeps what it finds (02 §20: transfers
// and payments are financial records, "silinmez; süre sonunda hard delete").
const retainedReason = "incoming bank transfers and payments are financial records kept for 10 years (financial_records; " +
	"tax and commercial law): they are neither deleted nor anonymised on account erasure; the staff references " +
	"(entered_by, matched_by, return_requested_by, return_approved_by, captured_by) stay as references to the " +
	"anonymised account and the rows are destroyed when the retention period ends"

// privacyProvider is the KVKK provider of payments (01 §3.3). The user
// references of payments are staff actions: who recorded, matched or
// handled the return of a transfer, who captured a payment by hand. Export
// lists them with IDs, amounts and states (never a sender's name or IBAN:
// the sender of a transfer is not linked to an account; a request from a
// sender who is not a user is answered by the DPO from the rows by hand);
// Erase retains everything (a legal obligation, recorded in the DSR
// answer).
type privacyProvider struct{ s *service }

var _ privacy.Provider = privacyProvider{}

// Name implements privacy.Provider.
func (privacyProvider) Name() string { return "payments" }

// Export implements privacy.Provider.
func (p privacyProvider) Export(ctx context.Context, userID uuid.UUID) (privacy.ExportSection, error) {
	ts, err := p.s.d.Store.TransfersOfUser(ctx, userID)
	if err != nil {
		return privacy.ExportSection{}, err
	}
	ps, err := p.s.d.Store.PaymentsCapturedBy(ctx, userID)
	if err != nil {
		return privacy.ExportSection{}, err
	}
	out := privacy.ExportSection{Module: "payments", Items: make([]map[string]any, 0, len(ts)+len(ps))}
	is := func(id *uuid.UUID) bool { return id != nil && *id == userID }
	for _, t := range ts {
		var roles []string
		if t.EnteredBy == userID {
			roles = append(roles, "entered")
		}
		if is(t.MatchedBy) {
			roles = append(roles, "matched")
		}
		if is(t.ReturnRequestedBy) {
			roles = append(roles, "return_requested")
		}
		if is(t.ReturnApprovedBy) {
			roles = append(roles, "return_approved")
		}
		out.Items = append(out.Items, map[string]any{
			"record": "incoming_transfer", "transfer_id": t.ID.String(), "roles": roles, "status": string(t.Status),
			"amount": t.Amount, "currency": t.Currency, "value_date": t.ValueDate.String(),
			"recorded_at": t.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	for _, py := range ps {
		item := map[string]any{"record": "payment_captured", "payment_id": py.ID.String(), "order_id": py.OrderID.String(),
			"provider": string(py.Provider), "amount": py.Amount, "currency": py.Currency}
		if py.CapturedAt != nil {
			item["captured_at"] = py.CapturedAt.UTC().Format(time.RFC3339)
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}

// Erase implements privacy.Provider: nothing is deleted or anonymised (see
// retainedReason). It is idempotent.
func (p privacyProvider) Erase(ctx context.Context, userID uuid.UUID) (privacy.EraseResult, error) {
	ts, err := p.s.d.Store.TransfersOfUser(ctx, userID)
	if err != nil {
		return privacy.EraseResult{}, err
	}
	ps, err := p.s.d.Store.PaymentsCapturedBy(ctx, userID)
	if err != nil {
		return privacy.EraseResult{}, err
	}
	res := privacy.EraseResult{Module: "payments", Retained: len(ts) + len(ps)}
	if res.Retained > 0 {
		res.RetainedReason = retainedReason
	}
	return res, nil
}
