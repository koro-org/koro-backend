package events

import (
	"context"
	"errors"
	"log/slog"

	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/platform/problem"
)

// coverOwner is the files owner module of an event cover (event_cover) and
// of a registration export (event_export).
const coverOwner = "events"

// DropRejectedCoverTaskName consumes file.rejected (01 §6.2, T-176): an
// event cover the scan rejected leaves the event and is deleted.
const DropRejectedCoverTaskName = "events.drop_rejected_cover"

// auditCoverRejected is the audit action of the system path (03 §4.1
// format; not an HTTP action).
const auditCoverRejected = "events.event.drop_rejected_cover"

// dropRejectedCover is events.drop_rejected_cover (file.rejected, T-176;
// files.SubscribeRejected): the event whose cover the scan rejected
// (malware, or content that could not be cleaned) loses cover_file_id and
// the file is deleted through files, in one transaction, whatever the
// event's status. updated_at moves (the ETag with it). No event.updated is
// written: webcache's own file.rejected subscription already revalidates
// the event's pages (BE-120), one request per rejection, and the cover is
// no field the registrants hear about. A cover replaced meanwhile only
// loses its file; a second delivery deletes nothing. A registration export
// is server-generated and never scanned, so it is never rejected.
func (s *service) dropRejectedCover(ctx context.Context, r files.Rejection) error {
	if r.Purpose != files.PurposeEventCover {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "rejected events file of an unexpected purpose kept",
			slog.String("file_id", r.FileID.String()), slog.String("purpose", string(r.Purpose)))
		return nil
	}
	if s.d.Files == nil {
		return problem.ErrUnavailable // retried: the cover file must not outlive its reference
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		e, err := s.d.Store.LockEvent(ctx, r.EntityID)
		switch {
		case errors.Is(err, ErrNotFound):
			return s.d.Files.Delete(ctx, r.Actor, r.FileID)
		case err != nil:
			return err
		}
		if e.CoverFileID == nil || *e.CoverFileID != r.FileID {
			return s.d.Files.Delete(ctx, r.Actor, r.FileID)
		}
		e.CoverFileID = nil
		// org_id from the locked events row (06 H-38).
		if _, err := s.d.Store.UpdateEvent(ctx, e); err != nil {
			return err
		}
		if err := s.d.Files.Delete(ctx, r.Actor, r.FileID); err != nil {
			return err
		}
		meta := r.Meta()
		meta["status"] = string(e.Status)
		return s.d.Audit.Log(ctx, auditEvent(r.Actor, authz.Action(auditCoverRejected), eventAggType, e.ID, e.OrgID, "", meta))
	})
}
