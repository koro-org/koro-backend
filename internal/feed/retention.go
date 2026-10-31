package feed

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/db"
	"koro/internal/platform/privacy"
)

// Retention (02 §12, §24.2; BE-057, the feed.purge_removed row of 01 §7):
// kvkk.retention_sweep calls the sweeper once per policy; an unapproved
// policy is a dry run (count only, inside a transaction kvkk rolls back),
// an approved one deletes in batches of RetentionBatch rows, one
// transaction each, oldest first.
//
//	post_removed       a removed post goes 12 months after removed_at with
//	                   its media rows, likes, saves and hides (ON DELETE
//	                   CASCADE) and its image files (01 §6.3 feed →
//	                   files.Files.Delete, same transaction; T-247). A post a
//	                   dispatch or a contact request still refers to waits
//	                   until that record is gone (T-152 option a).
//	post_media_dropped an image an edit took off a published post
//	                   (post_dropped_media, BE-158) goes 12 months after it
//	                   was dropped with its file, unless a report on the
//	                   post is still open or in review (the moderators'
//	                   evidence waits). A removed post's purge takes its
//	                   dropped images too.
//	content_report     a resolved report (actioned / dismissed) goes 12
//	                   months after resolved_at with its snapshot of the
//	                   reported post (BE-160); its moderation actions keep
//	                   their decision, report_id becomes NULL.
//	moderation_action  a decision goes 2 years after it was taken, through
//	                   000121's moderation_actions_purge (append-only table).

// Retention policy codes feed sweeps.
const (
	PolicyPostRemoved      = "post_removed"
	PolicyMediaDropped     = "post_media_dropped"
	PolicyContentReport    = "content_report"
	PolicyModerationAction = "moderation_action"

	// RetentionBatch is the most rows (posts, reports, actions) one purge
	// transaction deletes.
	RetentionBatch = 1000

	retentionJob = "feed.retention"
)

var retentionTables = map[string]string{
	PolicyPostRemoved:      "posts",
	PolicyMediaDropped:     "post_dropped_media",
	PolicyContentReport:    "content_reports",
	PolicyModerationAction: "moderation_actions",
}

// RetentionStore is what the sweeper needs from the store (BE-057).
type RetentionStore interface {
	// RemovedPostsDue returns up to limit posts removed before cutoff that
	// no dispatch or contact request refers to, oldest first, with their
	// media, locked for the transaction (rows locked elsewhere are
	// skipped). CountRemovedPostsDue counts them without a limit.
	RemovedPostsDue(ctx context.Context, cutoff time.Time, limit int) ([]Post, error)
	CountRemovedPostsDue(ctx context.Context, cutoff time.Time) (int, error)
	// PurgePosts hard-deletes the posts (their media rows, dropped image
	// rows and reader rows go by ON DELETE CASCADE) and returns how many.
	PurgePosts(ctx context.Context, ids []uuid.UUID) (int, error)
	// DroppedMediaDue returns up to limit images dropped off a post before
	// cutoff whose post has no report open or in review, oldest first,
	// locked for the transaction (rows locked elsewhere are skipped).
	// CountDroppedMediaDue counts them without a limit (BE-158).
	DroppedMediaDue(ctx context.Context, cutoff time.Time, limit int) ([]DroppedMedia, error)
	CountDroppedMediaDue(ctx context.Context, cutoff time.Time) (int, error)
	// PurgeResolvedReports deletes up to limit reports resolved before
	// cutoff, oldest first (their actions' report_id becomes NULL); dryRun
	// counts them and deletes nothing.
	PurgeResolvedReports(ctx context.Context, cutoff time.Time, limit int, dryRun bool) (int, error)
	// PurgeModerationActions deletes up to limit actions taken before
	// cutoff, oldest first; dryRun counts. The GORM store goes through
	// 000121's moderation_actions_purge: it never passes now − the policy's
	// period and refuses a real purge of an unapproved policy (55000).
	PurgeModerationActions(ctx context.Context, cutoff time.Time, limit int, dryRun bool) (int, error)
}

// errNoFiles: removed posts and dropped images cannot go without the files
// module.
var errNoFiles = errors.New("feed: post images cannot be purged: files not wired")

// sweeper is feed's privacy.RetentionSweeper: it needs the purges of the
// store, the media of the removed posts (their image files go too), the
// files module and the transaction manager only.
type sweeper struct {
	retention RetentionStore
	media     mediaStore
	files     Files
	tx        db.TxManager
}

var _ privacy.RetentionSweeper = sweeper{}

// PolicyCodes implements privacy.RetentionSweeper.
func (sweeper) PolicyCodes() []string {
	out := make([]string, 0, len(retentionTables))
	for code := range retentionTables {
		out = append(out, code)
	}
	slices.Sort(out)
	return out
}

// Sweep implements privacy.RetentionSweeper.
func (w sweeper) Sweep(ctx context.Context, p privacy.RetentionPolicy, now time.Time) (privacy.SweepResult, error) {
	table, ok := retentionTables[p.Code]
	if !ok {
		return privacy.SweepResult{}, fmt.Errorf("feed: no retention sweep for policy %q", p.Code)
	}
	cutoff := now.Add(-p.Retention)
	var purge func(ctx context.Context, limit int, dry bool) (int, error)
	switch p.Code {
	case PolicyPostRemoved:
		purge = func(ctx context.Context, limit int, dry bool) (int, error) {
			return w.purgeRemovedPosts(ctx, cutoff, limit, dry)
		}
	case PolicyMediaDropped:
		purge = func(ctx context.Context, limit int, dry bool) (int, error) {
			return w.purgeDroppedMedia(ctx, cutoff, limit, dry)
		}
	case PolicyContentReport:
		purge = func(ctx context.Context, limit int, dry bool) (int, error) {
			return w.retention.PurgeResolvedReports(ctx, cutoff, limit, dry)
		}
	default:
		purge = func(ctx context.Context, limit int, dry bool) (int, error) {
			return w.retention.PurgeModerationActions(ctx, cutoff, limit, dry)
		}
	}
	res := privacy.SweepResult{PolicyCode: p.Code, Table: table, Method: privacy.MethodHardDelete}
	if !p.LegalApproved {
		n, err := purge(ctx, 0, true)
		res.Count = n
		return res, err
	}
	n, err := db.Batches(ctx, w.tx, RetentionBatch, func(ctx context.Context) (int, error) {
		return purge(ctx, RetentionBatch, false)
	})
	res.Count = n
	return res, err
}

// purgeRemovedPosts handles one batch of post_removed: the posts go, then
// their image files, shown and dropped (the rows that referenced them went
// with the posts), in the transaction carried by ctx.
func (w sweeper) purgeRemovedPosts(ctx context.Context, cutoff time.Time, limit int, dry bool) (int, error) {
	if dry {
		return w.retention.CountRemovedPostsDue(ctx, cutoff)
	}
	posts, err := w.retention.RemovedPostsDue(ctx, cutoff, limit)
	if err != nil || len(posts) == 0 {
		return 0, err
	}
	ids := make([]uuid.UUID, len(posts))
	for i, p := range posts {
		ids[i] = p.ID
	}
	media, err := w.media.MediaOf(ctx, ids)
	if err != nil {
		return 0, err
	}
	dropped, err := w.media.DroppedMediaOf(ctx, ids)
	if err != nil {
		return 0, err
	}
	n, err := w.retention.PurgePosts(ctx, ids)
	if err != nil {
		return 0, err
	}
	var gone []uuid.UUID
	for _, id := range ids {
		gone = append(gone, mediaFiles(media[id])...)
		gone = append(gone, droppedFiles(dropped[id])...)
	}
	return n, w.deleteFiles(ctx, gone)
}

// purgeDroppedMedia handles one batch of post_media_dropped (BE-158): the
// dropped rows go, then their files, in the transaction carried by ctx.
func (w sweeper) purgeDroppedMedia(ctx context.Context, cutoff time.Time, limit int, dry bool) (int, error) {
	if dry {
		return w.retention.CountDroppedMediaDue(ctx, cutoff)
	}
	due, err := w.retention.DroppedMediaDue(ctx, cutoff, limit)
	if err != nil || len(due) == 0 {
		return 0, err
	}
	ids := droppedFiles(due)
	n, err := w.media.ForgetDroppedMedia(ctx, ids)
	if err != nil {
		return 0, err
	}
	return n, w.deleteFiles(ctx, ids)
}

// deleteFiles deletes the image files of the purge as the retention job.
func (w sweeper) deleteFiles(ctx context.Context, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	if w.files == nil {
		return errNoFiles
	}
	sys := authz.SystemActor(retentionJob)
	for _, id := range ids {
		if err := w.files.Delete(ctx, sys, id); err != nil {
			return err
		}
	}
	return nil
}
