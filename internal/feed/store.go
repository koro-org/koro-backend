package feed

import (
	"context"
	"time"
	"uuid"
)

// Store is the persistence boundary of feed (05 §2.2) over posts,
// post_media and feed_org_settings (02 §12.1–12.3), with read access to
// the reader tables BE-029 writes (org_follows, org_mutes, post_hides,
// post_reactions, post_saves). The GORM store (store_gorm.go) and the
// in-memory store (store_mem.go) pass the same contract test
// (store_contract_test.go).
//
// Every method works in the transaction carried by ctx, if any; the service
// draws the boundaries.
//
// Org scope: a post is addressed by its id (GET /posts/{post_id}); the
// service reads it with GetPost/LockPost, takes org_id from the row and
// passes that org to every later call (06 H-38). post_media has no org_id:
// ReplaceMedia writes it only through a posts row of the given org.
type Store interface {
	// InsertPost writes a new post and returns it as stored (created_at,
	// updated_at from the database).
	InsertPost(ctx context.Context, p Post) (Post, error)
	// GetPost returns the post or ErrNotFound.
	GetPost(ctx context.Context, id uuid.UUID) (Post, error)
	// LockPost is GetPost with the row locked for the transaction.
	LockPost(ctx context.Context, id uuid.UUID) (Post, error)
	// UpdatePost writes the mutable columns of p (scoped to p.OrgID) and
	// returns the stored row: updated_at changes on every call, so the ETag
	// does too (04 §1.13). ErrNotFound when the post is gone.
	UpdatePost(ctx context.Context, p Post) (Post, error)
	// DeletePost hard-deletes a post of orgID that was never published
	// (draft or pending_review) with its media (ON DELETE CASCADE).
	// ErrNotFound when no such post id belongs to orgID.
	DeletePost(ctx context.Context, orgID, id uuid.UUID) error

	// ReplaceMedia replaces the media of post id (of orgID) by media in
	// their order (position 0…). ErrNotFound when the post is not orgID's.
	ReplaceMedia(ctx context.Context, orgID, id uuid.UUID, media []Media) error
	// MediaOf returns the media of the posts, by post, in position order
	// (one statement for a page).
	MediaOf(ctx context.Context, postIDs []uuid.UUID) (map[uuid.UUID][]Media, error)

	// ListFeed returns up to limit published posts the reader f may see,
	// after the position after (nil: the first page), newest first by
	// (published_at, id) (02 §12.2 "Görünürlük", R-003 §1.5.2).
	ListFeed(ctx context.Context, f FeedFilter, after *After, limit int) ([]Post, error)
	// ListOrgPosts returns up to limit published posts of orgID the reader
	// f may see (the org page), newest first by (published_at, id). Mutes
	// and hidden posts are feed-only: they do not apply here.
	ListOrgPosts(ctx context.Context, orgID uuid.UUID, f Reader, after *After, limit int) ([]Post, error)
	// ListOrgManage returns up to limit posts of orgID in every status (or
	// in statuses when non-empty), newest first by id.
	ListOrgManage(ctx context.Context, orgID uuid.UUID, statuses []Status, after *uuid.UUID, limit int) ([]Post, error)

	// OrgSettings returns the feed settings of orgID; ok=false without a
	// row (the org is new, 02 §12.1).
	OrgSettings(ctx context.Context, orgID uuid.UUID) (OrgSettings, bool, error)
	// Follows reports whether userID follows orgID (org_follows).
	Follows(ctx context.Context, userID, orgID uuid.UUID) (bool, error)
	// ViewerState returns which of postIDs userID liked and saved and which
	// of orgIDs they follow (one statement for a page).
	ViewerState(ctx context.Context, userID uuid.UUID, postIDs, orgIDs []uuid.UUID) (ViewerState, error)

	// PostsOfAuthor returns every post authored by userID, oldest first
	// (KVKK export).
	PostsOfAuthor(ctx context.Context, userID uuid.UUID) ([]Post, error)
}

// Reader is who reads published posts (03 §3.6 "Gönderi"): what makes a
// post visible to them.
type Reader struct {
	// All: the reader sees every published post (a moderator, a platform
	// admin, a writer of the author org on the org page).
	All bool
	// UserID is the signed-in reader; nil for an anonymous one, who sees
	// public, non-commercial posts only (T-57).
	UserID *uuid.UUID
	// Keys are the reader's audience keys (students.ViewerKeys; 'all' at
	// least). A post is visible when every dimension of its keys has a
	// match (02 §9.1: OR within a dimension, AND across dimensions).
	Keys []string
	// Minor readers never see commercial posts (03 k15).
	Minor bool
	// MemberOrgs are the reader's active memberships: a member of the
	// author org counts as a follower of it (followers audience; the
	// "following" tab), sensitive clubs included, which never create an
	// automatic follow (02 §12.7).
	MemberOrgs []uuid.UUID
}

// FeedFilter narrows ListFeed.
type FeedFilter struct {
	Reader
	// Following keeps the posts of followed orgs (org_follows) and of the
	// reader's member orgs: the "Takip ettiklerim" tab.
	Following bool
	// Kind keeps one post kind when set.
	Kind Kind
}

// ViewerState is what a reader did on a page of posts.
type ViewerState struct {
	Liked     map[uuid.UUID]bool // post id
	Saved     map[uuid.UUID]bool // post id
	Following map[uuid.UUID]bool // org id
}

// After is the keyset position of a published page: the last row's
// published_at and id.
type After struct {
	At time.Time
	ID uuid.UUID
}
