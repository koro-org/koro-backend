package feed

import (
	"context"
	"encoding/json"
	"strings"
	"uuid"

	"koro/internal/authz"
	"koro/internal/entitlements"
	"koro/internal/orgs"
	"koro/internal/outreach"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/problem"
)

// Targeted posts (BE-032; 01 §6.3 "Hedefli gönderi yayını", 02 §12.2, 03
// k16, 04 §2.7, §F.1). A targeted post is a draft whose audience_kind is
// segment: its keys are compiled by outreach from a saved segment or an
// inline definition, and a company spends one targeted_post unit on it.
// Both happen in the transaction that writes the draft (one Idempotency-
// Key, one compilation, one unit); the audience never changes afterwards
// (updatePost refuses it, publication keeps it). Deleting the draft — or a
// moderator rejecting it in review — gives the unit back.

// TargetedInput is createTargetedPost's body: PostWrite (its AudienceKind
// is ignored: the post is a segment post) and the audience, exactly one of
// a saved segment of the org and an inline definition.
type TargetedInput struct {
	Post       PostInput
	SegmentID  *uuid.UUID
	Definition *outreach.Definition
}

// TargetedView is the created draft in its management view with the
// published audience estimate and, for a company, the targeted_post units
// left (04 §F.1).
type TargetedView struct {
	PostView
	// Estimate is the published estimate (outreach.PublicEstimate): no
	// number for a club's own members below 10 or for a sensitive club,
	// whose membership is never counted.
	Estimate outreach.Estimate
	// Remaining is the company's targeted_post units left; nil for the
	// authors that spend none (club, university, platform).
	Remaining *int
}

// audienceSnapshot is posts.audience_snapshot: the definition the keys
// were compiled from and the saved segment it came from (02 §12.2 "yayın
// anındaki segment tanımı (denetim)").
type audienceSnapshot struct {
	outreach.Definition
	SegmentID *uuid.UUID `json:"segment_id,omitempty"`
}

// spendsQuota reports whether an author spends targeted_post units: a
// company (03 k16; entitlements are company features, 06 H-40).
func spendsQuota(k AuthorKind) bool { return k == AuthorCompany }

// postRef is the entitlements reference of a post's unit.
func postRef(id uuid.UUID) entitlements.Ref {
	return entitlements.Ref{Type: entitlements.RefPost, ID: id}
}

// CreateTargetedPost implements Service. Order: the org (404) → authz on
// its management surface (404/403) → the author kind → the org status
// (403) → the feed trust level (403 when suspended) → the body (422) → in
// one transaction under the Idempotency-Key: outreach.Compile (422 scope,
// sensitive club, k ≥ 10; 429/503 count budget), the company's
// targeted_post unit (403 entitlements.not_entitled), the draft with its
// media, and the audit record.
func (s *service) CreateTargetedPost(ctx context.Context, a authz.Actor, orgID uuid.UUID, in TargetedInput) (TargetedView, error) {
	o, err := s.org(ctx, orgID)
	if err != nil {
		return TargetedView{}, err
	}
	if err := authz.Can(ctx, a, authz.FeedPostCreateTargeted, orgResource(o)); err != nil {
		return TargetedView{}, err
	}
	kind, ok := authorKinds[o.Kind]
	if !ok {
		return TargetedView{}, authz.ErrForbidden
	}
	if err := writable(a, o); err != nil {
		return TargetedView{}, err
	}
	if err := s.notSuspended(ctx, o); err != nil {
		return TargetedView{}, err
	}
	quota := spendsQuota(kind)
	if s.d.Targeting == nil || quota && s.d.Entitlements == nil {
		return TargetedView{}, problem.ErrUnavailable
	}
	if (in.SegmentID == nil) == (in.Definition == nil) {
		return TargetedView{}, fieldErr("body.audience", "exactly one of segment_id and definition is required")
	}
	base := Post{
		OrgID: o.ID, AuthorOrgKind: kind, AuthorUserID: a.UserID, Status: StatusDraft, AudienceKind: AudienceSegment,
		SchemaVersion: 1,
	}
	patch := in.Post.patch()
	patch.AudienceKind = nil
	ap, err := s.apply(ctx, a, o, base, patch, nil)
	if err != nil {
		return TargetedView{}, err
	}
	in.Post.AudienceKind = AudienceSegment
	hash, err := idempotency.Hash(struct {
		Op  string
		Org uuid.UUID
		In  TargetedInput
	}{"create_targeted", o.ID, in})
	if err != nil {
		return TargetedView{}, err
	}
	spec := outreach.SegmentSpec{OrgID: o.ID, SegmentID: in.SegmentID, Definition: in.Definition}
	id, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		c, err := s.d.Targeting.Compile(ctx, a, spec)
		if err != nil {
			return uuid.Nil(), err
		}
		p := ap.next
		p.ID = uuid.NewV7()
		if p, err = withAudience(p, c); err != nil {
			return uuid.Nil(), err
		}
		if quota {
			usage, err := s.d.Entitlements.Consume(ctx, o.ID, entitlements.FeatureTargetedPost, 1, postRef(p.ID))
			if err != nil {
				return uuid.Nil(), err
			}
			p.EntitlementUsageID = &usage
		}
		if p, err = s.d.Store.InsertPost(ctx, p); err != nil {
			return uuid.Nil(), err
		}
		if err := s.writeMedia(ctx, a, p, ap.media); err != nil {
			return uuid.Nil(), err
		}
		meta := map[string]any{
			"kind": string(p.Kind), "audience_kind": string(p.AudienceKind), "dimensions": int(p.AudienceDimCount),
			"commercial": p.CommercialFlag, "quota_consumed": quota,
		}
		if c.SegmentID != nil {
			meta["segment_id"] = c.SegmentID.String()
		}
		return p.ID, s.d.Audit.Log(ctx, auditEvent(a, authz.FeedPostCreateTargeted, p, meta))
	})
	if err != nil {
		return TargetedView{}, err
	}
	v, err := s.managedView(ctx, a, id)
	if err != nil {
		return TargetedView{}, err
	}
	out := TargetedView{PostView: v, Estimate: outreach.Estimate{BelowThreshold: true}}
	if n := v.Post.AudienceSizeEstimate; n != nil {
		out.Estimate = outreach.PublicEstimate(*n)
	}
	if quota {
		av, err := s.d.Entitlements.Available(ctx, o.ID, entitlements.FeatureTargetedPost)
		if err != nil {
			return TargetedView{}, err
		}
		left := av.Remaining
		out.Remaining = &left
	}
	return out, nil
}

// withAudience writes the compiled targeting onto a draft: the keys and
// dimensions, the snapshot and the published estimate (02 §12.2: NULL
// when it has no number, BE-030 "Size == 0").
func withAudience(p Post, c outreach.Compiled) (Post, error) {
	if len(c.Keys) == 0 || c.DimCount < 1 || c.DimCount > 4 {
		return Post{}, problem.ErrInternal
	}
	snap, err := json.Marshal(audienceSnapshot{Definition: c.Definition, SegmentID: c.SegmentID})
	if err != nil {
		return Post{}, err
	}
	p.AudienceKeys, p.AudienceDimCount, p.AudienceSnapshot = append([]string(nil), c.Keys...), c.DimCount, snap
	p.AudienceSizeEstimate = nil
	if c.Size > 0 {
		n := c.Size
		p.AudienceSizeEstimate = &n
	}
	return p, nil
}

// notSuspended refuses a new post of an org whose feed trust level is
// suspended (02 §12.1, set by moderation): a draft it could never publish
// would only hold a unit or an upload. The platform org is never
// suspended.
func (s *service) notSuspended(ctx context.Context, o orgs.OrgSummary) error {
	if o.Kind == orgs.KindPlatform {
		return nil
	}
	st, ok, err := s.d.Store.OrgSettings(ctx, o.ID)
	switch {
	case err != nil:
		return err
	case ok && st.TrustLevel == TrustSuspended:
		return ErrPostingSuspended
	}
	return nil
}

// releaseUnit gives the targeted_post unit of a post that was never
// published back (draft deleted, rejected in review), in the transaction
// carried by ctx.
func (s *service) releaseUnit(ctx context.Context, p Post) error {
	if p.EntitlementUsageID == nil {
		return nil
	}
	if s.d.Entitlements == nil {
		return problem.ErrUnavailable // the unit would be lost: fail closed
	}
	return s.d.Entitlements.Release(ctx, postRef(p.ID))
}

// checkTargets re-checks a segment post of a company or the platform at
// publication: a club it targets may have become sensitive since the
// draft was compiled (03 Y2: only the club's own admin addresses its
// members). Without directory every club counts as sensitive (fail
// closed).
func (s *service) checkTargets(ctx context.Context, p Post) error {
	if p.AudienceKind != AudienceSegment || p.AuthorOrgKind == AuthorClub {
		return nil
	}
	var clubs []uuid.UUID
	for _, k := range p.AudienceKeys {
		if id, ok := strings.CutPrefix(k, "club:"); ok {
			if cid, err := uuid.Parse(id); err == nil {
				clubs = append(clubs, cid)
			}
		}
	}
	if len(clubs) == 0 {
		return nil
	}
	if s.d.Clubs == nil {
		return outreach.ErrSensitiveTarget
	}
	sensitive, err := s.d.Clubs.SensitiveOrgIDs(ctx, clubs)
	if err != nil {
		return err
	}
	for _, id := range clubs {
		if sensitive[id] {
			return outreach.ErrSensitiveTarget
		}
	}
	return nil
}
