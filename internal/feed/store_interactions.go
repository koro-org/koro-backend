package feed

import (
	"context"
	"uuid"
)

// interactionStore is the reader interactions (BE-029; 02 §12.4–12.8):
// post_reactions, post_saves, post_hides, org_follows and org_mutes
// (interact.go, relations.go, jobs.go and the viewer state of read.go).
//
// Every write is idempotent and reports whether it changed a row. A
// like or a save moves the post's counter in the same statement, only
// when its row was really inserted or deleted (so a repeated PUT or a
// race of two requests counts once), and a decrement never goes below
// zero. A counter update does not change posts.updated_at (the ETag).
type interactionStore interface {
	// Follows reports whether userID follows orgID (org_follows).
	Follows(ctx context.Context, userID, orgID uuid.UUID) (bool, error)
	// ViewerState returns which of postIDs userID liked and saved and which
	// of orgIDs they follow and muted (one statement for a page).
	ViewerState(ctx context.Context, userID uuid.UUID, postIDs, orgIDs []uuid.UUID) (ViewerState, error)

	// AddLike / RemoveLike write post_reactions and posts.like_count.
	AddLike(ctx context.Context, userID, postID uuid.UUID) (bool, error)
	RemoveLike(ctx context.Context, userID, postID uuid.UUID) (bool, error)
	// WithdrawnLikes returns the pairs among likes whose post exists and
	// holds no like of the user (talent's reconciliation, BE-090).
	WithdrawnLikes(ctx context.Context, likes []LikeKey) (map[LikeKey]bool, error)
	// AddSave / RemoveSave write post_saves and posts.save_count.
	AddSave(ctx context.Context, userID, postID uuid.UUID) (bool, error)
	RemoveSave(ctx context.Context, userID, postID uuid.UUID) (bool, error)
	// AddHide / RemoveHide write post_hides (BE-145: a hide can be undone).
	AddHide(ctx context.Context, userID, postID uuid.UUID) (bool, error)
	RemoveHide(ctx context.Context, userID, postID uuid.UUID) (bool, error)
	// ListSaved returns up to limit published posts saved by r.UserID that r
	// may still see (a post no longer visible is skipped), newest save
	// first by (created_at of the save, post_id), after the position after.
	ListSaved(ctx context.Context, r Reader, after *After, limit int) ([]MarkedPost, error)
	// ListHidden is ListSaved over the posts r.UserID hid (post_hides;
	// BE-145): a hidden post r may no longer see is skipped as well.
	ListHidden(ctx context.Context, r Reader, after *After, limit int) ([]MarkedPost, error)

	// FollowManual writes the user's own follow: a new row is manual, an
	// automatic one becomes manual (the user chose it); notify nil keeps
	// the current value (false for a new row). It reports whether the row
	// is new.
	FollowManual(ctx context.Context, userID, orgID uuid.UUID, notify *bool) (bool, error)
	// AddAutoFollow writes an auto_membership follow unless the user
	// already follows the org (any source).
	AddAutoFollow(ctx context.Context, userID, orgID uuid.UUID) (bool, error)
	// RemoveAutoFollow deletes the user's follow of orgID only when it is
	// auto_membership (a manual follow stays).
	RemoveAutoFollow(ctx context.Context, userID, orgID uuid.UUID) (bool, error)
	// RemoveAutoFollowsOf deletes every auto_membership follow of orgID (a
	// club that became sensitive) and returns the count.
	RemoveAutoFollowsOf(ctx context.Context, orgID uuid.UUID) (int, error)
	// RemoveFollow deletes the user's follow of orgID whatever its source.
	RemoveFollow(ctx context.Context, userID, orgID uuid.UUID) (bool, error)
	// ListFollows returns up to limit follows of userID, newest first by
	// (created_at, org_id), after the position after.
	ListFollows(ctx context.Context, userID uuid.UUID, after *After, limit int) ([]Follow, error)

	// AddMute / RemoveMute write org_mutes.
	AddMute(ctx context.Context, userID, orgID uuid.UUID) (bool, error)
	RemoveMute(ctx context.Context, userID, orgID uuid.UUID) (bool, error)
	// ListMutes returns up to limit mutes of userID, newest first by
	// (created_at, org_id), after the position after.
	ListMutes(ctx context.Context, userID uuid.UUID, after *After, limit int) ([]Mute, error)

	// ReconcileCounters sets like_count and save_count of every post whose
	// counter differs from its rows (01 §7 feed.reconcile_counters) and
	// returns how many posts it corrected. A drifted row is locked before
	// it is counted, so a like committing meanwhile is never lost.
	ReconcileCounters(ctx context.Context) (int, error)

	// InteractionsOf returns every like, save, hide, follow and mute of
	// userID (KVKK export).
	InteractionsOf(ctx context.Context, userID uuid.UUID) (Interactions, error)
	// EraseInteractions hard-deletes them (KVKK erase; the counters of the
	// liked and saved posts go down) and returns the number of rows.
	EraseInteractions(ctx context.Context, userID uuid.UUID) (int, error)
}

// ViewerState is what a reader did on a page of posts.
type ViewerState struct {
	Liked     map[uuid.UUID]bool // post id
	Saved     map[uuid.UUID]bool // post id
	Following map[uuid.UUID]bool // org id
	Muted     map[uuid.UUID]bool // org id (org_mutes; BE-122 listMyOrgRelations)
}

func newViewerState() ViewerState {
	return ViewerState{Liked: map[uuid.UUID]bool{}, Saved: map[uuid.UUID]bool{}, Following: map[uuid.UUID]bool{},
		Muted: map[uuid.UUID]bool{}}
}
