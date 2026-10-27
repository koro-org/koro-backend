package feed

import (
	"context"
	"uuid"
)

// mediaStore is post_media (02 §12.3), the images of a post: written with
// the post (write.go), read with a page (read.go) and before a removed
// post is purged (retention.go). post_media has no org_id: see Store.
type mediaStore interface {
	// ReplaceMedia replaces the media of post id (of orgID) by media in
	// their order (position 0…). ErrNotFound when the post is not orgID's.
	ReplaceMedia(ctx context.Context, orgID, id uuid.UUID, media []Media) error
	// MediaOf returns the media of the posts, by post, in position order
	// (one statement for a page).
	MediaOf(ctx context.Context, postIDs []uuid.UUID) (map[uuid.UUID][]Media, error)
}
