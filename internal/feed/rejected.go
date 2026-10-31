package feed

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"uuid"

	"koro/internal/audit"
	"koro/internal/files"
)

// mediaOwner is the files owner module of a post image (post_media).
const mediaOwner = "feed"

// DropRejectedMediaTaskName consumes file.rejected (01 §6.2, T-176): a post
// image the scan rejected leaves its post and is deleted.
const DropRejectedMediaTaskName = "feed.drop_rejected_media"

// auditMediaRejected is the audit action of the system path (03 §4.1
// format; not an HTTP action).
const auditMediaRejected = "feed.post.drop_rejected_media"

// dropRejectedMedia is feed.drop_rejected_media (file.rejected, T-176;
// files.SubscribeRejected): the post_media row of an image the scan
// rejected (malware, or content that could not be cleaned) goes, the other
// images keep their order, and the file is deleted through files, in one
// transaction whatever the post's status (a draft can then be published,
// a restored post comes back without it). The post's updated_at moves, so
// its ETag does: an edit started on the old images is refused with 412.
// edited_at stays (the author did not edit) and no post.changed is written:
// webcache's own file.rejected subscription already revalidates a public
// post's page (BE-150), one request per rejection. A post that is gone or
// no longer shows the image (purged, replaced) only loses the file — and
// its dropped row, when an edit kept it for the moderators (BE-158); a
// second delivery finds no file and deletes nothing.
func (s *service) dropRejectedMedia(ctx context.Context, r files.Rejection) error {
	if r.Purpose != files.PurposePostMedia {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "rejected feed file of an unknown purpose kept",
			slog.String("file_id", r.FileID.String()), slog.String("purpose", string(r.Purpose)))
		return nil
	}
	if s.d.Files == nil {
		return errNoFiles
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		p, err := s.posts().LockPost(ctx, r.EntityID)
		switch {
		case errors.Is(err, ErrNotFound):
			return s.d.Files.Delete(ctx, r.Actor, r.FileID)
		case err != nil:
			return err
		}
		media, err := s.media().MediaOf(ctx, []uuid.UUID{p.ID})
		if err != nil {
			return err
		}
		shown := media[p.ID]
		rest := slices.DeleteFunc(slices.Clone(shown), func(m Media) bool { return m.FileID == r.FileID })
		if len(rest) == len(shown) {
			// Not shown: maybe an image an edit dropped off the published
			// post and kept for the moderators (BE-158); its row goes first
			// (post_dropped_media_file_id_fkey).
			if _, err := s.media().ForgetDroppedMedia(ctx, []uuid.UUID{r.FileID}); err != nil {
				return err
			}
			return s.d.Files.Delete(ctx, r.Actor, r.FileID)
		}
		// org_id from the locked posts row (06 H-38).
		if err := s.media().ReplaceMedia(ctx, p.OrgID, p.ID, rest); err != nil {
			return err
		}
		if _, err := s.posts().UpdatePost(ctx, p); err != nil {
			return err
		}
		if err := s.d.Files.Delete(ctx, r.Actor, r.FileID); err != nil {
			return err
		}
		meta := r.Meta()
		meta["status"], meta["media_left"] = string(p.Status), len(rest)
		id, org := p.ID, p.OrgID
		return s.d.Audit.Log(ctx, audit.Event{
			Action: auditMediaRejected, Outcome: audit.Success, OrgID: &org, ResourceType: postAggType, ResourceID: &id,
			Metadata: meta,
		}.By(r.Actor))
	})
}
