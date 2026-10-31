package orgs

import (
	"context"
	"errors"
	"log/slog"

	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/platform/problem"
)

// logoOwner is the files owner module of an org logo (org_logo).
const logoOwner = "orgs"

// DropRejectedLogoTaskName consumes file.rejected (01 §6.2, T-176): an org
// logo the scan rejected leaves the org and is deleted.
const DropRejectedLogoTaskName = "orgs.drop_rejected_logo"

// auditLogoRejected is the audit action of the system path (03 §4.1
// format; not an HTTP action).
const auditLogoRejected = "orgs.org.drop_rejected_logo"

// dropRejectedLogo is orgs.drop_rejected_logo (file.rejected, T-176;
// files.SubscribeRejected): the org whose logo the scan rejected (malware,
// or content that could not be cleaned) loses logo_file_id and the file is
// deleted through files, in one transaction. updated_at moves (the ETag
// with it). No org.updated is written: webcache's own file.rejected
// subscription already revalidates the org's page (BE-120), one request
// per rejection. A logo replaced or cleared meanwhile only loses its file;
// a second delivery deletes nothing.
func (s *service) dropRejectedLogo(ctx context.Context, r files.Rejection) error {
	if r.Purpose != files.PurposeOrgLogo {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "rejected orgs file of an unknown purpose kept",
			slog.String("file_id", r.FileID.String()), slog.String("purpose", string(r.Purpose)))
		return nil
	}
	if s.d.Files == nil {
		return problem.ErrUnavailable // retried: the logo file must not outlive its reference
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		o, err := s.orgs().LockOrg(ctx, r.EntityID)
		switch {
		case errors.Is(err, ErrNotFound):
			return s.d.Files.Delete(ctx, r.Actor, r.FileID)
		case err != nil:
			return err
		}
		if o.LogoFileID == nil || *o.LogoFileID != r.FileID {
			return s.d.Files.Delete(ctx, r.Actor, r.FileID)
		}
		o.LogoFileID = nil
		if _, err := s.orgs().UpdateOrg(ctx, o); err != nil {
			return err
		}
		if err := s.d.Files.Delete(ctx, r.Actor, r.FileID); err != nil {
			return err
		}
		return s.d.Audit.Log(ctx, auditEvent(r.Actor, authz.Action(auditLogoRejected), o.ID, "", r.Meta()))
	})
}
