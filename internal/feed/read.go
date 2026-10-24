package feed

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/events"
	"koro/internal/orgs"
	"koro/internal/outreach"
	"koro/internal/platform/page"
)

// Page sizes of the feed (04 §2.7: limit ≤ 50; ticket: LIMIT 21 = 20 + 1).
const (
	DefaultFeedLimit = 20
	MaxFeedLimit     = 50
	mediaTTL         = 5 * time.Minute
)

// Feed tabs (04 §2.7 getFeed).
const (
	TabDiscover  = "discover"
	TabFollowing = "following"
)

// FeedQuery narrows getFeed.
type FeedQuery struct {
	Tab   string // TabDiscover (default) or TabFollowing
	Kind  Kind   // optional
	Limit int
	After *After
}

// PageQuery is the page of listPosts.
type PageQuery struct {
	Limit int
	After *After
}

// ManageQuery narrows listOrgPostsManage (statuses; newest first by id).
type ManageQuery struct {
	Statuses []Status
	Limit    int
	After    *uuid.UUID
}

// PostPage is a page of posts. Next is the keyset position of the next
// published page, NextID the one of a management page (nil: last page).
type PostPage struct {
	Items  []PostView
	Next   *After
	NextID *uuid.UUID
}

// MediaView is an image with its short-lived download URL ("" when the
// file is not clean or files is not wired).
type MediaView struct {
	Media
	URL string
}

// ViewerFlags is what the reader did on a post (04 §F.2 "viewer").
type ViewerFlags struct {
	Liked           bool
	Saved           bool
	FollowingAuthor bool
}

// PostView is a post as one caller may see it. Manage is true for the
// author org's management view (writes, listOrgPostsManage): only then
// does the DTO carry the status, the audience and the author user (04
// §F.2). Viewer is nil for an anonymous reader and in the management view.
type PostView struct {
	Post Post
	Org  orgs.OrgSummary
	// LogoURL is a short-lived URL of the author org's logo ("" without a
	// logo, before the scan clears it or without files; T-251).
	LogoURL string
	Media   []MediaView
	Event   *events.EventSummary
	ETag    string
	Viewer  *ViewerFlags
	Manage  bool
	// ReasonCode is the moderation reason of a hidden or removed post in
	// the management view (04 §2.7 "durum + reason_code"; BE-032): the
	// moderator's reason_code, reported_by_users for a post hidden by the
	// report threshold, "" for a post its org removed.
	ReasonCode string
}

// Stats are the counters of getPostStats (04 §2.7): no identity of who
// liked or saved (03 Y7).
type Stats struct {
	LikeCount int
	SaveCount int
	// Dispatch holds the counts of a targeted post's notification
	// dispatch (BE-035; nil: the post has none). Each count is a published
	// estimate (06 H-61): nil below 10 or before the run.
	Dispatch *outreach.DispatchCounts
}

// Get implements Posts.
func (s *service) Get(ctx context.Context, postID uuid.UUID) (PostSummary, error) {
	p, err := s.d.Store.GetPost(ctx, postID)
	if err != nil {
		return PostSummary{}, err
	}
	return summaryOf(p), nil
}

// summaryOf is the cross-module view of p.
func summaryOf(p Post) PostSummary {
	out := PostSummary{
		ID: p.ID, OrgID: p.OrgID, Kind: p.Kind, Status: p.Status, Opportunity: decodeOpportunity(p.Opportunity),
		AuthorKind: p.AuthorOrgKind, AudienceKind: p.AudienceKind, AudienceKeys: slices.Clone([]string(p.AudienceKeys)),
		DimCount: p.AudienceDimCount, Commercial: p.CommercialFlag,
	}
	if p.Title != nil {
		out.Title = *p.Title
	}
	if p.AudienceKind == AudienceSegment && len(p.AudienceSnapshot) > 0 {
		var snap audienceSnapshot
		if json.Unmarshal(p.AudienceSnapshot, &snap) == nil && snap.SegmentID != nil {
			id := *snap.SegmentID
			out.SegmentID = &id
		}
	}
	return out
}

// resource builds the authz view of p for a: the audience match of the
// reader's keys (needed for segment posts only: public and followers posts
// carry ['all'], which every reader holds) and, for a followers post,
// whether the reader follows (or is a member of) the author org.
func (s *service) resource(ctx context.Context, a authz.Actor, p Post) (authz.Resource, error) {
	match, follows := true, false
	if p.Status != StatusPublished {
		return postResource(p, false, false), nil
	}
	if p.AudienceKind == AudienceSegment {
		r, err := s.readerOf(ctx, a)
		if err != nil {
			return authz.Resource{}, err
		}
		match = audienceMatch(p.AudienceKeys, p.AudienceDimCount, r.Keys)
	}
	if p.AudienceKind == AudienceFollowers && a.IsUser() {
		if a.MemberOf(p.OrgID) {
			follows = true
		} else {
			f, err := s.d.Store.Follows(ctx, a.UserID, p.OrgID)
			if err != nil {
				return authz.Resource{}, err
			}
			follows = f
		}
	}
	return postResource(p, match, follows), nil
}

// authorize builds the post's resource for a and runs authz.Can.
func (s *service) authorize(ctx context.Context, a authz.Actor, act authz.Action, p Post) error {
	res, err := s.resource(ctx, a, p)
	if err != nil {
		return err
	}
	return authz.Can(ctx, a, act, res)
}

// GetPost implements Service: the post (404 when missing or not visible,
// 03 §3.6) in the reader view; its writers manage it through
// listOrgPostsManage and the writes.
func (s *service) GetPost(ctx context.Context, a authz.Actor, id uuid.UUID) (PostView, error) {
	p, err := s.d.Store.GetPost(ctx, id)
	if err != nil {
		return PostView{}, err
	}
	if err := s.authorize(ctx, a, authz.FeedPostRead, p); err != nil {
		return PostView{}, err
	}
	vs, err := s.views(ctx, a, []Post{p}, false)
	if err != nil {
		return PostView{}, err
	}
	if len(vs) == 0 {
		return PostView{}, ErrNotFound
	}
	return vs[0], nil
}

// GetFeed implements Service: fan-out on read (R-003 §1.5.2). Discover
// lists every post the reader may see; following keeps the followed and
// member orgs. Muted orgs and hidden posts are left out (BE-029 writes
// them), a minor gets no commercial post.
func (s *service) GetFeed(ctx context.Context, a authz.Actor, q FeedQuery) (PostPage, error) {
	if err := authz.Can(ctx, a, authz.FeedFeedRead, feedResource); err != nil {
		return PostPage{}, err
	}
	r, err := s.readerOf(ctx, a)
	if err != nil {
		return PostPage{}, err
	}
	f := FeedFilter{Reader: r, Following: q.Tab == TabFollowing, Kind: q.Kind}
	limit := clampFeed(q.Limit)
	rows, err := s.d.Store.ListFeed(ctx, f, q.After, limit+1)
	if err != nil {
		return PostPage{}, err
	}
	return s.publishedPage(ctx, a, rows, limit, false)
}

func clampFeed(n int) int {
	switch {
	case n <= 0:
		return DefaultFeedLimit
	case n > MaxFeedLimit:
		return MaxFeedLimit
	}
	return n
}

// ListPosts implements Service: the published posts of an org's page the
// caller may see (anonymous: public, non-commercial; T-57).
func (s *service) ListPosts(ctx context.Context, a authz.Actor, orgID uuid.UUID, q PageQuery) (PostPage, error) {
	o, err := s.org(ctx, orgID)
	if err != nil {
		return PostPage{}, err
	}
	if err := authz.Can(ctx, a, authz.FeedPostListByOrg, orgResource(o)); err != nil {
		return PostPage{}, err
	}
	r, err := s.readerOf(ctx, a)
	if err != nil {
		return PostPage{}, err
	}
	r.All = a.IsUser() && staffReader(a, o.ID)
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.ListOrgPosts(ctx, o.ID, r, q.After, limit+1)
	if err != nil {
		return PostPage{}, err
	}
	return s.publishedPage(ctx, a, rows, limit, false)
}

func (s *service) publishedPage(ctx context.Context, a authz.Actor, rows []Post, limit int, manage bool) (PostPage, error) {
	out := PostPage{Items: []PostView{}}
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		out.Next = &After{At: *last.PublishedAt, ID: last.ID}
	}
	vs, err := s.views(ctx, a, rows, manage)
	if err != nil {
		return PostPage{}, err
	}
	out.Items = vs
	return out, nil
}

// ListOrgPosts implements Service: every post of the org (drafts, review,
// hidden and removed included) for its management surface (03 §7.6:
// non-members 404, members without the role 403).
func (s *service) ListOrgPosts(ctx context.Context, a authz.Actor, orgID uuid.UUID, q ManageQuery) (PostPage, error) {
	o, err := s.org(ctx, orgID)
	if err != nil {
		return PostPage{}, err
	}
	if err := authz.Can(ctx, a, authz.FeedPostListOrgManage, orgResource(o)); err != nil {
		return PostPage{}, err
	}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.ListOrgManage(ctx, o.ID, q.Statuses, q.After, limit+1)
	if err != nil {
		return PostPage{}, err
	}
	out := PostPage{Items: []PostView{}}
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1].ID
		out.NextID = &last
	}
	vs, err := s.views(ctx, a, rows, true)
	if err != nil {
		return PostPage{}, err
	}
	out.Items = vs
	return out, nil
}

// PostStats implements Service (feed.post.stats): the author org's
// writers read the counters of their posts.
func (s *service) PostStats(ctx context.Context, a authz.Actor, id uuid.UUID) (Stats, error) {
	p, err := s.d.Store.GetPost(ctx, id)
	if err != nil {
		return Stats{}, err
	}
	if err := s.authorize(ctx, a, authz.FeedPostStats, p); err != nil {
		return Stats{}, err
	}
	st := Stats{LikeCount: p.LikeCount, SaveCount: p.SaveCount}
	if p.AudienceKind == AudienceSegment && s.d.Targeting != nil {
		d, ok, err := s.d.Targeting.DispatchOfPost(ctx, p.OrgID, p.ID)
		if err != nil {
			return Stats{}, err
		}
		if ok {
			c := d.Counts
			st.Dispatch = &c
		}
	}
	return st, nil
}

// managedView reads the post after a write by one of its writers.
func (s *service) managedView(ctx context.Context, a authz.Actor, id uuid.UUID) (PostView, error) {
	p, err := s.d.Store.GetPost(ctx, id)
	if err != nil {
		return PostView{}, err
	}
	vs, err := s.views(ctx, a, []Post{p}, true)
	if err != nil {
		return PostView{}, err
	}
	if len(vs) == 0 {
		return PostView{}, ErrNotFound
	}
	return vs[0], nil
}

// views builds the views of rows with a constant number of feed store
// statements whatever the page size (TestFeedQuery_NoNPlusOne): the
// author orgs (one GetMany), the media (one MediaOf) and the reader's
// state (one ViewerState). A shared event, a medium URL and an author
// logo URL are read per distinct event, medium and org (events and files
// have no batch read yet).
func (s *service) views(ctx context.Context, a authz.Actor, rows []Post, manage bool) ([]PostView, error) {
	out := make([]PostView, 0, len(rows))
	if len(rows) == 0 {
		return out, nil
	}
	ids, orgIDs, eventIDs := make([]uuid.UUID, 0, len(rows)), []uuid.UUID{}, []uuid.UUID{}
	for _, p := range rows {
		ids = append(ids, p.ID)
		if !slices.Contains(orgIDs, p.OrgID) {
			orgIDs = append(orgIDs, p.OrgID)
		}
		if p.EventID != nil && !slices.Contains(eventIDs, *p.EventID) {
			eventIDs = append(eventIDs, *p.EventID)
		}
	}
	os, err := s.d.Orgs.GetMany(ctx, orgIDs)
	if err != nil {
		return nil, err
	}
	media, err := s.d.Store.MediaOf(ctx, ids)
	if err != nil {
		return nil, err
	}
	reasons, err := s.reasons(ctx, rows, manage)
	if err != nil {
		return nil, err
	}
	var state *ViewerState
	if a.IsUser() && !manage {
		st, err := s.d.Store.ViewerState(ctx, a.UserID, ids, orgIDs)
		if err != nil {
			return nil, err
		}
		state = &st
	}
	logos := s.logoURLs(ctx, os)
	evs := map[uuid.UUID]events.EventSummary{}
	if s.d.Events != nil {
		for _, id := range eventIDs {
			e, err := s.d.Events.Get(ctx, id)
			switch {
			case errors.Is(err, events.ErrNotFound):
				continue
			case err != nil:
				return nil, err
			}
			evs[id] = e
		}
	}
	for _, p := range rows {
		o, ok := os[p.OrgID]
		if !ok {
			continue // an org row never disappears; a missing one is not shown
		}
		v := PostView{Post: p, Org: o, LogoURL: logos[p.OrgID], ETag: etagOf(p), Manage: manage, ReasonCode: reasons[p.ID]}
		if manage && p.Status == StatusRemoved {
			// The author org sees a removed post's status, not its content
			// (03 §3.6).
			v.Post = Post{ID: p.ID, OrgID: p.OrgID, AuthorOrgKind: p.AuthorOrgKind, Kind: p.Kind, Status: p.Status,
				AudienceKind: p.AudienceKind, CommercialFlag: p.CommercialFlag, PublishedAt: p.PublishedAt,
				RemovedAt: p.RemovedAt, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt}
			out = append(out, v)
			continue
		}
		for _, m := range media[p.ID] {
			mv := MediaView{Media: m}
			if s.d.Files != nil {
				if dl, err := s.d.Files.PresignGet(ctx, m.FileID, mediaTTL); err == nil {
					mv.URL = dl.URL
				}
			}
			v.Media = append(v.Media, mv)
		}
		if p.EventID != nil {
			// The summary is shown while the event stays public (an event
			// made members-only after the share is not spread further);
			// the author org's view always carries it.
			if e, ok := evs[*p.EventID]; ok && (manage || e.Visibility == events.VisibilityPublic && e.Status != events.StatusDraft) {
				v.Event = &e
			}
		}
		if state != nil {
			v.Viewer = &ViewerFlags{
				Liked: state.Liked[p.ID], Saved: state.Saved[p.ID], FollowingAuthor: state.Following[p.OrgID],
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// logoURLs signs the logo of each author org once per page (T-251). A logo
// that cannot be signed (not clean, gone, files not wired) is left out: a
// missing logo never fails a read.
func (s *service) logoURLs(ctx context.Context, os map[uuid.UUID]orgs.OrgSummary) map[uuid.UUID]string {
	out := map[uuid.UUID]string{}
	if s.d.Files == nil {
		return out
	}
	for id, o := range os {
		if o.LogoFileID == nil {
			continue
		}
		if dl, err := s.d.Files.PresignGet(ctx, *o.LogoFileID, mediaTTL); err == nil {
			out[id] = dl.URL
		}
	}
	return out
}

// reasons returns the moderation reason of the hidden and removed posts of
// a management page (one statement; none for a reader page): the reason
// code of the decision that set the status, reported_by_users for a post
// the report threshold hid.
func (s *service) reasons(ctx context.Context, rows []Post, manage bool) (map[uuid.UUID]string, error) {
	out := map[uuid.UUID]string{}
	if !manage {
		return out, nil
	}
	var ids []uuid.UUID
	for _, p := range rows {
		if p.Status == StatusHidden || p.Status == StatusRemoved {
			ids = append(ids, p.ID)
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	last, err := s.d.Store.LatestActions(ctx, TargetPost, ids)
	if err != nil {
		return nil, err
	}
	for _, p := range rows {
		m, ok := last[p.ID]
		switch {
		case p.Status == StatusHidden && ok && m.Action == ModHide, p.Status == StatusRemoved && ok && m.Action == ModRemove:
			out[p.ID] = m.ReasonCode
		case p.Status == StatusHidden:
			out[p.ID] = autoHideReason
		}
	}
	return out, nil
}

// likesBatch bounds the pairs of one WithdrawnLikes store read.
const likesBatch = 1000

// WithdrawnLikes implements Likes.
func (s *service) WithdrawnLikes(ctx context.Context, likes []LikeKey) (map[LikeKey]bool, error) {
	out := make(map[LikeKey]bool)
	for batch := range slices.Chunk(likes, likesBatch) {
		got, err := s.d.Store.WithdrawnLikes(ctx, batch)
		if err != nil {
			return nil, err
		}
		maps.Copy(out, got)
	}
	return out, nil
}
