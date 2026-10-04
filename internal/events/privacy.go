package events

import (
	"context"
	"uuid"

	"koro/internal/platform/privacy"
)

// privacyRetainedReason explains what Erase leaves in place.
const privacyRetainedReason = "events rows hold no personal data (02 §11.1): created_by and attendance_finalized_by " +
	"reference the account, which identity anonymises, as do the form columns updated_by, published_by and " +
	"lint_override_by (02 §11.3-11.5) and a registration's approved_by; check-ins go with their registration " +
	"(ON DELETE CASCADE)"

// privacyProvider is the KVKK provider of events (01 §3.3): a person's
// staff assignments (02 §11.2), registrations with their answers (the
// special-category ones opened for the data subject) and consent snapshot,
// and tickets (02 §11.6–11.7). Export lists them; Erase deletes them (the
// tickets and check-ins go with their registration, a held seat is
// released and given to the waiting list). Exports of registrations
// (BE-050) join this provider in their ticket.
type privacyProvider struct{ s *service }

var _ privacy.Provider = privacyProvider{}

// Name implements privacy.Provider.
func (privacyProvider) Name() string { return "events" }

// Export implements privacy.Provider. Ticket tokens and manual codes are
// credentials, not exported (the ticket's version and status are).
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
	regs, err := p.s.d.Store.RegistrationsOfUser(ctx, userID)
	if err != nil {
		return privacy.ExportSection{}, err
	}
	for _, r := range regs {
		item := map[string]any{
			"type": "event_registration", "registration_id": r.ID.String(), "event_id": r.EventID.String(),
			"org_id": r.OrgID.String(), "status": string(r.Status), "source": string(r.Source),
			"answers": decodeAnswers(r.Answers), "consent_snapshot": decodeSnapshot(r.ConsentSnapshot),
			"created_at": r.CreatedAt, "confirmed_at": r.ConfirmedAt, "waitlisted_at": r.WaitlistedAt,
			"cancelled_at": r.CancelledAt, "checked_in_at": r.CheckedInAt,
		}
		if r.FormVersionID != nil {
			item["form_version_id"] = r.FormVersionID.String()
		}
		if r.CancelledByKind != nil {
			item["cancelled_by"] = string(*r.CancelledByKind)
		}
		if r.CancelReason != nil {
			item["cancel_reason"] = *r.CancelReason
		}
		sensitive, err := p.s.openSensitive(ctx, r)
		if err != nil {
			return privacy.ExportSection{}, err
		}
		if sensitive != nil {
			item["answers_sensitive"] = sensitive
		}
		tickets, err := p.s.d.Store.TicketsOfRegistration(ctx, r.OrgID, r.ID)
		if err != nil {
			return privacy.ExportSection{}, err
		}
		ts := make([]map[string]any, 0, len(tickets))
		for _, t := range tickets {
			ts = append(ts, map[string]any{"version": t.Version, "status": string(t.Status), "issued_at": t.IssuedAt,
				"revoked_at": t.RevokedAt})
		}
		item["tickets"] = ts
		out.Items = append(out.Items, item)
	}
	return out, nil
}

// Erase implements privacy.Provider; idempotent (a second run deletes
// nothing). A registration holding a seat gives it back under its event's
// row lock and the waiting list moves up.
func (p privacyProvider) Erase(ctx context.Context, userID uuid.UUID) (privacy.EraseResult, error) {
	var n int
	err := p.s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		var err error
		if n, err = p.s.d.Store.DeleteStaffOfUser(ctx, userID); err != nil {
			return err
		}
		regs, err := p.s.d.Store.RegistrationsOfUser(ctx, userID)
		if err != nil {
			return err
		}
		for _, r := range regs {
			e, err := p.s.d.Store.LockEvent(ctx, r.EventID)
			if err != nil {
				return err
			}
			cur, err := p.s.d.Store.LockRegistration(ctx, e.OrgID, e.ID, r.ID)
			if err != nil {
				return err
			}
			if err := p.s.d.Store.DeleteRegistration(ctx, e.OrgID, cur.ID); err != nil {
				return err
			}
			n++
			if !cur.Status.Counted() {
				continue
			}
			if err := p.s.d.Store.ReleaseSeat(ctx, e.OrgID, e.ID); err != nil {
				return err
			}
			sens, err := p.s.sensitive(ctx, e)
			if err != nil {
				return err
			}
			if err := p.s.promote(ctx, e, sens, p.s.now()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return privacy.EraseResult{}, err
	}
	return privacy.EraseResult{Module: "events", Deleted: n, RetainedReason: privacyRetainedReason}, nil
}
