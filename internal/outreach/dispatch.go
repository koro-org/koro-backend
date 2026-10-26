package outreach

import (
	"context"
	"errors"
	"slices"
	"strings"
	"unicode/utf8"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/entitlements"
	"koro/internal/notify"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
)

// Dispatch of targeted posts (BE-035; 01 §6.3 "Hedefli gönderi yayını",
// 02 §13.2, 03 §5.8 k18, 04 §2.8, §F.1). Scheduling writes the row, the
// unit and the outbox event in one transaction; the run (dispatch_run.go)
// notifies the audience once the post is published.

// Audit actions of the dispatch (03 §4.1: the authz actions) and the
// audited resource type.
const (
	auditDispatchResource = "dispatch"
	// dispatchAggType is the aggregate type of outreach.dispatch_scheduled.
	dispatchAggType = "outreach_dispatch"
	// maxTitle is title_snapshot's limit (outreach_dispatches_title_snapshot_chk).
	maxTitle = 140
)

// dispatchRef is the entitlements reference of a dispatch's unit.
func dispatchRef(id uuid.UUID) entitlements.Ref {
	return entitlements.Ref{Type: entitlements.RefDispatch, ID: id}
}

// dispatchSurface is the authz view of an org's dispatches
// (/orgs/{org_id}/dispatches, and the org of the post of
// /posts/{post_id}/dispatches; 03 §3.5 dispatch: OrgID = the org, no
// Attrs): the org management surface.
func dispatchSurface(orgID uuid.UUID) authz.Resource {
	id := orgID
	return authz.Resource{Type: authz.TypeDispatch, ID: id, OrgID: &id}
}

// deepLink is the in-app path of a post (outreach_dispatches.deep_link).
func deepLink(postID uuid.UUID) string { return notify.PostLink(postID).String() }

// commercialPost reports whether a post's dispatch must be commercial: a
// company's post (posts_company_commercial_chk) and a sponsored post (T-16:
// commercial_flag) reach only consenting students (03 k18).
func commercialPost(p PostInfo) bool { return p.Commercial || p.AuthorKind == AuthorCompany }

// spendsUnit reports whether a post's dispatch spends a targeted_dispatch
// unit: a company's (03 k18; entitlements are company features, 06 H-40).
func spendsUnit(p PostInfo) bool { return p.AuthorKind == AuthorCompany }

// ScheduleDispatch implements Targeting.
func (s *service) ScheduleDispatch(ctx context.Context, in DispatchInput) (uuid.UUID, error) {
	a, p := in.Actor, in.Post
	if p.ID == uuid.Nil() || p.OrgID == uuid.Nil() {
		return uuid.Nil(), ErrNotFound
	}
	if err := authz.Can(ctx, a, authz.OutreachDispatchCreate, dispatchSurface(p.OrgID)); err != nil {
		return uuid.Nil(), err
	}
	if err := s.usableOrg(ctx, a, p.OrgID); err != nil {
		return uuid.Nil(), err
	}
	plan, err := s.planDispatch(ctx, a, p, in.Channels, in.Commercial)
	if err != nil {
		return uuid.Nil(), err
	}
	var id uuid.UUID
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		switch _, err := s.d.Store.DispatchOfPost(ctx, p.OrgID, p.ID, true); {
		case err == nil:
			return ErrDispatchExists // before the unit: nothing to give back
		case !errors.Is(err, ErrNotFound):
			return err
		}
		d := plan
		d.ID = uuid.NewV7()
		if spendsUnit(p) {
			usage, err := s.d.Entitlements.Consume(ctx, p.OrgID, entitlements.FeatureTargetedDispatch, 1, dispatchRef(d.ID))
			if err != nil {
				return err
			}
			d.EntitlementUsageID = &usage
		}
		stored, err := s.d.Store.InsertDispatch(ctx, d)
		if err != nil {
			return err
		}
		env, err := domainevent.New(domainevent.OutreachDispatchScheduled{DispatchID: stored.ID, OrgID: stored.OrgID,
			PostID: stored.PostID}, dispatchAggType, stored.ID)
		if err != nil {
			return err
		}
		if err := s.d.Outbox.Add(ctx, env); err != nil {
			return err
		}
		id = stored.ID
		return s.d.Audit.Log(ctx, auditDispatch(a, authz.OutreachDispatchCreate, stored, map[string]any{
			"post_id": stored.PostID.String(), "channels": []string(stored.Channels), "commercial": stored.Commercial,
			"dimensions": int(stored.AudienceDimCount), "quota_consumed": stored.EntitlementUsageID != nil,
			"post_status": string(p.Status),
		}))
	})
	if err != nil {
		return uuid.Nil(), err
	}
	return id, nil
}

// planDispatch checks a dispatch of p by a and returns its row (without ID
// and unit): a targeted post that may still reach its audience, a's scope
// in the post's org (03 k16/k18), the commercial nature and the channels.
func (s *service) planDispatch(ctx context.Context, a authz.Actor, p PostInfo, channels []Channel, commercial *bool) (Dispatch, error) {
	if !p.Targeted || len(p.Keys) == 0 || p.DimCount < 1 || p.DimCount > 4 {
		return Dispatch{}, ErrPostNotTargeted
	}
	switch p.Status {
	case PostDraft, PostPendingReview, PostPublished:
	default: // hidden, removed: the post no longer reaches anyone
		return Dispatch{}, ErrInvalidState
	}
	sc := ScopeOf(a, p.OrgID)
	if err := s.checkDispatchScope(ctx, sc, p); err != nil {
		return Dispatch{}, err
	}
	must := commercialPost(p)
	c := must
	if commercial != nil {
		if must && !*commercial {
			return Dispatch{}, problem.Field("body.commercial",
				"the dispatch of a company's or a sponsored post is commercial (only consenting students)")
		}
		c = *commercial
	}
	chans, err := s.checkChannels(channels, c)
	if err != nil {
		return Dispatch{}, err
	}
	title := strings.TrimSpace(p.Title)
	if utf8.RuneCountInString(title) > maxTitle {
		title = string([]rune(title)[:maxTitle])
	}
	return Dispatch{
		OrgID: p.OrgID, PostID: p.ID, SegmentID: cloneID(p.SegmentID), AudienceKeys: slices.Clone(p.Keys),
		AudienceDimCount: p.DimCount, Channels: chans, TitleSnapshot: title, DeepLink: deepLink(p.ID), Commercial: c,
		Status: DispatchScheduled, CreatedBy: a.UserID,
	}, nil
}

// checkDispatchScope applies a's targeting scope to the keys p was
// compiled with (03 k16, k18): a club_admin notifies its own members only
// (club:<own club>, the member list); a university never names a club; a
// company or the platform never a club that is sensitive now (03 Y2: it
// may have become sensitive since the post was compiled).
func (s *service) checkDispatchScope(ctx context.Context, sc Scope, p PostInfo) error {
	var clubs []uuid.UUID
	for _, k := range p.Keys {
		if id, ok := strings.CutPrefix(k, keyClub); ok {
			cid, err := uuid.Parse(id)
			if err != nil {
				return ErrTargetOutOfScope
			}
			clubs = append(clubs, cid)
		}
	}
	switch sc {
	case ScopeClub:
		if len(p.Keys) != 1 || p.Keys[0] != keyClub+p.OrgID.String() {
			return ErrTargetOutOfScope
		}
		return nil
	case ScopeUniversity:
		if len(clubs) > 0 {
			return ErrTargetOutOfScope
		}
		return nil
	case ScopeCompany, ScopePlatform:
		if len(clubs) == 0 {
			return nil
		}
		sensitive, err := s.d.Catalog.SensitiveOrgIDs(ctx, clubs)
		if err != nil {
			return err
		}
		for _, id := range clubs {
			if sensitive[id] {
				return ErrSensitiveTarget
			}
		}
		return nil
	}
	return ErrTargetOutOfScope
}

// checkChannels validates the channels of a dispatch and returns them
// sorted: one to three of push, email and in_app. A commercial dispatch
// never goes in-app (notify: an in-app message has no opt-out) and uses
// e-mail only with the one-click unsubscribe endpoint (6563: every
// commercial e-mail carries the opt-out link); e-mail needs the web origin
// of the post link.
func (s *service) checkChannels(in []Channel, commercial bool) ([]string, error) {
	const loc = "body.channels"
	if len(in) == 0 || len(in) > 3 {
		return nil, problem.Field(loc, "1-3 of push, email, in_app")
	}
	out := make([]string, 0, len(in))
	for _, c := range in {
		if !c.valid() {
			return nil, problem.Field(loc, "must be push, email or in_app")
		}
		out = append(out, string(c))
	}
	slices.Sort(out)
	if len(slices.Compact(slices.Clone(out))) != len(out) {
		return nil, problem.Field(loc, "a channel is given twice")
	}
	switch {
	case commercial && slices.Contains(out, string(ChannelInApp)):
		return nil, problem.Field(loc, "a commercial dispatch goes by push or e-mail (an in-app message has no opt-out)")
	case slices.Contains(out, string(ChannelEmail)) && s.d.LinkBaseURL == "":
		return nil, problem.Field(loc, "e-mail is not available (no link origin configured)")
	case commercial && slices.Contains(out, string(ChannelEmail)) && s.d.Unsubscribe == nil:
		return nil, problem.Field(loc, "commercial e-mail is not available (no unsubscribe endpoint configured)")
	}
	return out, nil
}

// DiscardDispatch implements Targeting.
func (s *service) DiscardDispatch(ctx context.Context, a authz.Actor, orgID, postID uuid.UUID) error {
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		d, err := s.d.Store.DispatchOfPost(ctx, orgID, postID, true)
		switch {
		case errors.Is(err, ErrNotFound):
			return nil
		case err != nil:
			return err
		}
		if d.Status != DispatchScheduled && d.Status != DispatchCancelled {
			return ErrInvalidState // it ran: the post was published
		}
		if err := s.releaseUnit(ctx, d); err != nil {
			return err
		}
		if err := s.d.Store.DeleteDispatch(ctx, orgID, d.ID); err != nil {
			return err
		}
		return s.d.Audit.Log(ctx, auditDispatch(a, authz.OutreachDispatchCancel, d, map[string]any{
			"reason": "post_withdrawn", "deleted": true, "previous_status": string(d.Status),
		}))
	})
}

// DispatchOfPost implements Targeting.
func (s *service) DispatchOfPost(ctx context.Context, orgID, postID uuid.UUID) (DispatchView, bool, error) {
	d, err := s.d.Store.DispatchOfPost(ctx, orgID, postID, false)
	switch {
	case errors.Is(err, ErrNotFound):
		return DispatchView{}, false, nil
	case err != nil:
		return DispatchView{}, false, err
	}
	return viewOf(d), true, nil
}

// DispatchIDsOfPosts implements Targeting.
func (s *service) DispatchIDsOfPosts(ctx context.Context, orgID uuid.UUID, postIDs []uuid.UUID) (map[uuid.UUID]uuid.UUID, error) {
	return s.d.Store.DispatchIDsOfPosts(ctx, orgID, postIDs)
}

// releaseUnit gives a dispatch's targeted_dispatch unit back (a ref that
// consumed nothing, or was released already, is a no-op), in the
// transaction carried by ctx.
func (s *service) releaseUnit(ctx context.Context, d Dispatch) error {
	if d.EntitlementUsageID == nil {
		return nil
	}
	return s.d.Entitlements.Release(ctx, dispatchRef(d.ID))
}

// CreateDispatch implements Service. Order: the post (404) → authz on its
// org's surface (404/403; the org from the post row, 06 H-38) → under the
// Idempotency-Key, in one transaction: ScheduleDispatch.
func (s *service) CreateDispatch(ctx context.Context, a authz.Actor, postID uuid.UUID, req DispatchRequest) (DispatchView, error) {
	p, err := s.d.Posts.DispatchPost(ctx, postID)
	if err != nil {
		return DispatchView{}, err
	}
	if err := authz.Can(ctx, a, authz.OutreachDispatchCreate, dispatchSurface(p.OrgID)); err != nil {
		return DispatchView{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op   string
		Post uuid.UUID
		Req  DispatchRequest
	}{"dispatch", postID, req})
	if err != nil {
		return DispatchView{}, err
	}
	id, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		return s.ScheduleDispatch(ctx, DispatchInput{Actor: a, Post: p, Channels: req.Channels, Commercial: req.Commercial})
	})
	if err != nil {
		return DispatchView{}, err
	}
	d, err := s.d.Store.GetDispatch(ctx, p.OrgID, id)
	if err != nil {
		return DispatchView{}, err
	}
	return viewOf(d), nil
}

// ListDispatches implements Service.
func (s *service) ListDispatches(ctx context.Context, a authz.Actor, orgID uuid.UUID, q DispatchQuery) (DispatchPage, error) {
	if err := authz.Can(ctx, a, authz.OutreachDispatchList, dispatchSurface(orgID)); err != nil {
		return DispatchPage{}, err
	}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.ListDispatches(ctx, orgID, q.After, limit+1)
	if err != nil {
		return DispatchPage{}, err
	}
	rows, more := page.Cut(rows, limit)
	out := DispatchPage{Items: make([]DispatchView, len(rows))}
	for i, r := range rows {
		out.Items[i] = viewOf(r)
	}
	if more {
		last := rows[len(rows)-1].ID
		out.Next = &last
	}
	return out, nil
}

// GetDispatch implements Service.
func (s *service) GetDispatch(ctx context.Context, a authz.Actor, orgID, id uuid.UUID) (DispatchView, error) {
	if err := authz.Can(ctx, a, authz.OutreachDispatchRead, dispatchSurface(orgID)); err != nil {
		return DispatchView{}, err
	}
	d, err := s.d.Store.GetDispatch(ctx, orgID, id)
	if err != nil {
		return DispatchView{}, err
	}
	return viewOf(d), nil
}

// CancelDispatch implements Service. A scheduled dispatch is cancelled and
// a company's unit goes back in the same transaction; cancelling a
// cancelled dispatch changes nothing; a running or finished one is 409.
func (s *service) CancelDispatch(ctx context.Context, a authz.Actor, orgID, id uuid.UUID) (DispatchView, error) {
	if err := authz.Can(ctx, a, authz.OutreachDispatchCancel, dispatchSurface(orgID)); err != nil {
		return DispatchView{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op  string
		Org uuid.UUID
		ID  uuid.UUID
	}{"cancel_dispatch", orgID, id})
	if err != nil {
		return DispatchView{}, err
	}
	if _, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		cur, err := s.d.Store.LockDispatch(ctx, orgID, id)
		switch {
		case err != nil:
			return uuid.Nil(), err
		case cur.Status == DispatchCancelled:
			return cur.ID, nil
		case cur.Status != DispatchScheduled:
			return uuid.Nil(), ErrInvalidState
		}
		next, ok, err := s.d.Store.TransitionDispatch(ctx, orgID, id, DispatchScheduled, DispatchCancelled, nil, s.now())
		switch {
		case err != nil:
			return uuid.Nil(), err
		case !ok:
			return uuid.Nil(), ErrInvalidState // a worker claimed it meanwhile
		}
		if err := s.releaseUnit(ctx, next); err != nil {
			return uuid.Nil(), err
		}
		return next.ID, s.d.Audit.Log(ctx, auditDispatch(a, authz.OutreachDispatchCancel, next, map[string]any{
			"reason": "cancelled_by_org", "unit_released": next.EntitlementUsageID != nil,
		}))
	}); err != nil {
		return DispatchView{}, err
	}
	d, err := s.d.Store.GetDispatch(ctx, orgID, id)
	if err != nil {
		return DispatchView{}, err
	}
	return viewOf(d), nil
}

// auditDispatch is the audit record of a dispatch write by a. The metadata
// never holds a count (06 H-61). A write by someone outside the org (a
// moderator rejecting the post, the system) is not filed under the org: its
// audit trail would show the moderator (BE-032); the org goes into the
// metadata instead.
func auditDispatch(a authz.Actor, action authz.Action, d Dispatch, meta map[string]any) audit.Event {
	id, org := d.ID, d.OrgID
	if meta == nil {
		meta = map[string]any{}
	}
	e := audit.Event{
		Action: string(action), Outcome: audit.Success, ResourceType: auditDispatchResource, ResourceID: &id,
		Metadata: meta,
	}
	if a.IsUser() && a.MemberOf(org) {
		e.OrgID, e.ActorOrgID = &org, &org
	} else {
		meta["org_id"] = org.String()
	}
	return e.By(a)
}
