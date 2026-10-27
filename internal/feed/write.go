package feed

import (
	"context"
	"errors"
	"log/slog"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/orgs"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/httpx"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/outbox"
)

// CreatePost implements Service. Order: the org (404) → authz on its
// management surface (404/403) → the author kind → the org status (403)
// → the feed trust level (403 when suspended, BE-032) → the body (422) →
// in one transaction: the draft, its media and their attachment, and the
// audit record.
func (s *service) CreatePost(ctx context.Context, a authz.Actor, orgID uuid.UUID, in PostInput) (PostView, error) {
	o, err := s.org(ctx, orgID)
	if err != nil {
		return PostView{}, err
	}
	if err := authz.Can(ctx, a, authz.FeedPostCreate, authz.OrgResource(o.ID, string(o.Kind), string(o.Status))); err != nil {
		return PostView{}, err
	}
	kind, ok := authorKinds[o.Kind]
	if !ok {
		return PostView{}, authz.ErrForbidden
	}
	if err := writable(a, o); err != nil {
		return PostView{}, err
	}
	if err := s.notSuspended(ctx, o); err != nil {
		return PostView{}, err
	}
	base := Post{
		OrgID: o.ID, AuthorOrgKind: kind, AuthorUserID: a.UserID, Status: StatusDraft, AudienceKind: AudiencePublic,
		AudienceKeys: []string{KeyAll}, AudienceDimCount: 1, SchemaVersion: 1,
	}
	if in.AudienceKind == "" {
		in.AudienceKind = AudiencePublic
	}
	ap, err := s.apply(ctx, a, o, base, in.patch(), nil)
	if err != nil {
		return PostView{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op  string
		Org uuid.UUID
		In  PostInput
	}{"create", o.ID, in})
	if err != nil {
		return PostView{}, err
	}
	id, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		p := ap.next
		p.ID = uuid.NewV7()
		p, err := s.posts().InsertPost(ctx, p)
		if err != nil {
			return uuid.Nil(), err
		}
		if err := s.writeMedia(ctx, a, p, ap.media); err != nil {
			return uuid.Nil(), err
		}
		return p.ID, s.d.Audit.Log(ctx, auditEvent(a, authz.FeedPostCreate, p, map[string]any{
			"kind": string(p.Kind), "audience_kind": string(p.AudienceKind), "commercial": p.CommercialFlag,
		}))
	})
	if err != nil {
		return PostView{}, err
	}
	return s.managedView(ctx, a, id)
}

// writeMedia replaces the media of p and attaches every image to the post
// in the transaction (files k30: the upload was authorised for post_media;
// checkMedia checked its owner). nil media leaves them as they are.
func (s *service) writeMedia(ctx context.Context, a authz.Actor, p Post, media []MediaInput) error {
	if media == nil {
		return nil
	}
	for _, m := range media {
		if err := s.d.Files.Attach(ctx, a, m.FileID, files.Owner{Module: "feed", EntityID: p.ID, Purpose: files.PurposePostMedia}); err != nil {
			return err
		}
	}
	return s.media().ReplaceMedia(ctx, p.OrgID, p.ID, mediaRows(media))
}

// locked reads the post for a change in the transaction carried by ctx:
// the row (locked; 404) → authz (404 invisible, 403 without the role) →
// the author org's status (403). org_id comes from the row (06 H-38).
func (s *service) locked(ctx context.Context, a authz.Actor, act authz.Action, id uuid.UUID) (Post, orgs.OrgSummary, error) {
	p, err := s.posts().LockPost(ctx, id)
	if err != nil {
		return Post{}, orgs.OrgSummary{}, err
	}
	if err := s.authorize(ctx, a, act, p); err != nil {
		return Post{}, orgs.OrgSummary{}, err
	}
	o, err := s.org(ctx, p.OrgID)
	if err != nil {
		return Post{}, orgs.OrgSummary{}, err
	}
	if err := writable(a, o); err != nil {
		return Post{}, orgs.OrgSummary{}, err
	}
	return p, o, nil
}

// UpdatePost implements Service. Order: the post (404) → authz (404/403) →
// the org status (403) → the post status (409: hidden and removed posts
// take no edit) → If-Match (428/412) → the patch (422; 409 for the kind,
// audience or label of a published post). An edit of a published post
// stamps edited_at.
func (s *service) UpdatePost(ctx context.Context, a authz.Actor, id uuid.UUID, ifMatch string, p PostPatch) (PostView, error) {
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		cur, o, err := s.locked(ctx, a, authz.FeedPostUpdate, id)
		if err != nil {
			return err
		}
		switch cur.Status {
		case StatusDraft, StatusPendingReview, StatusPublished:
		default:
			return ErrInvalidState
		}
		if err := httpx.CheckIfMatch(ifMatch, httpx.ETag(cur.UpdatedAt)); err != nil {
			return err
		}
		if p.empty() {
			return ErrEmptyPatch
		}
		media, err := s.media().MediaOf(ctx, []uuid.UUID{cur.ID})
		if err != nil {
			return err
		}
		ap, err := s.apply(ctx, a, o, cur, p, media[cur.ID])
		if err != nil {
			return err
		}
		next := ap.next
		if cur.Status == StatusPublished {
			now := s.now()
			next.EditedAt = &now
		}
		if next, err = s.posts().UpdatePost(ctx, next); err != nil {
			return err
		}
		if err := s.writeMedia(ctx, a, next, ap.media); err != nil {
			return err
		}
		return s.d.Audit.Log(ctx, auditEvent(a, authz.FeedPostUpdate, next, map[string]any{
			"status": string(next.Status), "media_replaced": ap.media != nil,
		}))
	})
	if err != nil {
		return PostView{}, err
	}
	return s.managedView(ctx, a, id)
}

// emit publishes a feed event after the commit (mechanism A, 01 §6.2). A
// failure is logged, never returned: the business write stands.
func (s *service) emit(ctx context.Context, p domainevent.Payload, postID uuid.UUID) {
	s.emitFor(ctx, p, postAggType, postID)
}

// emitFor is emit for an aggregate of another type (a content report).
func (s *service) emitFor(ctx context.Context, p domainevent.Payload, aggType string, id uuid.UUID) {
	if err := outbox.EmitAfterCommit(ctx, s.d.Outbox, p, aggType, id); err != nil {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "feed event not emitted",
			slog.String("topic", p.Topic()), slog.String("aggregate_id", id.String()), slog.String("err", err.Error()))
	}
}

// needsReview is the pre-moderation rule of 02 §12.1 and T-15: a new org
// (no settings row) with fewer approved posts than the threshold, and a
// restricted org until its restriction ends, publish into review; a
// suspended org cannot publish. The platform org is always trusted.
func (s *service) needsReview(ctx context.Context, o orgs.OrgSummary, now time.Time) (bool, error) {
	if o.Kind == orgs.KindPlatform {
		return false, nil
	}
	st, ok, err := s.settings().OrgSettings(ctx, o.ID)
	if err != nil {
		return false, err
	}
	if !ok {
		return true, nil
	}
	switch st.TrustLevel {
	case TrustTrusted:
		return false, nil
	case TrustSuspended:
		return false, ErrPostingSuspended
	case TrustRestricted:
		if st.RestrictedUntil == nil || st.RestrictedUntil.After(now) {
			return true, nil
		}
	}
	return st.ApprovedPostsCount < s.d.ReviewThreshold, nil
}

// PublishPost implements Service: draft → published (trusted org,
// post.published) or pending_review (new or restricted org,
// post.submitted_for_review). Every image must be cleared by the scan
// (409 files.not_clean) and a shared event must still be a published
// public event (422).
func (s *service) PublishPost(ctx context.Context, a authz.Actor, id uuid.UUID) (PostView, error) {
	hash, err := idempotency.Hash(struct {
		Op   string
		Post uuid.UUID
	}{"publish", id})
	if err != nil {
		return PostView{}, err
	}
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		p, o, err := s.locked(ctx, a, authz.FeedPostPublish, id)
		if err != nil {
			return uuid.Nil(), err
		}
		if p.Status != StatusDraft {
			return uuid.Nil(), ErrInvalidState
		}
		if err := s.publishable(ctx, o, p); err != nil {
			return uuid.Nil(), err
		}
		now := s.now()
		review, err := s.needsReview(ctx, o, now)
		if err != nil {
			return uuid.Nil(), err
		}
		next := p
		if review {
			next.Status = StatusPendingReview
		} else {
			next.Status, next.PublishedAt = StatusPublished, &now
		}
		if next, err = s.posts().UpdatePost(ctx, next); err != nil {
			return uuid.Nil(), err
		}
		if review {
			s.emit(ctx, domainevent.PostSubmittedForReview{PostID: p.ID, OrgID: p.OrgID}, p.ID)
		} else {
			s.emit(ctx, domainevent.PostPublished{PostID: p.ID, OrgID: p.OrgID, Kind: string(p.Kind),
				Commercial: p.CommercialFlag}, p.ID)
		}
		return p.ID, s.d.Audit.Log(ctx, auditEvent(a, authz.FeedPostPublish, next, map[string]any{
			"status": string(next.Status),
		}))
	})
	if err != nil {
		return PostView{}, err
	}
	return s.managedView(ctx, a, id)
}

// publishable checks what publication needs besides the draft status: the
// shape of the kind, clean images and a shareable event.
func (s *service) publishable(ctx context.Context, o orgs.OrgSummary, p Post) error {
	media, err := s.media().MediaOf(ctx, []uuid.UUID{p.ID})
	if err != nil {
		return err
	}
	if err := shape(p, len(media[p.ID])); err != nil {
		return err
	}
	for _, m := range media[p.ID] {
		if s.d.Files == nil {
			return files.ErrNotClean
		}
		f, err := s.d.Files.Get(ctx, m.FileID)
		switch {
		case errors.Is(err, files.ErrNotFound):
			return files.ErrNotClean
		case err != nil:
			return err
		case !clean(f):
			return files.ErrNotClean
		}
	}
	if p.EventID != nil {
		if err := s.checkEvent(ctx, o, *p.EventID); err != nil {
			return err
		}
	}
	return s.checkTargets(ctx, p)
}

// DeletePost implements Service: a post that was never published (draft,
// pending_review) is deleted with its media rows and its targeted_post
// unit goes back to the company (04 §2.7 "+ hak iadesi", same
// transaction); a published or hidden one becomes removed (04 §2.7; kept
// 12 months, then purged: BE-057) and keeps its unit, which was used. A
// removed post is 404 for the author org (it sees it in its management
// list only).
func (s *service) DeletePost(ctx context.Context, a authz.Actor, id uuid.UUID) error {
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		p, _, err := s.locked(ctx, a, authz.FeedPostDelete, id)
		if err != nil {
			return err
		}
		switch p.Status {
		case StatusDraft, StatusPendingReview:
			if err := s.releaseUnit(ctx, p); err != nil {
				return err
			}
			if err := s.discardDispatch(ctx, a, p); err != nil { // BE-035: before the post (FK)
				return err
			}
			if err := s.posts().DeletePost(ctx, p.OrgID, p.ID); err != nil {
				return err
			}
		case StatusPublished, StatusHidden:
			now := s.now()
			next := p
			next.Status, next.RemovedAt = StatusRemoved, &now
			if _, err := s.posts().UpdatePost(ctx, next); err != nil {
				return err
			}
		default:
			return ErrInvalidState
		}
		return s.d.Audit.Log(ctx, auditEvent(a, authz.FeedPostDelete, p, map[string]any{
			"previous_status": string(p.Status),
			"quota_released":  p.EntitlementUsageID != nil && (p.Status == StatusDraft || p.Status == StatusPendingReview),
		}))
	})
}
