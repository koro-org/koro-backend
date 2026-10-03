package directory

import (
	"context"
	"errors"
	"log/slog"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/orgs"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
)

// MemberThreshold is the k-anonymity threshold of member counts (k ≥ 10).
const MemberThreshold = 10

// ClubQuery narrows listClubs (04 §2.3: q, university_id, category,
// claimed, sort name).
type ClubQuery struct {
	Q            string
	UniversityID *uuid.UUID
	Category     Category
	Claimed      *bool
	Sort         string
	Limit        int
	After        *After
}

// ClubCard is a club in a list, with its member count (nil: hidden).
type ClubCard struct {
	Row         ClubRow
	MemberCount *int
}

// ClubPage is a page of clubs.
type ClubPage struct {
	Items []ClubCard
	Next  *After
	Sort  string
}

// ClubView is getClub: the club, its org's public profile, its university,
// the member count (nil below the threshold or for a sensitive club) and
// the ETag of updateClub.
type ClubView struct {
	Club        Club
	Org         orgs.OrgView
	University  University
	MemberCount *int
	ETag        string
}

// ClubPatch is the body of updateClub (04 §2.3): nil leaves a field as it
// is. SocialLinks replaces the whole map (an empty map clears it);
// ClearFoundedYear removes the year.
type ClubPatch struct {
	Category         *Category
	JoinPolicy       *JoinPolicy
	SocialLinks      map[string]string
	SetSocialLinks   bool
	FoundedYear      *int16
	ClearFoundedYear bool
}

func (p ClubPatch) empty() bool {
	return p.Category == nil && p.JoinPolicy == nil && !p.SetSocialLinks && p.FoundedYear == nil && !p.ClearFoundedYear
}

// ClubInput is the body of adminCreateClub.
type ClubInput struct {
	UniversityID    uuid.UUID
	Name            string
	Slug            string
	Category        Category
	JoinPolicy      JoinPolicy
	IsSensitive     bool
	SensitiveReason SensitiveReason
}

// SensitivityInput is the body of adminSetClubSensitivity.
type SensitivityInput struct {
	IsSensitive     bool
	SensitiveReason SensitiveReason
}

// countOf returns the member count to show: none for a sensitive club,
// none below the threshold, none when no counter is wired (03 §5.3
// directory.club.read).
func (s *service) countOf(ctx context.Context, c Club) *int {
	if c.IsSensitive || s.d.MemberCount == nil {
		return nil
	}
	n, found, err := s.d.MemberCount(ctx, c.OrgID)
	if err != nil {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "club member count not read",
			slog.String("club_org_id", c.OrgID.String()), slog.String("err", err.Error()))
		return nil
	}
	if !found || n < MemberThreshold {
		return nil
	}
	return &n
}

// ListClubs implements Service. A sensitive club is listed (its existence
// is public, 03 §3.6) without a member count.
func (s *service) ListClubs(ctx context.Context, a authz.Actor, q ClubQuery) (ClubPage, error) {
	if err := authz.Can(ctx, a, authz.DirectoryClubList, catalogResource); err != nil {
		return ClubPage{}, err
	}
	text, err := cleanQuery("query.q", q.Q)
	if err != nil {
		return ClubPage{}, err
	}
	if q.Category != "" && !q.Category.Valid() {
		return ClubPage{}, fieldErr("query.category", "unknown category")
	}
	f := ClubFilter{Q: text, UniversityID: q.UniversityID, Category: q.Category, Claimed: q.Claimed, Sort: sortOf(q.Sort)}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.ListClubs(ctx, f, q.After, limit+1)
	if err != nil {
		return ClubPage{}, err
	}
	rows, more := page.Cut(rows, limit)
	out := ClubPage{Items: make([]ClubCard, len(rows)), Sort: f.Sort}
	for i, r := range rows {
		out.Items[i] = ClubCard{Row: r, MemberCount: s.countOf(ctx, r.Club)}
	}
	if more {
		last := rows[len(rows)-1]
		out.Next = &After{Name: last.Name, ID: last.OrgID}
	}
	return out, nil
}

// GetClubView implements Service. The org's profile follows orgs'
// visibility (a closed club is 404 except for its members and staff).
func (s *service) GetClubView(ctx context.Context, a authz.Actor, clubID uuid.UUID) (ClubView, error) {
	c, err := s.GetClub(ctx, clubID)
	if err != nil {
		return ClubView{}, err
	}
	if err := authz.Can(ctx, a, authz.DirectoryClubRead, clubResource(c)); err != nil {
		return ClubView{}, err
	}
	return s.view(ctx, a, c)
}

// view builds the view of c as a sees it.
func (s *service) view(ctx context.Context, a authz.Actor, c Club) (ClubView, error) {
	o, err := s.d.Orgs.GetOrg(ctx, a, c.OrgID)
	if err != nil {
		return ClubView{}, err
	}
	u, err := s.d.Store.GetUniversity(ctx, c.UniversityID)
	if err != nil {
		return ClubView{}, err
	}
	return ClubView{Club: c, Org: o, University: u, MemberCount: s.countOf(ctx, c), ETag: etagOf(c.UpdatedAt)}, nil
}

// writableOrg is the org status rule of a club write (03 §2.3): a closed
// club takes no change, a suspended one only a platform admin's.
func (s *service) writableOrg(ctx context.Context, a authz.Actor, orgID uuid.UUID) error {
	o, err := s.d.Orgs.Get(ctx, orgID)
	if err != nil {
		return err
	}
	switch o.Status {
	case orgs.StatusClosed:
		return ErrInvalidState
	case orgs.StatusSuspended:
		if !a.HasPlatformRole(authz.PlatformAdmin) {
			return orgs.ErrOrgSuspended
		}
	}
	return nil
}

// UpdateClub implements Service. Order: the club (404) → authz (403) → org
// status → If-Match (428/412) → the patch (422; k7).
func (s *service) UpdateClub(ctx context.Context, a authz.Actor, clubID uuid.UUID, ifMatch string, p ClubPatch) (ClubView, error) {
	var out Club
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		c, err := s.d.Store.LockClub(ctx, clubID)
		if err != nil {
			return err
		}
		if err := authz.Can(ctx, a, authz.DirectoryClubUpdate, clubResource(c)); err != nil {
			return err
		}
		if err := s.writableOrg(ctx, a, c.OrgID); err != nil {
			return err
		}
		if err := checkIfMatch(ifMatch, etagOf(c.UpdatedAt)); err != nil {
			return err
		}
		if p.empty() {
			return ErrEmptyPatch
		}
		next, changed, err := applyClubPatch(c, p)
		if err != nil {
			return err
		}
		if out, err = s.d.Store.UpdateClub(ctx, next); err != nil {
			return err
		}
		org := c.OrgID
		return s.d.Audit.Log(ctx, auditEvent(a, authz.DirectoryClubUpdate, authz.TypeClub, c.OrgID, &org, "",
			map[string]any{"fields": changed}))
	})
	if err != nil {
		return ClubView{}, err
	}
	return s.view(ctx, a, out)
}

// applyClubPatch validates p against c and returns the new row and the
// names of the changed fields (audit: names only).
func applyClubPatch(c Club, p ClubPatch) (Club, []string, error) {
	next, changed := c, []string{}
	if p.Category != nil {
		if !p.Category.Valid() {
			return Club{}, nil, fieldErr("body.category", "unknown category")
		}
		// 03 k7: a sensitive category needs the flag, which only a platform
		// admin sets (adminSetClubSensitivity).
		if p.Category.Sensitive() && !c.IsSensitive {
			return Club{}, nil, ErrSensitivityRequired
		}
		next.Category, changed = *p.Category, append(changed, "category")
	}
	if p.JoinPolicy != nil {
		if !p.JoinPolicy.Valid() {
			return Club{}, nil, fieldErr("body.join_policy", "must be open or approval")
		}
		next.JoinPolicy, changed = *p.JoinPolicy, append(changed, "join_policy")
	}
	if p.SetSocialLinks {
		links, err := cleanSocialLinks("body.social_links", p.SocialLinks)
		if err != nil {
			return Club{}, nil, err
		}
		next.SocialLinks, changed = links, append(changed, "social_links")
	}
	switch {
	case p.FoundedYear != nil:
		if y := *p.FoundedYear; y < 1900 || y > 2100 {
			return Club{}, nil, fieldErr("body.founded_year", "must be 1900-2100")
		}
		next.FoundedYear, changed = ptr(*p.FoundedYear), append(changed, "founded_year")
	case p.ClearFoundedYear:
		next.FoundedYear, changed = nil, append(changed, "founded_year")
	}
	return next, changed, nil
}

// cleanSensitivity checks the flag/reason pair (clubs CHECKs).
func cleanSensitivity(sensitive bool, reason SensitiveReason, loc string) (*SensitiveReason, error) {
	switch {
	case sensitive && reason == "":
		return nil, fieldErr(loc, "is required for a sensitive club")
	case sensitive && !reason.Valid():
		return nil, fieldErr(loc, "unknown reason")
	case !sensitive && reason != "":
		return nil, fieldErr(loc, "applies to a sensitive club only")
	case sensitive:
		return ptr(reason), nil
	}
	return nil, nil
}

// AdminCreateClub implements Service: the club's org (orgs.OrgAdmin.Create)
// and its clubs row in one transaction (01 §6.3, T-78), with the audit
// record. A new club has no members, so its sensitivity emits no event.
func (s *service) AdminCreateClub(ctx context.Context, a authz.Actor, in ClubInput) (ClubView, error) {
	if err := authz.Can(ctx, a, authz.DirectoryClubCreate, adminSurface); err != nil {
		return ClubView{}, err
	}
	name, err := cleanText("body.name", in.Name, 2, maxName)
	if err != nil {
		return ClubView{}, err
	}
	if in.Slug != "" && (!skillSlugRe.MatchString(in.Slug) || len(in.Slug) > maxSlug) {
		return ClubView{}, fieldErr("body.slug", "must be lower-case letters and digits separated by single hyphens, at most 80")
	}
	switch {
	case !in.Category.Valid():
		return ClubView{}, fieldErr("body.category", "unknown category")
	case !in.JoinPolicy.Valid():
		return ClubView{}, fieldErr("body.join_policy", "must be open or approval")
	case in.Category.Sensitive() && !in.IsSensitive:
		return ClubView{}, ErrSensitivityRequired
	}
	reason, err := cleanSensitivity(in.IsSensitive, in.SensitiveReason, "body.sensitive_reason")
	if err != nil {
		return ClubView{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op string
		In ClubInput
	}{"create_club", in})
	if err != nil {
		return ClubView{}, err
	}
	by := a.UserID
	id, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		u, err := s.d.Store.LockUniversity(ctx, in.UniversityID)
		if errors.Is(err, ErrNotFound) || err == nil && !u.IsActive {
			return uuid.Nil(), fieldErr("body.university_id", "must be an active university")
		}
		if err != nil {
			return uuid.Nil(), err
		}
		orgID, err := s.d.Orgs.Create(ctx, orgs.CreateOrg{
			Kind: orgs.KindClub, Name: name, Slug: in.Slug, City: u.City, CreatedBy: &by,
		})
		if err != nil {
			return uuid.Nil(), err
		}
		c := Club{
			OrgID: orgID, UniversityID: u.ID, Category: in.Category, IsSensitive: in.IsSensitive,
			SensitiveReason: reason, JoinPolicy: in.JoinPolicy, ClaimStatus: ClaimUnclaimed, SocialLinks: SocialLinks{},
		}
		if in.IsSensitive {
			now := s.now()
			c.SensitivitySetBy, c.SensitivitySetAt = &by, &now
		}
		if _, err := s.d.Store.InsertClub(ctx, c); err != nil {
			return uuid.Nil(), err
		}
		return orgID, s.d.Audit.Log(ctx, auditEvent(a, authz.DirectoryClubCreate, authz.TypeClub, orgID, &orgID, "",
			map[string]any{"university_id": u.ID.String(), "category": string(in.Category), audit.MetaSensitive: in.IsSensitive}))
	})
	if err != nil {
		return ClubView{}, err
	}
	c, err := s.d.Store.GetClub(ctx, id)
	if err != nil {
		return ClubView{}, err
	}
	return s.view(ctx, a, c)
}

// AdminSetClubSensitivity implements Service (R): authz (403 before
// reading) → body → re-authentication → in one transaction: the flag, the
// audit record and, when the flag changes, club.sensitivity_changed
// (02 §8.5, T-80: students drops the club's audience keys).
func (s *service) AdminSetClubSensitivity(ctx context.Context, a authz.Actor, clubID uuid.UUID, in SensitivityInput, proof identity.ReauthProof) (ClubView, error) {
	if err := authz.Can(ctx, a, authz.DirectoryClubSetSensitivity, adminSurface); err != nil {
		return ClubView{}, err
	}
	reason, err := cleanSensitivity(in.IsSensitive, in.SensitiveReason, "body.sensitive_reason")
	if err != nil {
		return ClubView{}, err
	}
	if s.d.Reauth == nil {
		return ClubView{}, errUnavailable
	}
	kind, err := s.d.Reauth.Verify(ctx, a.UserID, proof)
	if err != nil {
		return ClubView{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op   string
		Club uuid.UUID
		In   SensitivityInput
	}{"set_sensitivity", clubID, in})
	if err != nil {
		return ClubView{}, err
	}
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		c, err := s.d.Store.LockClub(ctx, clubID)
		if err != nil {
			return uuid.Nil(), err
		}
		if !in.IsSensitive && c.Category.Sensitive() {
			return uuid.Nil(), ErrSensitivityRequired // a religious/political/identity club stays sensitive
		}
		if c.IsSensitive == in.IsSensitive && deref(c.SensitiveReason) == deref(reason) {
			return c.OrgID, nil // nothing changes: no audit row, no event
		}
		prev := c.IsSensitive
		now, by := s.now(), a.UserID
		c.IsSensitive, c.SensitiveReason, c.SensitivitySetBy = in.IsSensitive, reason, &by
		c.SensitivitySetAt = nil
		if in.IsSensitive {
			c.SensitivitySetAt = &now
		}
		if _, err := s.d.Store.UpdateClub(ctx, c); err != nil {
			return uuid.Nil(), err
		}
		meta := map[string]any{audit.MetaSensitive: in.IsSensitive, "previous_sensitive": prev, audit.MetaReauth: kind}
		if reason != nil {
			meta["sensitive_reason"] = string(*reason)
		}
		org := c.OrgID
		if err := s.d.Audit.Log(ctx, auditEvent(a, authz.DirectoryClubSetSensitivity, authz.TypeClub, c.OrgID, &org, "", meta)); err != nil {
			return uuid.Nil(), err
		}
		if prev == in.IsSensitive {
			return c.OrgID, nil // only the reason changed: the audience is unaffected
		}
		env, err := domainevent.New(domainevent.ClubSensitivityChanged{ClubOrgID: c.OrgID, Sensitive: in.IsSensitive}, "club", c.OrgID)
		if err != nil {
			return uuid.Nil(), err
		}
		return c.OrgID, s.d.Outbox.Add(ctx, env)
	})
	if err != nil {
		return ClubView{}, err
	}
	c, err := s.d.Store.GetClub(ctx, clubID)
	if err != nil {
		return ClubView{}, err
	}
	return s.view(ctx, a, c)
}

// RequestClubSensitivity implements Service (T-67): no table; the request
// is an audit record (note = its reason) and an in-app notice to every
// platform admin, sent by the worker after the commit.
func (s *service) RequestClubSensitivity(ctx context.Context, a authz.Actor, clubID uuid.UUID, reason SensitiveReason, note string) error {
	c, err := s.GetClub(ctx, clubID)
	if err != nil {
		return err
	}
	if err := authz.Can(ctx, a, authz.DirectoryClubRequestSensitivity, clubResource(c)); err != nil {
		return err
	}
	if !reason.Valid() {
		return fieldErr("body.sensitive_reason", "unknown reason")
	}
	n, err := cleanOptional("body.note", note, maxNote)
	if err != nil {
		return err
	}
	if c.IsSensitive {
		return ErrInvalidState
	}
	if err := s.writableOrg(ctx, a, c.OrgID); err != nil {
		return err
	}
	hash, err := idempotency.Hash(struct {
		Op     string
		Club   uuid.UUID
		Reason SensitiveReason
		Note   string
	}{"request_sensitivity", clubID, reason, deref(n)})
	if err != nil {
		return err
	}
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		req := uuid.NewV7()
		org := c.OrgID
		if err := s.d.Audit.Log(ctx, auditEvent(a, authz.DirectoryClubRequestSensitivity, authz.TypeClub, c.OrgID, &org, deref(n),
			map[string]any{"sensitive_reason": string(reason), "request_id": req.String()})); err != nil {
			return uuid.Nil(), err
		}
		s.d.Queue.AfterCommit(ctx, SensitivityRequestTaskName,
			sensitivityRequest{RequestID: req, ClubOrgID: c.OrgID}, SensitivityRequestTaskName+":"+req.String())
		return req, nil
	})
	return err
}
