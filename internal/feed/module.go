// Package feed is the feed module (01 §4.14; KARARLAR-TOPLANTI-2 #1):
// clubs, universities, companies and the platform post to one feed.
// BE-027 builds the core: posts of four kinds with a validated Tiptap body
// and images, the draft → (pending_review →) published life cycle with the
// pre-moderation of new orgs, the feed itself (fan-out on read, keyset
// pagination) and the org page. BE-029 adds the reader's interactions:
// likes, saves and hides of posts, follows and mutes of orgs, the
// automatic follow of a membership and the hourly counter reconciliation.
// BE-032 adds targeted posts (a compiled segment audience, a company's
// targeted_post quota), user reports and platform moderation (the review
// queue, decisions on posts and orgs, the trust levels).
//
// Rules the package enforces:
//
//   - Org isolation (03 §3.6, §7.6, 06 H-38): a post is addressed by its id;
//     its org_id comes from the row, never from the request, and every later
//     read and write is scoped to that org. A post of another org the caller
//     may not see is 404 (a draft of club_b for club_a's admin), a visible
//     one without the role 403. post_media rows are written only through a
//     posts row of the org.
//   - Visibility (03 §3.6 "Gönderi", 02 §12.2): a published post is the
//     reader's when every dimension of its audience keys matches the
//     reader's keys (students.ViewerKeys; 'all' for everyone), a followers
//     post needs a follow of (or a membership in) the author org, a minor
//     never sees a commercial post (k15), an anonymous reader sees public
//     non-commercial posts on the org page only (T-57). Drafts, posts in
//     review and hidden posts are the author org's writers', moderators'
//     and platform admins'; removed posts are moderators' and platform
//     admins' (the author org keeps their status in its management list).
//   - Writers (03 §5.7 S cells): the author org's club_admin,
//     university_rep (and career_staff: content and events, 03 §1.2),
//     recruiter, company_admin and company_owner; platform_admin for the
//     platform org only (k-po). A suspended org's members cannot write on
//     its behalf (403 orgs.org_suspended), an unverified company cannot
//     (403 orgs.org_not_verified), a closed org takes nothing.
//   - Commercial label (03 §5.7, 02 posts_company_commercial_chk): every
//     company post is commercial whatever the request says.
//   - Rich text (05 §14.6, R-003 §1.5.4): the body is Tiptap JSON checked
//     against a closed node / mark / attribute allow-list; unknown content
//     is 422, an unsafe link loses its mark, raw HTML is never accepted.
//     body_text (preview, search) is derived on the server.
//   - Pre-moderation (02 §12.1, T-15): an org whose feed trust level is new
//     (no settings row) with fewer approved posts than the threshold, or
//     restricted, publishes into pending_review (post.submitted_for_review,
//     A); a trusted org publishes at once (post.published, A); a suspended
//     org cannot publish (403 feed.posting_suspended). The platform org is
//     always trusted. Images must be cleared by the virus scan (409
//     files.not_clean); a shared event must be a published public event (a
//     sensitive event only by its own org).
//   - A published post keeps its kind, audience and commercial label (409
//     feed.published_immutable); an edit stamps edited_at. A post that was
//     never published is deleted for good; a published one is removed.
//   - Statistics (04 §2.7): like and save counts only, never who liked or
//     saved (03 Y7); dispatch counts come with targeted posts (BE-032/BE-035,
//     k ≥ 10).
//   - Interactions (02 §12.4–12.8, 04 §F.3, §F.5): a like, a save or a hide
//     needs a published post the reader may see (404 otherwise); PUT and
//     DELETE are idempotent and a counter moves only with its row, so a
//     repeated or concurrent like counts once. Likes, saves, hides,
//     follows and mutes are the reader's personal data: the author org
//     sees the counters only, never who (03 Y7). A mute and a hide take
//     posts out of the feed, not out of the org page. A membership follows
//     its org automatically (auto_membership) except in a sensitive club,
//     whose follow would reveal the membership (02 §12.7); the end of the
//     membership removes the automatic follow only.
//   - Targeted posts (01 §6.3, 03 k16, 04 §F.1): the audience is compiled
//     by outreach (scope, k ≥ 10, no sensitive club, no minor) and, for a
//     company, one targeted_post unit is consumed, in the transaction that
//     writes the draft; an Idempotency-Key replay neither compiles nor
//     consumes again. The audience is fixed at creation; deleting (or a
//     moderator rejecting) a draft gives the unit back. A club addresses
//     its own members (club:<id>, a sensitive club too) without k ≥ 10 and
//     without a quota; only its members see such a post.
//   - Reports (02 §12.9, 04 §F.4): one open report per user and target; a
//     repeated report returns the open one. The reporter's identity leaves
//     the module only through the admin report endpoints (03 Y7), never in
//     an org's audit trail. A published post reported by enough distinct
//     users is hidden until a moderator decides (R-003 §1.5.3).
//   - Moderation (02 §12.10, 03 §5.7): moderators and platform admins work
//     the review queue and the reports; every decision is an append-only
//     moderation_actions row and an audit record, and the author org is
//     notified with the reason code (the way to appeal: support, then a
//     restore). Nobody moderates a post or an org they write for, and only
//     a platform admin suspends an org or lifts a suspension (k17).
//   - Personal data: posts carry the author's user ID (audit), never shown
//     to readers; logs carry IDs only.
package feed

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/outbox"
	"koro/internal/platform/privacy"
	"koro/internal/platform/queue"
)

// DefaultReviewThreshold is the number of approved posts after which a new
// org publishes without review (T-15: "ilk 3 gönderi").
const DefaultReviewThreshold = 3

// DefaultReportHideThreshold is the number of distinct users whose open
// reports hide a published post until a moderator decides (R-003 §1.5.3:
// "≥3 farklı kullanıcı bildirirse").
const DefaultReportHideThreshold = 3

// Posts is the read contract of other modules (01 §4.14).
type Posts interface {
	// Get returns the summary of a post (any status) or ErrNotFound.
	Get(ctx context.Context, postID uuid.UUID) (PostSummary, error)
}

// Service is the feed service: the cross-module contract plus the HTTP use
// cases of BE-027 and BE-029 (04 §2.7). Every method that takes an actor calls
// authz.Can first (05 §5.3).
type Service interface {
	Posts

	// GetFeed is getFeed (feed.feed.read): the reader's feed.
	GetFeed(ctx context.Context, a authz.Actor, q FeedQuery) (PostPage, error)
	// ListPosts is listPosts (feed.post.list_by_org, P): an org's page.
	ListPosts(ctx context.Context, a authz.Actor, orgID uuid.UUID, q PageQuery) (PostPage, error)
	// GetPost is getPost (feed.post.read, P).
	GetPost(ctx context.Context, a authz.Actor, id uuid.UUID) (PostView, error)
	// ListOrgPosts is listOrgPostsManage (feed.post.list_org_manage): every
	// post of the org, drafts included.
	ListOrgPosts(ctx context.Context, a authz.Actor, orgID uuid.UUID, q ManageQuery) (PostPage, error)
	// CreatePost is createPost (feed.post.create): a public or followers
	// draft.
	CreatePost(ctx context.Context, a authz.Actor, orgID uuid.UUID, in PostInput) (PostView, error)
	// UpdatePost is updatePost (feed.post.update): ifMatch is the If-Match
	// header (04 §1.13: missing → 428, stale → 412).
	UpdatePost(ctx context.Context, a authz.Actor, id uuid.UUID, ifMatch string, p PostPatch) (PostView, error)
	// PublishPost is publishPost (feed.post.publish): draft → published or
	// pending_review.
	PublishPost(ctx context.Context, a authz.Actor, id uuid.UUID) (PostView, error)
	// DeletePost is deletePost (feed.post.delete): a post never published
	// is deleted, a published one removed.
	DeletePost(ctx context.Context, a authz.Actor, id uuid.UUID) error
	// PostStats is getPostStats (feed.post.stats).
	PostStats(ctx context.Context, a authz.Actor, id uuid.UUID) (Stats, error)

	// LikePost / UnlikePost are likePost / unlikePost (feed.reaction.manage).
	LikePost(ctx context.Context, a authz.Actor, postID uuid.UUID) error
	UnlikePost(ctx context.Context, a authz.Actor, postID uuid.UUID) error
	// SavePost / UnsavePost are savePost / unsavePost (feed.save.manage).
	SavePost(ctx context.Context, a authz.Actor, postID uuid.UUID) error
	UnsavePost(ctx context.Context, a authz.Actor, postID uuid.UUID) error
	// HidePost is hidePost (feed.hide.create).
	HidePost(ctx context.Context, a authz.Actor, postID uuid.UUID) error
	// ListSavedPosts is listMySavedPosts (feed.save.list_own): Next is the
	// position (saved_at, post_id) of the next page.
	ListSavedPosts(ctx context.Context, a authz.Actor, q PageQuery) (PostPage, error)
	// FollowOrg / UnfollowOrg are followOrg / unfollowOrg
	// (feed.follow.manage); notify nil keeps the current setting.
	FollowOrg(ctx context.Context, a authz.Actor, orgID uuid.UUID, notify *bool) error
	UnfollowOrg(ctx context.Context, a authz.Actor, orgID uuid.UUID) error
	// ListFollows is listMyFollows (feed.follow.list_own).
	ListFollows(ctx context.Context, a authz.Actor, q PageQuery) (OrgPage, error)
	// MuteOrg / UnmuteOrg are muteOrg / unmuteOrg (feed.mute.manage).
	MuteOrg(ctx context.Context, a authz.Actor, orgID uuid.UUID) error
	UnmuteOrg(ctx context.Context, a authz.Actor, orgID uuid.UUID) error
	// ListMutes is listMyMutes (feed.mute.list_own).
	ListMutes(ctx context.Context, a authz.Actor, q PageQuery) (OrgPage, error)

	// CreateTargetedPost is createTargetedPost (feed.post.create_targeted,
	// 03 k16): a segment draft with its compiled audience and, for a
	// company, one targeted_post unit consumed in the same transaction.
	CreateTargetedPost(ctx context.Context, a authz.Actor, orgID uuid.UUID, in TargetedInput) (TargetedView, error)

	// CreateReport is createContentReport (feed.report.create): Created is
	// false when the reporter's open report on the target is returned.
	CreateReport(ctx context.Context, a authz.Actor, in ReportInput) (ReportResult, error)
	// ListReports is adminListReports (feed.report.list).
	ListReports(ctx context.Context, a authz.Actor, q ReportQuery) (ReportPage, error)
	// GetReport is adminGetReport (feed.report.read): the report with a
	// summary of its target.
	GetReport(ctx context.Context, a authz.Actor, id uuid.UUID) (ReportView, error)
	// ResolveReport is adminResolveReport (feed.report.resolve).
	ResolveReport(ctx context.Context, a authz.Actor, id uuid.UUID, in ResolveInput) (ReportView, error)
	// ListReviewQueue is adminListReviewQueue (feed.review_queue.list):
	// the posts in pending_review, oldest first.
	ListReviewQueue(ctx context.Context, a authz.Actor, q PageQuery) (PostPage, error)
	// ModeratePost is adminModeratePost (feed.post.moderate).
	ModeratePost(ctx context.Context, a authz.Actor, postID uuid.UUID, in ModerationInput) (PostModeration, error)
	// ModerateOrg is adminModerateOrg (feed.org.moderate).
	ModerateOrg(ctx context.Context, a authz.Actor, orgID uuid.UUID, in ModerationInput) (OrgModeration, error)
	// ListModerationActions is adminListModerationActions
	// (feed.moderation_action.list).
	ListModerationActions(ctx context.Context, a authz.Actor, q ActionQuery) (ActionPage, error)
}

// Deps are the dependencies of the feed module. Store, Tx, Outbox, Queue,
// Audit, Orgs, Idempotency, Clock and Log are required (New returns an
// error otherwise).
type Deps struct {
	Store       Store
	Tx          db.TxManager
	Outbox      outbox.Writer       // post.published, post.submitted_for_review, post.moderated, content_report.created (A)
	Queue       queue.Enqueuer      // the org moderation and report outcome notices (A, BE-032)
	Audit       AuditLogger         // every write, same transaction
	Orgs        Orgs                // the author org's kind, status and name
	Idempotency *idempotency.Keeper // optional Idempotency-Key of the POST operations (06 H-5)
	Clock       clock.Clock
	Log         *slog.Logger

	// Viewers is students' Profiles (audience keys of a reader). nil →
	// every reader holds 'all' only (targeted posts are seen by nobody but
	// their staff).
	Viewers Viewers
	// Events is the events module (event_share). nil → an event cannot be
	// shared (503) and shared events show no summary.
	Events Events
	// Files attaches and serves the images. nil → media are refused (503)
	// and no media URL is returned.
	Files Files
	// Clubs is directory's catalog (sensitive clubs). nil → every club
	// counts as sensitive: no club membership creates an automatic follow
	// (fail closed, as orgs does without club facts, T-208).
	Clubs Clubs
	// Targeting is outreach's Compile (targeted posts). nil → a targeted
	// post cannot be created (503).
	Targeting Targeting
	// Entitlements is the targeted_post quota. nil → a company's targeted
	// post cannot be created and a draft holding a unit cannot be deleted
	// or rejected (503: the unit would be lost).
	Entitlements Entitlements
	// Sender delivers the in-app moderation notices. nil → the notice
	// tasks fail and are retried (a notice is never dropped silently).
	Sender Sender
	// Staff lists the moderators (and the platform admins when there is
	// none) who receive the review queue and report notices. nil → as
	// Sender.
	Staff Staff

	// ReviewThreshold is FEED_REVIEW_THRESHOLD: the approved posts after
	// which a new org publishes without review; 0 → DefaultReviewThreshold.
	ReviewThreshold int
	// ReportHideThreshold is FEED_REPORT_HIDE_THRESHOLD: the distinct
	// users whose open reports hide a published post until a moderator
	// decides; 0 → DefaultReportHideThreshold.
	ReportHideThreshold int
}

func (d Deps) validate() error {
	var errs []error
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"Store", d.Store != nil}, {"Tx", d.Tx != nil}, {"Outbox", d.Outbox != nil}, {"Queue", d.Queue != nil},
		{"Audit", d.Audit != nil},
		{"Orgs", d.Orgs != nil}, {"Idempotency", d.Idempotency != nil}, {"Clock", d.Clock != nil}, {"Log", d.Log != nil},
	} {
		if !c.ok {
			errs = append(errs, fmt.Errorf("feed.Deps.%s is missing", c.name))
		}
	}
	if d.ReviewThreshold < 0 {
		errs = append(errs, errors.New("feed.Deps.ReviewThreshold is negative"))
	}
	if d.ReportHideThreshold < 0 {
		errs = append(errs, errors.New("feed.Deps.ReportHideThreshold is negative"))
	}
	return errors.Join(errs...)
}

// Module is what feed hands to the composition root.
type Module struct {
	Service Service
	// Privacy is the KVKK provider (the posts a user authored, their likes,
	// saves, hides, follows and mutes, their reports and, for a moderator,
	// their moderation actions).
	Privacy privacy.Provider

	svc *service
}

// New builds the feed module; a missing dependency is a wiring bug that
// stops the process at start.
func New(d Deps) (Module, error) {
	if err := d.validate(); err != nil {
		return Module{}, err
	}
	if d.ReviewThreshold == 0 {
		d.ReviewThreshold = DefaultReviewThreshold
	}
	if d.ReportHideThreshold == 0 {
		d.ReportHideThreshold = DefaultReportHideThreshold
	}
	s := &service{d: d}
	return Module{Service: s, Privacy: privacyProvider{s: s}, svc: s}, nil
}

type service struct{ d Deps }

var _ Service = (*service)(nil)

// now is the service clock in the database's precision (microseconds).
func (s *service) now() time.Time { return s.d.Clock.Now().UTC().Truncate(time.Microsecond) }
