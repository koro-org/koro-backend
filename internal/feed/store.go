package feed

import (
	"context"
	"time"
	"uuid"
)

// Store is the persistence boundary of feed (05 §2.2) over posts,
// post_media and feed_org_settings (02 §12.1–12.3) and the reader tables
// (post_reactions, post_saves, post_hides, org_follows, org_mutes; 02
// §12.4–12.8, BE-029). The GORM store (store_gorm.go) and the
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

	// ---- reader interactions (BE-029; 02 §12.4–12.8)
	//
	// Every write is idempotent and reports whether it changed a row. A
	// like or a save moves the post's counter in the same statement, only
	// when its row was really inserted or deleted (so a repeated PUT or a
	// race of two requests counts once), and a decrement never goes below
	// zero. A counter update does not change posts.updated_at (the ETag).

	// AddLike / RemoveLike write post_reactions and posts.like_count.
	AddLike(ctx context.Context, userID, postID uuid.UUID) (bool, error)
	RemoveLike(ctx context.Context, userID, postID uuid.UUID) (bool, error)
	// AddSave / RemoveSave write post_saves and posts.save_count.
	AddSave(ctx context.Context, userID, postID uuid.UUID) (bool, error)
	RemoveSave(ctx context.Context, userID, postID uuid.UUID) (bool, error)
	// AddHide writes post_hides (there is no unhide in v1).
	AddHide(ctx context.Context, userID, postID uuid.UUID) (bool, error)
	// ListSaved returns up to limit published posts saved by r.UserID that r
	// may still see (a post no longer visible is skipped), newest save
	// first by (saved_at, post_id), after the position after.
	ListSaved(ctx context.Context, r Reader, after *After, limit int) ([]SavedPost, error)

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

	// ---- reports, moderation and trust (BE-032; 02 §12.1, §12.9–12.10)
	//
	// Reports and moderation actions have no org scope: moderation is the
	// platform's (02 §12.9 "Org kapsamı: yok"). The service authorises the
	// admin surface before it reads them.

	// InsertReport writes r as an open report unless the reporter already
	// has an open (open or in_review) report on the target
	// (content_reports_open_uq): then it returns that report and
	// created=false. Concurrent duplicates end with one row.
	InsertReport(ctx context.Context, r Report) (Report, bool, error)
	// GetReport returns the report or ErrNotFound.
	GetReport(ctx context.Context, id uuid.UUID) (Report, error)
	// LockReport is GetReport with the row locked for the transaction.
	LockReport(ctx context.Context, id uuid.UUID) (Report, error)
	// UpdateReport writes the status, assigned_to, resolution_note and
	// resolved_at of r and returns the stored row (ErrNotFound when gone).
	UpdateReport(ctx context.Context, r Report) (Report, error)
	// ListReports returns up to limit reports of f by (created_at, id),
	// oldest first unless f.Newest, after the position after.
	ListReports(ctx context.Context, f ReportFilter, after *After, limit int) ([]Report, error)
	// OpenReporters counts the users with an open report on the target (one
	// open report per user and target: the distinct reporters).
	OpenReporters(ctx context.Context, t TargetType, targetID uuid.UUID) (int, error)
	// ReportsOf returns every report filed by userID, oldest first (KVKK
	// export).
	ReportsOf(ctx context.Context, userID uuid.UUID) ([]Report, error)
	// DeleteReportsOf hard-deletes them (KVKK erase; the moderation actions
	// keep their decision, report_id becomes NULL) and returns the count.
	DeleteReportsOf(ctx context.Context, userID uuid.UUID) (int, error)

	// InsertModerationAction appends m (moderation_actions is append-only)
	// and returns it as stored.
	InsertModerationAction(ctx context.Context, m ModerationAction) (ModerationAction, error)
	// GetModerationAction returns the action or ErrNotFound.
	GetModerationAction(ctx context.Context, id uuid.UUID) (ModerationAction, error)
	// ListModerationActions returns up to limit actions of f, newest first
	// by id, after the id after.
	ListModerationActions(ctx context.Context, f ActionFilter, after *uuid.UUID, limit int) ([]ModerationAction, error)
	// LatestActions returns the newest action on each target of type t
	// among ids (one statement for a page).
	LatestActions(ctx context.Context, t TargetType, ids []uuid.UUID) (map[uuid.UUID]ModerationAction, error)
	// ReportActions counts the actions taken with reportID.
	ReportActions(ctx context.Context, reportID uuid.UUID) (int, error)
	// ActionsBy returns the actions taken by userID, oldest first (KVKK
	// export of a moderator).
	ActionsBy(ctx context.Context, userID uuid.UUID) ([]ModerationAction, error)

	// ListReview returns up to limit posts in pending_review of every org,
	// oldest first by (created_at, id), after the position after (the
	// review queue, posts_review_idx).
	ListReview(ctx context.Context, after *After, limit int) ([]Post, error)

	// AddApprovedPost counts one more approved post of orgID (a new
	// settings row when there was none) and returns the row.
	AddApprovedPost(ctx context.Context, orgID uuid.UUID) (OrgSettings, error)
	// PromoteTrusted makes orgID trusted when it is new and has at least
	// threshold approved posts (feed.org_settings.auto_trust) and reports
	// whether it changed.
	PromoteTrusted(ctx context.Context, orgID uuid.UUID, threshold int) (bool, error)
	// SetTrust writes the trust level, restricted_until, reason and
	// updated_by of s (a new row when there was none; the approved post
	// count stays) and returns the row.
	SetTrust(ctx context.Context, s OrgSettings) (OrgSettings, error)
}

// ReportFilter narrows ListReports (adminListReports).
type ReportFilter struct {
	Statuses   []ReportStatus // empty: every status
	Reason     ReportReason   // "": every reason
	TargetType TargetType     // "": both
	Newest     bool           // newest first (sort=-created_at)
}

// ActionFilter narrows ListModerationActions (adminListModerationActions).
type ActionFilter struct {
	TargetType TargetType // "": both
	TargetID   *uuid.UUID // nil: every target
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
