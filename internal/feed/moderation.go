package feed

import (
	"context"
	"errors"
	"regexp"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/orgs"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
)

// Moderation (BE-032; 02 §12.1, §12.10, 03 §5.7, 04 §2.7). Moderators and
// platform admins decide on posts (approve a post in review, hide, remove,
// restore) and on orgs (warn, restrict, suspend: the feed trust level).
// Every decision is one append-only moderation_actions row, one audit
// record (no org scope: the moderator's identity stays with the platform)
// and a notice to the author org with the reason code; the author org's
// way back is an appeal through support, answered with restore.

// Moderation limits (02 §12.10, 04 §2.7).
const (
	maxModerationNote = 1000
	// maxRestriction bounds restricted_until: a longer restriction is a
	// suspension.
	maxRestriction = 365 * 24 * time.Hour
	// autoTrustJob names the system actor of feed.org_settings.auto_trust.
	autoTrustJob = "feed.auto_trust"
)

// reasonCodePattern is moderation_actions_reason_code_chk.
var reasonCodePattern = regexp.MustCompile(`^[a-z_]{2,40}$`)

// ModerationInput is the body of adminModeratePost and adminModerateOrg
// (04 §2.7).
type ModerationInput struct {
	Action     ModAction
	ReasonCode string
	Note       string
	ReportID   *uuid.UUID
	// RestrictedUntil is restrict_org's end (required there, in the
	// future, at most a year ahead; refused for every other action).
	RestrictedUntil *time.Time
}

// PostModeration answers adminModeratePost: the action taken and the post
// after it.
type PostModeration struct {
	Action ModerationAction
	Post   Post
}

// OrgModeration answers adminModerateOrg: the action taken and the org's
// feed settings after it.
type OrgModeration struct {
	Action   ModerationAction
	Settings OrgSettings
}

// ActionQuery narrows adminListModerationActions.
type ActionQuery struct {
	Filter ActionFilter
	Limit  int
	After  *uuid.UUID
}

// ActionPage is a page of moderation actions, newest first.
type ActionPage struct {
	Items  []ModerationAction
	NextID *uuid.UUID
}

// checkModeration validates the common fields of a decision.
func checkModeration(in ModerationInput) (*string, error) {
	if !reasonCodePattern.MatchString(in.ReasonCode) {
		return nil, fieldErr("body.reason_code", "must be 2-40 lowercase letters or underscores")
	}
	return cleanText("body.note", in.Note, maxModerationNote)
}

// linkedReport checks the report a decision cites: an unresolved report
// of the same target (422 otherwise; a resolved one is 409).
func (s *service) linkedReport(ctx context.Context, id *uuid.UUID, t TargetType, target uuid.UUID) error {
	if id == nil {
		return nil
	}
	r, err := s.d.Store.GetReport(ctx, *id)
	switch {
	case errors.Is(err, ErrNotFound):
		return fieldErr("body.report_id", "must be a report of this target")
	case err != nil:
		return err
	case r.TargetType != t || r.TargetID != target:
		return fieldErr("body.report_id", "must be a report of this target")
	case !r.Status.Unresolved():
		return ErrInvalidState
	}
	return nil
}

// moderationAudit is the audit record of a decision. It has no org scope
// (OrgID nil): the reported org's audit trail never shows who moderated it
// (moderators' safety); the org is in the metadata.
func moderationAudit(a authz.Actor, act authz.Action, m ModerationAction, orgID uuid.UUID, meta map[string]any) audit.Event {
	id := m.TargetID
	meta["moderation_action_id"] = m.ID.String()
	meta["action"] = string(m.Action)
	meta["reason_code"] = m.ReasonCode
	meta["org_id"] = orgID.String()
	if m.ReportID != nil {
		meta["report_id"] = m.ReportID.String()
	}
	return audit.Event{
		Action: string(act), Outcome: audit.Success, ResourceType: string(m.TargetType), ResourceID: &id, Metadata: meta,
	}.By(a)
}

// ModeratePost implements Service. Order: the admin surface (403 without
// a platform role) → the input (422) → in one transaction: the post (404,
// locked) → the moderator is not its author nor a writer of its org (403)
// → the cited report (422/409) → the transition (409 outside the state
// machine) → the moderation action, the audit record, post.moderated (A).
//
//	approve  pending_review → published (counts towards the org's trust; auto_trust)
//	hide     published → hidden
//	remove   published, hidden → removed; pending_review → removed (rejected: the unit goes back)
//	restore  hidden → published; removed by a moderator (remove was the last decision) → published
func (s *service) ModeratePost(ctx context.Context, a authz.Actor, postID uuid.UUID, in ModerationInput) (PostModeration, error) {
	if err := authz.Can(ctx, a, authz.FeedPostModerate, adminSurface); err != nil {
		return PostModeration{}, err
	}
	if !in.Action.postAction() {
		return PostModeration{}, fieldErr("body.action", "must be approve, hide, remove or restore")
	}
	if in.RestrictedUntil != nil {
		return PostModeration{}, fieldErr("body.restricted_until", "applies to restrict_org only")
	}
	note, err := checkModeration(in)
	if err != nil {
		return PostModeration{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op   string
		Post uuid.UUID
		In   ModerationInput
	}{"moderate_post", postID, in})
	if err != nil {
		return PostModeration{}, err
	}
	actionID, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		p, err := s.d.Store.LockPost(ctx, postID)
		if err != nil {
			return uuid.Nil(), err
		}
		if err := authz.Can(ctx, a, authz.FeedPostModerate, moderatedPost(p)); err != nil {
			return uuid.Nil(), err
		}
		if err := s.linkedReport(ctx, in.ReportID, TargetPost, p.ID); err != nil {
			return uuid.Nil(), err
		}
		next, err := s.transition(ctx, a, p, in.Action)
		if err != nil {
			return uuid.Nil(), err
		}
		if next, err = s.d.Store.UpdatePost(ctx, next); err != nil {
			return uuid.Nil(), err
		}
		if in.Action == ModApprove {
			if err := s.countApproval(ctx, p.OrgID); err != nil {
				return uuid.Nil(), err
			}
		}
		m, err := s.d.Store.InsertModerationAction(ctx, ModerationAction{
			ID: uuid.NewV7(), ReportID: in.ReportID, TargetType: TargetPost, TargetID: p.ID, Action: in.Action,
			ReasonCode: in.ReasonCode, ActorUserID: a.UserID, Note: note,
		})
		if err != nil {
			return uuid.Nil(), err
		}
		if err := s.d.Audit.Log(ctx, moderationAudit(a, authz.FeedPostModerate, m, p.OrgID, map[string]any{
			"previous_status": string(p.Status), "status": string(next.Status),
		})); err != nil {
			return uuid.Nil(), err
		}
		s.emit(ctx, domainevent.PostModerated{PostID: p.ID, OrgID: p.OrgID, Action: string(m.Action),
			ReasonCode: m.ReasonCode}, p.ID)
		if in.Action == ModApprove {
			s.emit(ctx, domainevent.PostPublished{PostID: p.ID, OrgID: p.OrgID, Kind: string(p.Kind),
				Commercial: p.CommercialFlag}, p.ID)
		}
		return m.ID, nil
	})
	if err != nil {
		return PostModeration{}, err
	}
	p, err := s.d.Store.GetPost(ctx, postID)
	if err != nil {
		return PostModeration{}, err
	}
	m, err := s.d.Store.GetModerationAction(ctx, actionID)
	if err != nil {
		return PostModeration{}, err
	}
	return PostModeration{Action: m, Post: p}, nil
}

// transition is the post's next row under a decision (see ModeratePost).
func (s *service) transition(ctx context.Context, a authz.Actor, p Post, act ModAction) (Post, error) {
	next, now := p, s.now()
	me := a.UserID
	switch {
	case act == ModApprove && p.Status == StatusPendingReview:
		o, err := s.org(ctx, p.OrgID)
		if err != nil {
			return Post{}, err
		}
		if err := s.notSuspended(ctx, o); err != nil {
			return Post{}, err
		}
		if err := s.publishable(ctx, o, p); err != nil {
			return Post{}, err
		}
		next.Status, next.PublishedAt, next.ReviewedBy, next.ReviewedAt = StatusPublished, &now, &me, &now
	case act == ModHide && p.Status == StatusPublished:
		next.Status = StatusHidden
	case act == ModRemove && (p.Status == StatusPublished || p.Status == StatusHidden):
		next.Status, next.RemovedAt = StatusRemoved, &now
	case act == ModRemove && p.Status == StatusPendingReview:
		// Rejected in review: never published, so the targeted_post unit
		// goes back (T-147: a new post consumes a new one).
		if err := s.releaseUnit(ctx, p); err != nil {
			return Post{}, err
		}
		next.Status, next.RemovedAt, next.ReviewedBy, next.ReviewedAt = StatusRemoved, &now, &me, &now
	case act == ModRestore && p.Status == StatusHidden:
		next.Status = StatusPublished
	case act == ModRestore && p.Status == StatusRemoved && p.PublishedAt != nil:
		// Only a moderator's removal is undone (an appeal); the author
		// org's own removal stays.
		last, err := s.d.Store.LatestActions(ctx, TargetPost, []uuid.UUID{p.ID})
		if err != nil {
			return Post{}, err
		}
		if m, ok := last[p.ID]; !ok || m.Action != ModRemove {
			return Post{}, ErrInvalidState
		}
		next.Status, next.RemovedAt = StatusPublished, nil
	default:
		return Post{}, ErrInvalidState
	}
	return next, nil
}

// countApproval counts an approved post of the org and, once a new org
// reaches the review threshold, makes it trusted
// (feed.org_settings.auto_trust, a system action of 03 §3.7, audited as
// such).
func (s *service) countApproval(ctx context.Context, orgID uuid.UUID) error {
	st, err := s.d.Store.AddApprovedPost(ctx, orgID)
	if err != nil || st.TrustLevel != TrustNew || st.ApprovedPostsCount < s.d.ReviewThreshold {
		return err
	}
	sys := authz.SystemActor(autoTrustJob)
	if err := authz.Can(ctx, sys, authz.FeedOrgSettingsAutoTrust, authz.Resource{}); err != nil {
		return err
	}
	promoted, err := s.d.Store.PromoteTrusted(ctx, orgID, s.d.ReviewThreshold)
	if err != nil || !promoted {
		return err
	}
	org := orgID
	return s.d.Audit.Log(ctx, audit.Event{
		Action: string(authz.FeedOrgSettingsAutoTrust), Outcome: audit.Success, OrgID: &org, ResourceType: "org",
		ResourceID: &org, Metadata: map[string]any{"approved_posts": st.ApprovedPostsCount, "threshold": s.d.ReviewThreshold},
	}.By(sys))
}

// ModerateOrg implements Service. Order: the admin surface (403 without a
// platform role) → the input (422) → the org (404; the platform org 409)
// → the decision on this org (403: a member of the org, or a moderator
// suspending or changing a suspended org, k17) → in one transaction: the
// cited report (422/409), the trust level, the moderation action, the
// audit record; the org's managers are notified after the commit (A).
//
//	warn_org      the trust level stays (a recorded warning)
//	restrict_org  restricted until restricted_until: every post goes to review (T-15)
//	suspend_org   suspended: no new post, no publication (403 feed.posting_suspended)
func (s *service) ModerateOrg(ctx context.Context, a authz.Actor, orgID uuid.UUID, in ModerationInput) (OrgModeration, error) {
	if err := authz.Can(ctx, a, authz.FeedOrgModerate, adminSurface); err != nil {
		return OrgModeration{}, err
	}
	if !in.Action.orgAction() {
		return OrgModeration{}, fieldErr("body.action", "must be warn_org, restrict_org or suspend_org")
	}
	now := s.now()
	switch {
	case in.Action == ModRestrictOrg && in.RestrictedUntil == nil:
		return OrgModeration{}, fieldErr("body.restricted_until", "is required for restrict_org")
	case in.Action == ModRestrictOrg && (!in.RestrictedUntil.After(now) || in.RestrictedUntil.Sub(now) > maxRestriction):
		return OrgModeration{}, fieldErr("body.restricted_until", "must be in the future, at most a year ahead")
	case in.Action != ModRestrictOrg && in.RestrictedUntil != nil:
		return OrgModeration{}, fieldErr("body.restricted_until", "applies to restrict_org only")
	}
	note, err := checkModeration(in)
	if err != nil {
		return OrgModeration{}, err
	}
	o, err := s.org(ctx, orgID)
	if err != nil {
		return OrgModeration{}, err
	}
	cur, _, err := s.d.Store.OrgSettings(ctx, o.ID)
	if err != nil {
		return OrgModeration{}, err
	}
	if cur.TrustLevel == "" {
		cur = OrgSettings{OrgID: o.ID, TrustLevel: TrustNew}
	}
	if err := authz.Can(ctx, a, authz.FeedOrgModerate, moderatedOrg(o, in.Action, cur.TrustLevel)); err != nil {
		return OrgModeration{}, err
	}
	if o.Kind == orgs.KindPlatform {
		return OrgModeration{}, ErrInvalidState // the platform org is always trusted
	}
	hash, err := idempotency.Hash(struct {
		Op  string
		Org uuid.UUID
		In  ModerationInput
	}{"moderate_org", o.ID, in})
	if err != nil {
		return OrgModeration{}, err
	}
	actionID, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		if err := s.linkedReport(ctx, in.ReportID, TargetOrg, o.ID); err != nil {
			return uuid.Nil(), err
		}
		next := cur
		me, reason := a.UserID, in.ReasonCode
		switch in.Action {
		case ModRestrictOrg:
			until := in.RestrictedUntil.UTC().Truncate(time.Microsecond)
			if next, err = s.d.Store.SetTrust(ctx, OrgSettings{OrgID: o.ID, TrustLevel: TrustRestricted,
				RestrictedUntil: &until, Reason: &reason, UpdatedBy: &me}); err != nil {
				return uuid.Nil(), err
			}
		case ModSuspendOrg:
			if next, err = s.d.Store.SetTrust(ctx, OrgSettings{OrgID: o.ID, TrustLevel: TrustSuspended,
				Reason: &reason, UpdatedBy: &me}); err != nil {
				return uuid.Nil(), err
			}
		}
		m, err := s.d.Store.InsertModerationAction(ctx, ModerationAction{
			ID: uuid.NewV7(), ReportID: in.ReportID, TargetType: TargetOrg, TargetID: o.ID, Action: in.Action,
			ReasonCode: in.ReasonCode, ActorUserID: a.UserID, Note: note,
		})
		if err != nil {
			return uuid.Nil(), err
		}
		meta := map[string]any{"previous_trust_level": string(cur.TrustLevel), "trust_level": string(next.TrustLevel)}
		if next.RestrictedUntil != nil {
			meta["restricted_until"] = next.RestrictedUntil.Format(time.RFC3339)
		}
		if err := s.d.Audit.Log(ctx, moderationAudit(a, authz.FeedOrgModerate, m, o.ID, meta)); err != nil {
			return uuid.Nil(), err
		}
		s.d.Queue.AfterCommit(ctx, NotifyOrgModeratedTaskName,
			orgModeratedTask{ActionID: m.ID, OrgID: o.ID, Action: string(m.Action)}, "feed.org_moderated:"+m.ID.String())
		return m.ID, nil
	})
	if err != nil {
		return OrgModeration{}, err
	}
	m, err := s.d.Store.GetModerationAction(ctx, actionID)
	if err != nil {
		return OrgModeration{}, err
	}
	st, ok, err := s.d.Store.OrgSettings(ctx, o.ID)
	if err != nil {
		return OrgModeration{}, err
	}
	if !ok {
		st = OrgSettings{OrgID: o.ID, TrustLevel: TrustNew}
	}
	return OrgModeration{Action: m, Settings: st}, nil
}

// ListReviewQueue implements Service (feed.review_queue.list): the posts
// in pending_review of every org, oldest first, in the management view.
func (s *service) ListReviewQueue(ctx context.Context, a authz.Actor, q PageQuery) (PostPage, error) {
	if err := authz.Can(ctx, a, authz.FeedReviewQueueList, adminSurface); err != nil {
		return PostPage{}, err
	}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.ListReview(ctx, q.After, limit+1)
	if err != nil {
		return PostPage{}, err
	}
	out := PostPage{Items: []PostView{}}
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		out.Next = &After{At: last.CreatedAt, ID: last.ID}
	}
	if out.Items, err = s.views(ctx, a, rows, true); err != nil {
		return PostPage{}, err
	}
	return out, nil
}

// ListModerationActions implements Service (feed.moderation_action.list).
func (s *service) ListModerationActions(ctx context.Context, a authz.Actor, q ActionQuery) (ActionPage, error) {
	if err := authz.Can(ctx, a, authz.FeedModerationActionList, adminSurface); err != nil {
		return ActionPage{}, err
	}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.ListModerationActions(ctx, q.Filter, q.After, limit+1)
	if err != nil {
		return ActionPage{}, err
	}
	out := ActionPage{Items: rows}
	if len(rows) > limit {
		out.Items = rows[:limit]
		last := out.Items[len(out.Items)-1].ID
		out.NextID = &last
	}
	return out, nil
}
