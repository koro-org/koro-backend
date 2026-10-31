package feed

import (
	"context"
	"uuid"
)

// mediaStore is post_media (02 §12.3), the images of a post: written with
// the post (write.go), read with a page (read.go) and before a removed
// post is purged (retention.go); and post_dropped_media (BE-158), the
// images an edit took off a published post, kept for the moderators until
// their retention. Neither table has an org_id: see Store.
type mediaStore interface {
	// ReplaceMedia replaces the media of post id (of orgID) by media in
	// their order (position 0…). ErrNotFound when the post is not orgID's.
	ReplaceMedia(ctx context.Context, orgID, id uuid.UUID, media []Media) error
	// MediaOf returns the media of the posts, by post, in position order
	// (one statement for a page).
	MediaOf(ctx context.Context, postIDs []uuid.UUID) (map[uuid.UUID][]Media, error)
	// DropMedia records the images dropped off post id (of orgID; their
	// PostID is set from it). ErrNotFound when the post is not orgID's.
	DropMedia(ctx context.Context, orgID, id uuid.UUID, dropped []DroppedMedia) error
	// DroppedMediaOf returns the dropped images of the posts, by post,
	// oldest first.
	DroppedMediaOf(ctx context.Context, postIDs []uuid.UUID) (map[uuid.UUID][]DroppedMedia, error)
	// ForgetDroppedMedia deletes the dropped rows of the files (an image
	// added back to its post, rejected by the scan, or past its retention)
	// and returns how many went; a file without one is no error.
	ForgetDroppedMedia(ctx context.Context, fileIDs []uuid.UUID) (int, error)
}
