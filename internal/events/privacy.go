package events

import (
	"context"
	"uuid"

	"koro/internal/platform/privacy"
)

// privacyRetainedReason explains what Erase leaves in place.
const privacyRetainedReason = "events rows hold no personal data (02 §11.1): created_by and attendance_finalized_by " +
	"reference the account, which identity anonymises, as do the form columns updated_by, published_by and " +
	"lint_override_by (02 §11.3-11.5), a registration's approved_by and a check-in's staff_user_id and voided_by " +
	"(the person's scans of other attendees stay with those attendees' records); the person's own check-ins go " +
	"with their registration (ON DELETE CASCADE). The registration exports the person requested as an organiser " +
	"(02 §11.9) stay as the organisation's accountability record for 2 years (event_export_log) without the IP " +
	"address; requested_by references the anonymised account. An export file holding the person as an attendee " +
	"lives at most 7 days (events.purge_exports)"

// privacyProvider is the KVKK provider of events (01 §3.3): a person's
// staff assignments (02 §11.2), registrations with their answers (the
// special-category ones opened for the data subject) and consent snapshot,
// tickets and check-ins (02 §11.6–11.8; BE-031: the check-ins of their
// registrations and the scans they made as staff). Export lists them;
// Erase deletes them (the tickets and check-ins go with their
// registration, a held seat is released and given to the waiting list).
// BE-050: the registration exports the person requested (02 §11.9:
// event, reason, fields, status, IP, downloads) are exported; Erase removes
// their IP address and keeps the rows (the organisation's accountability
// record, event_export_log).
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
		checkins, err := p.s.d.Store.CheckinsOfRegistration(ctx, r.OrgID, r.ID)
		if err != nil {
			return privacy.ExportSection{}, err
		}
		cs := make([]map[string]any, 0, len(checkins))
		for _, c := range checkins {
			cs = append(cs, map[string]any{"checkin_id": c.ID.String(), "method": string(c.Method), "scanned_at": c.ScannedAt,
				"voided_at": c.VoidedAt})
		}
		item["checkins"] = cs
		out.Items = append(out.Items, item)
	}
	// The scans the person made as event staff (their own activity; the
	// attendees appear by registration id only).
	scans, err := p.s.d.Store.CheckinsByStaff(ctx, userID)
	if err != nil {
		return privacy.ExportSection{}, err
	}
	for _, c := range scans {
		out.Items = append(out.Items, map[string]any{
			"type": "event_checkin_scanned", "checkin_id": c.ID.String(), "event_id": c.EventID.String(),
			"method": string(c.Method), "scanned_at": c.ScannedAt, "voided": c.VoidedAt != nil,
		})
	}
	// The registration exports the person requested (BE-050); never the
	// file (other people's data).
	exports, err := p.s.d.Store.ExportsOfUser(ctx, userID)
	if err != nil {
		return privacy.ExportSection{}, err
	}
	for _, x := range exports {
		item := map[string]any{
			"type": "event_data_export", "export_id": x.ID.String(), "event_id": x.EventID.String(), "org_id": x.OrgID.String(),
			"reason": x.Reason, "fields": []string(x.Fields), "format": string(x.Format), "status": string(x.Status),
			"row_count": x.RowCount, "download_count": x.DownloadCount, "first_downloaded_at": x.FirstDownloadedAt,
			"expires_at": x.ExpiresAt, "purged_at": x.PurgedAt, "created_at": x.CreatedAt,
		}
		if x.IP != nil {
			item["ip"] = *x.IP
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}

// Erase implements privacy.Provider; idempotent (a second run deletes
// nothing). A registration holding a seat gives it back under its event's
// row lock and the waiting list moves up.
func (p privacyProvider) Erase(ctx context.Context, userID uuid.UUID) (privacy.EraseResult, error) {
	var n, anon int
	err := p.s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		var err error
		if n, err = p.s.d.Store.DeleteStaffOfUser(ctx, userID); err != nil {
			return err
		}
		if anon, err = p.s.d.Store.ClearExportIPs(ctx, userID); err != nil {
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
	return privacy.EraseResult{Module: "events", Deleted: n, Anonymized: anon, RetainedReason: privacyRetainedReason}, nil
}
