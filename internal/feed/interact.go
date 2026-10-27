package feed

import (
	"context"
	"errors"
	"slices"
	"uuid"

	"koro/internal/authz"
	"koro/internal/orgs"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
)

// The reader's interactions (BE-029; 02 §12.4–12.8, 03 §5.7, 04 §2.7, §F.3,
// §F.5). None of them is audited: they are the reader's own preferences on
// content they may already see, not a privileged change, and an audit
// trail of who liked what would only copy personal data (the rows
// themselves are exported and erased through KVKK).

// OrgItem is an org of listMyFollows / listMyMutes. Follow is set for a
// follow (source and notify), nil for a mute. LogoURL is the org's
// short-lived logo URL ("" without a logo, before the scan clears it or
// without files; BE-098).
type OrgItem struct {
	Org     orgs.OrgSummary
	Follow  *Follow
	LogoURL string
}

// OrgPage is a page of OrgItem; Next is the keyset position (created_at,
// org_id) of the next page.
type OrgPage struct {
	Items []OrgItem
	Next  *After
}

// ownList is the resource of the reader's own lists (03 §5.7 cells O).
func ownList(a authz.Actor) authz.Resource {
	me := a.UserID
	return authz.Resource{Type: authz.TypeFeed, OwnerUserID: &me}
}

// interactable reads a post for a like, a save or a hide: 404 when it is
// missing or the reader may not see it (03 §3.6), 409 feed.invalid_state
// when it is visible but not published (its writers and moderators see
// drafts, which take no interaction).
func (s *service) interactable(ctx context.Context, a authz.Actor, act authz.Action, id uuid.UUID) (Post, error) {
	p, err := s.posts().GetPost(ctx, id)
	if err != nil {
		return Post{}, err
	}
	if err := s.authorize(ctx, a, act, p); err != nil {
		return Post{}, err
	}
	if p.Status != StatusPublished {
		return Post{}, ErrInvalidState
	}
	return p, nil
}

// withdraw runs an unlike or an unsave. A visible post (any status) takes
// it; on a post the reader may no longer see (removed, moved out of the
// audience) only the reader's own row may go, so a reader can still clean
// up without learning whether a post they never touched exists: no row is
// 404 like a missing post.
func (s *service) withdraw(ctx context.Context, a authz.Actor, act authz.Action, id uuid.UUID,
	remove func(context.Context, uuid.UUID, uuid.UUID) (bool, error), after func(context.Context, Post),
) error {
	p, err := s.posts().GetPost(ctx, id)
	if err != nil {
		return err
	}
	authErr := s.authorize(ctx, a, act, p)
	if authErr != nil && !errors.Is(authErr, authz.ErrNotFound) {
		return authErr
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		removed, err := remove(ctx, a.UserID, p.ID)
		switch {
		case err != nil:
			return err
		case !removed && authErr != nil:
			return authErr
		case removed && after != nil:
			after(ctx, p)
		}
		return nil
	})
}

// LikePost implements Service: one post_reactions row and one count per
// reader whatever the number of PUTs; post.liked (A) only for a new like.
func (s *service) LikePost(ctx context.Context, a authz.Actor, id uuid.UUID) error {
	p, err := s.interactable(ctx, a, authz.FeedReactionManage, id)
	if err != nil {
		return err
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		added, err := s.interactions().AddLike(ctx, a.UserID, p.ID)
		if err != nil || !added {
			return err
		}
		s.emit(ctx, domainevent.PostLiked{PostID: p.ID, UserID: a.UserID, AuthorOrgID: p.OrgID}, p.ID)
		return nil
	})
}

// UnlikePost implements Service: 204 whether or not there was a like;
// post.unliked (A) only when one was removed.
func (s *service) UnlikePost(ctx context.Context, a authz.Actor, id uuid.UUID) error {
	return s.withdraw(ctx, a, authz.FeedReactionManage, id, s.interactions().RemoveLike, func(ctx context.Context, p Post) {
		s.emit(ctx, domainevent.PostUnliked{PostID: p.ID, UserID: a.UserID, AuthorOrgID: p.OrgID}, p.ID)
	})
}

// SavePost implements Service; post.saved (A) only for a new save.
func (s *service) SavePost(ctx context.Context, a authz.Actor, id uuid.UUID) error {
	p, err := s.interactable(ctx, a, authz.FeedSaveManage, id)
	if err != nil {
		return err
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		added, err := s.interactions().AddSave(ctx, a.UserID, p.ID)
		if err != nil || !added {
			return err
		}
		s.emit(ctx, domainevent.PostSaved{PostID: p.ID, UserID: a.UserID, AuthorOrgID: p.OrgID}, p.ID)
		return nil
	})
}

// UnsavePost implements Service (01 §6.2 has no post.unsaved).
func (s *service) UnsavePost(ctx context.Context, a authz.Actor, id uuid.UUID) error {
	return s.withdraw(ctx, a, authz.FeedSaveManage, id, s.interactions().RemoveSave, nil)
}

// HidePost implements Service: the post leaves the reader's feed (not the
// org page); hiding twice is one row.
func (s *service) HidePost(ctx context.Context, a authz.Actor, id uuid.UUID) error {
	hash, err := idempotency.Hash(struct {
		Op   string
		Post uuid.UUID
	}{"hide", id})
	if err != nil {
		return err
	}
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (bool, error) {
		p, err := s.interactable(ctx, a, authz.FeedHideCreate, id)
		if err != nil {
			return false, err
		}
		return s.interactions().AddHide(ctx, a.UserID, p.ID)
	})
	return err
}

// UnhidePost implements Service (BE-145, Q-35): the post comes back to the
// reader's feed. Like unsave it is 204 whether or not there was a hide; on
// a post the reader no longer sees only their own row goes, and no row is
// 404 like a missing post (withdraw).
func (s *service) UnhidePost(ctx context.Context, a authz.Actor, id uuid.UUID) error {
	return s.withdraw(ctx, a, authz.FeedHideDelete, id, s.interactions().RemoveHide, nil)
}

// ListSavedPosts implements Service: the reader's saved posts they may
// still see, newest save first (04 §F.3: "artık görünmeyenler atlanır").
func (s *service) ListSavedPosts(ctx context.Context, a authz.Actor, q PageQuery) (PostPage, error) {
	return s.listMarked(ctx, a, authz.FeedSaveListOwn, s.interactions().ListSaved, q)
}

// ListHiddenPosts implements Service (BE-145): the posts the reader hid
// that they may still see, newest hide first. A hidden post they no longer
// see is skipped like a saved one: it would not reach their feed anyway,
// an item without the post would say whether it was removed or moved out
// of their audience, and the row still goes with unhidePost, the post's
// deletion (ON DELETE CASCADE) or the KVKK erase.
func (s *service) ListHiddenPosts(ctx context.Context, a authz.Actor, q PageQuery) (PostPage, error) {
	return s.listMarked(ctx, a, authz.FeedHideListOwn, s.interactions().ListHidden, q)
}

// listMarked pages one of the reader's own post lists (act on their own
// feed resource): the posts they may see, newest row first; Next is the
// position (row time, post_id) of the next page.
func (s *service) listMarked(ctx context.Context, a authz.Actor, act authz.Action,
	list func(context.Context, Reader, *After, int) ([]MarkedPost, error), q PageQuery,
) (PostPage, error) {
	if err := authz.Can(ctx, a, act, ownList(a)); err != nil {
		return PostPage{}, err
	}
	r, err := s.readerOf(ctx, a)
	if err != nil {
		return PostPage{}, err
	}
	// Moderators and platform admins see every published post (03 §3.6).
	r.All = a.HasPlatformRole(authz.PlatformModerator) || a.HasPlatformRole(authz.PlatformAdmin)
	limit := clampFeed(q.Limit)
	rows, err := list(ctx, r, q.After, limit+1)
	if err != nil {
		return PostPage{}, err
	}
	out := PostPage{Items: []PostView{}}
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		out.Next = &After{At: last.MarkedAt, ID: last.Post.ID}
	}
	posts := make([]Post, len(rows))
	for i, r := range rows {
		posts[i] = r.Post
	}
	if out.Items, err = s.views(ctx, a, posts, false); err != nil {
		return PostPage{}, err
	}
	return out, nil
}

// followable reads an org for a follow or a mute: 404 when it is missing
// or its profile is not visible to the reader (03 §3.6), 409
// orgs.invalid_state when it is closed.
func (s *service) followable(ctx context.Context, a authz.Actor, act authz.Action, orgID uuid.UUID) (orgs.OrgSummary, error) {
	o, err := s.org(ctx, orgID)
	if err != nil {
		return orgs.OrgSummary{}, err
	}
	if err := authz.Can(ctx, a, act, authz.OrgResource(o.ID, string(o.Kind), string(o.Status))); err != nil {
		return orgs.OrgSummary{}, err
	}
	if o.Status == orgs.StatusClosed {
		return orgs.OrgSummary{}, orgs.ErrInvalidState
	}
	return o, nil
}

// unfollowable is withdraw for an org: an org the reader may no longer see
// still loses the reader's own row; no row is 404.
func (s *service) unfollowable(ctx context.Context, a authz.Actor, act authz.Action, orgID uuid.UUID,
	remove func(context.Context, uuid.UUID, uuid.UUID) (bool, error),
) error {
	o, err := s.org(ctx, orgID)
	if err != nil {
		return err
	}
	authErr := authz.Can(ctx, a, act, authz.OrgResource(o.ID, string(o.Kind), string(o.Status)))
	if authErr != nil && !errors.Is(authErr, authz.ErrNotFound) {
		return authErr
	}
	removed, err := remove(ctx, a.UserID, o.ID)
	switch {
	case err != nil:
		return err
	case !removed && authErr != nil:
		return authErr
	}
	return nil
}

// FollowOrg implements Service: the user's own choice, so an automatic
// follow becomes manual (it survives the end of the membership). Following
// a sensitive club is allowed: the row is the user's, nobody else sees it
// (the org gets a count at most, k ≥ 10), and it says nothing about a
// membership.
func (s *service) FollowOrg(ctx context.Context, a authz.Actor, orgID uuid.UUID, notify *bool) error {
	o, err := s.followable(ctx, a, authz.FeedFollowManage, orgID)
	if err != nil {
		return err
	}
	_, err = s.interactions().FollowManual(ctx, a.UserID, o.ID, notify)
	return err
}

// UnfollowOrg implements Service: any source goes (an automatic follow
// comes back only with a new membership).
func (s *service) UnfollowOrg(ctx context.Context, a authz.Actor, orgID uuid.UUID) error {
	return s.unfollowable(ctx, a, authz.FeedFollowManage, orgID, s.interactions().RemoveFollow)
}

// MuteOrg implements Service: the org's posts leave the reader's feed (04
// §F.5); its page stays readable.
func (s *service) MuteOrg(ctx context.Context, a authz.Actor, orgID uuid.UUID) error {
	o, err := s.followable(ctx, a, authz.FeedMuteManage, orgID)
	if err != nil {
		return err
	}
	_, err = s.interactions().AddMute(ctx, a.UserID, o.ID)
	return err
}

// UnmuteOrg implements Service.
func (s *service) UnmuteOrg(ctx context.Context, a authz.Actor, orgID uuid.UUID) error {
	return s.unfollowable(ctx, a, authz.FeedMuteManage, orgID, s.interactions().RemoveMute)
}

// ListFollows implements Service.
func (s *service) ListFollows(ctx context.Context, a authz.Actor, q PageQuery) (OrgPage, error) {
	if err := authz.Can(ctx, a, authz.FeedFollowListOwn, ownList(a)); err != nil {
		return OrgPage{}, err
	}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.interactions().ListFollows(ctx, a.UserID, q.After, limit+1)
	if err != nil {
		return OrgPage{}, err
	}
	out := OrgPage{Items: []OrgItem{}}
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		out.Next = &After{At: last.CreatedAt, ID: last.OrgID}
	}
	ids := make([]uuid.UUID, len(rows))
	for i, f := range rows {
		ids[i] = f.OrgID
	}
	os, err := s.d.Orgs.GetMany(ctx, ids)
	if err != nil {
		return OrgPage{}, err
	}
	logos := s.logoURLs(ctx, os)
	for _, f := range rows {
		if o, ok := os[f.OrgID]; ok {
			out.Items = append(out.Items, OrgItem{Org: o, Follow: &f, LogoURL: logos[f.OrgID]})
		}
	}
	return out, nil
}

// ListMutes implements Service.
func (s *service) ListMutes(ctx context.Context, a authz.Actor, q PageQuery) (OrgPage, error) {
	if err := authz.Can(ctx, a, authz.FeedMuteListOwn, ownList(a)); err != nil {
		return OrgPage{}, err
	}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.interactions().ListMutes(ctx, a.UserID, q.After, limit+1)
	if err != nil {
		return OrgPage{}, err
	}
	out := OrgPage{Items: []OrgItem{}}
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		out.Next = &After{At: last.CreatedAt, ID: last.OrgID}
	}
	ids := make([]uuid.UUID, len(rows))
	for i, m := range rows {
		ids[i] = m.OrgID
	}
	os, err := s.d.Orgs.GetMany(ctx, ids)
	if err != nil {
		return OrgPage{}, err
	}
	logos := s.logoURLs(ctx, os)
	for _, m := range rows {
		if o, ok := os[m.OrgID]; ok {
			out.Items = append(out.Items, OrgItem{Org: o, LogoURL: logos[m.OrgID]})
		}
	}
	return out, nil
}

// syncAutoFollow makes the automatic follow of userID and orgID match the
// membership as it is now (membership.started and membership.ended are
// both answered by it, so their order does not matter): an active
// membership of a non-sensitive org has an auto_membership follow (unless
// the user already follows it), anything else has none. A manual follow is
// never touched. A club whose sensitivity is unknown counts as sensitive.
func (s *service) syncAutoFollow(ctx context.Context, userID, orgID uuid.UUID, kind authz.OrgKind) error {
	ms, err := s.d.Orgs.MembershipsOf(ctx, userID)
	if err != nil {
		return err
	}
	active := slices.ContainsFunc(ms, func(m authz.Membership) bool { return m.OrgID == orgID })
	if active && kind == authz.OrgClub {
		sensitive := true
		if s.d.Clubs != nil {
			set, err := s.d.Clubs.SensitiveOrgIDs(ctx, []uuid.UUID{orgID})
			if err != nil {
				return err
			}
			sensitive = set[orgID]
		}
		active = !sensitive
	}
	if active {
		_, err = s.interactions().AddAutoFollow(ctx, userID, orgID)
		return err
	}
	_, err = s.interactions().RemoveAutoFollow(ctx, userID, orgID)
	return err
}
