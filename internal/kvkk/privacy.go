package kvkk

import (
	"context"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/privacy"
)

// retainedReason is the legal basis of the request rows kept on erasure
// (02 §16.1: proof of the answer, dsr_record).
const retainedReason = "data_subject_requests: proof of the KVKK answer, kept 3 years after closing (dsr_record)"

// privacyProvider is kvkk's own KVKK provider (01 §3.3): the subject's
// requests. Export lists them; Erase deletes their export files, cancels
// the other open requests (nothing is left to answer) and keeps the rows
// as proof.
type privacyProvider struct{ s *service }

var _ privacy.Provider = privacyProvider{}

// Name implements privacy.Provider.
func (privacyProvider) Name() string { return "kvkk" }

// Export implements privacy.Provider.
func (p privacyProvider) Export(ctx context.Context, userID uuid.UUID) (privacy.ExportSection, error) {
	rows, err := p.s.d.Store.AllOf(ctx, userID)
	if err != nil {
		return privacy.ExportSection{}, err
	}
	out := privacy.ExportSection{Module: "kvkk", Items: make([]map[string]any, 0, len(rows))}
	for _, d := range rows {
		item := map[string]any{
			"dsr_id": d.ID.String(), "type": string(d.Type), "channel": string(d.Channel), "status": string(d.Status),
			"received_at": d.ReceivedAt.Format(time.RFC3339), "due_at": d.DueAt.Format(time.RFC3339),
		}
		if d.Details != "" {
			item["details"] = d.Details
		}
		if d.ResponseSummary != "" {
			item["response_summary"] = d.ResponseSummary
		}
		if d.RejectionReason != "" {
			item["rejection_reason"] = d.RejectionReason
		}
		if d.CompletedAt != nil {
			item["completed_at"] = d.CompletedAt.Format(time.RFC3339)
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}

// Erase implements privacy.Provider. It is idempotent: a second run finds
// no export file and no open request.
func (p privacyProvider) Erase(ctx context.Context, userID uuid.UUID) (privacy.EraseResult, error) {
	s := p.s
	res := privacy.EraseResult{Module: "kvkk"}
	rows, err := s.d.Store.AllOf(ctx, userID)
	if err != nil {
		return res, err
	}
	current := uuid.Nil()
	if sub, ok := privacy.SubjectFrom(ctx); ok {
		current = sub.DSRID
	}
	sys := authz.SystemActor(erasureJob)
	for _, row := range rows {
		if row.ExportFileID == nil && (!row.Status.Open() || row.ID == current) {
			continue
		}
		err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
			d, err := s.d.Store.Lock(ctx, row.ID)
			if err != nil {
				return err
			}
			meta := map[string]any{"step": "erasure"}
			fileID := d.ExportFileID
			if fileID != nil {
				d.ExportFileID = nil
				meta["file_id"] = fileID.String()
			}
			if d.Status.Open() && d.ID != current {
				d.Status = StatusCancelled
				meta["cancelled"] = true
			}
			if err := s.d.Store.Update(ctx, d); err != nil {
				return err
			}
			if fileID != nil {
				if err := s.d.Files.Delete(ctx, sys, *fileID); err != nil {
					return err
				}
				res.Deleted++
			}
			return s.d.Audit.Log(ctx, event(sys, authz.KvkkDsrComplete, d, meta))
		})
		if err != nil {
			return res, err
		}
	}
	if len(rows) > 0 {
		res.Retained, res.RetainedReason = len(rows), retainedReason
	}
	return res, nil
}
