package feed

import (
	"context"
	"slices"
	"strings"
	"time"
	"uuid"
)

// postStore is the posts aggregate (02 §12.2): the post rows, their reads
// by id and the published and manage lists (write.go, read.go,
// targeted.go). Org scope: see Store.
type postStore interface {
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

	// ListFeed returns up to limit published posts the reader f may see,
	// after the position after (nil: the first page), newest first by
	// (published_at, id) (02 §12.2 "Görünürlük", R-003 §1.5.2).
	ListFeed(ctx context.Context, f FeedFilter, after *After, limit int) ([]Post, error)
	// ListOrgPosts returns up to limit published posts of orgID the reader
	// f may see (the org page) of f.Kinds, newest first by (published_at,
	// id). Mutes and hidden posts are feed-only: they do not apply here.
	ListOrgPosts(ctx context.Context, orgID uuid.UUID, f OrgPostFilter, after *After, limit int) ([]Post, error)
	// ListEventShares returns up to limit published posts sharing one of
	// eventIDs (event_share) the reader r may see, newest first by
	// (published_at, id): the post pages that show the events' summaries
	// (webcache: one event, BE-150; an organiser's events, BE-157). No
	// event → no post.
	ListEventShares(ctx context.Context, eventIDs []uuid.UUID, r Reader, limit int) ([]Post, error)
	// ListOrgManage returns up to limit posts of orgID matching f, newest
	// first by id.
	ListOrgManage(ctx context.Context, orgID uuid.UUID, f ManageFilter, after *uuid.UUID, limit int) ([]Post, error)

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
	// Kinds keeps these post kinds (none: every kind; BE-191).
	Kinds Kinds
}

// OrgPostFilter narrows ListOrgPosts (BE-191).
type OrgPostFilter struct {
	Reader
	// Kinds keeps these post kinds (none: every kind).
	Kinds Kinds
}

// Kinds is the kind filter of the published lists (getFeed, listPosts;
// BE-191): a post of one of the kinds passes; none passes every kind.
type Kinds []Kind

// KindsOf returns the canonical filter of raw (the kind query values):
// sorted, a repeated kind once, so the same set in any order is the same
// filter (and the same cursor fingerprint, String).
func KindsOf(raw []string) Kinds {
	if len(raw) == 0 {
		return nil
	}
	out := make(Kinds, len(raw))
	for i, k := range raw {
		out[i] = Kind(k)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Valid reports whether every kind of ks is a post kind.
func (ks Kinds) Valid() bool {
	for _, k := range ks {
		if !k.Valid() {
			return false
		}
	}
	return true
}

// keeps reports whether a post of kind k passes the filter.
func (ks Kinds) keeps(k Kind) bool { return len(ks) == 0 || slices.Contains(ks, k) }

// strings returns the kinds as the values of an SQL IN list.
func (ks Kinds) strings() []string {
	out := make([]string, len(ks))
	for i, k := range ks {
		out[i] = string(k)
	}
	return out
}

// String is the filter in the cursor fingerprint: the kinds in their
// order, comma separated ("" for none; one kind is the kind itself, as
// before BE-191, so the cursors of a single kind stay valid).
func (ks Kinds) String() string { return strings.Join(ks.strings(), ",") }

// After is the keyset position of a published page: the last row's
// published_at and id.
type After struct {
	At time.Time
	ID uuid.UUID
}

// ManageFilter narrows ListOrgManage: every status (or Statuses when
// non-empty) and every kind (or Kind when set; BE-127: a company's
// published opportunity calls for createContactRequest).
type ManageFilter struct {
	Statuses []Status
	Kind     Kind
}
