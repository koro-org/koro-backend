package events

import (
	"context"
	"uuid"

	"koro/internal/platform/privacy"
)

// privacyRetainedReason explains what Erase leaves in place.
const privacyRetainedReason = "events rows hold no personal data (02 §11.1): created_by and attendance_finalized_by " +
	"reference the account, which identity anonymises, as do the form columns updated_by, published_by and " +
	"lint_override_by (02 §11.3-11.5); registrations, tickets and check-ins are BE-028/BE-031's"

// privacyProvider is the KVKK provider of the event core (01 §3.3): what
// it knows about a person is their staff assignments (02 §11.2: who works
// the door of which event). Export lists them; Erase deletes them (the
// assignment has no legal retention). Registrations, tickets, check-ins
// and exports join this provider in their tickets.
type privacyProvider struct{ s *service }

var _ privacy.Provider = privacyProvider{}

// Name implements privacy.Provider.
func (privacyProvider) Name() string { return "events" }

// Export implements privacy.Provider.
func (p privacyProvider) Export(ctx context.Context, userID uuid.UUID) (privacy.ExportSection, error) {
	rows, err := p.s.d.Store.StaffOfUser(ctx, userID)
	if err != nil {
		return privacy.ExportSection{}, err
	}
	out := privacy.ExportSection{Module: "events", Items: make([]map[string]any, 0, len(rows))}
	for _, r := range rows {
		out.Items = append(out.Items, map[string]any{
			"type": "event_staff", "event_id": r.EventID.String(), "org_id": r.OrgID.String(), "role": string(r.Role),
			"created_at": r.CreatedAt,
		})
	}
	return out, nil
}

// Erase implements privacy.Provider; idempotent (a second run deletes
// nothing).
func (p privacyProvider) Erase(ctx context.Context, userID uuid.UUID) (privacy.EraseResult, error) {
	var n int
	err := p.s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		var err error
		n, err = p.s.d.Store.DeleteStaffOfUser(ctx, userID)
		return err
	})
	if err != nil {
		return privacy.EraseResult{}, err
	}
	return privacy.EraseResult{Module: "events", Deleted: n, RetainedReason: privacyRetainedReason}, nil
}
