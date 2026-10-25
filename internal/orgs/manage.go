package orgs

import (
	"context"
	"errors"
	"uuid"

	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/httpx"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/outbox"
	"koro/internal/platform/problem"
)

// OrgPatch is the body of updateOrg (04 §2.2): nil leaves a field as it
// is, an empty string clears an optional one. The company fields apply to
// companies only.
type OrgPatch struct {
	Name        *string
	Description *string
	Logo        *uuid.UUID // a new logo (files purpose org_logo)
	ClearLogo   bool
	WebsiteURL  *string
	City        *string

	SectorCode  *string
	SizeBand    *string
	EmailDomain *string
	HQCity      *string
}

func (p OrgPatch) companyFields() bool {
	return p.SectorCode != nil || p.SizeBand != nil || p.EmailDomain != nil || p.HQCity != nil
}

func (p OrgPatch) empty() bool {
	return p.Name == nil && p.Description == nil && p.Logo == nil && !p.ClearLogo && p.WebsiteURL == nil &&
		p.City == nil && !p.companyFields()
}

// writable is the org status rule of writes on behalf of an org (03 §2.3):
// a closed org takes no change, a suspended one only a platform admin's.
func writable(a authz.Actor, o Org) error {
	switch o.Status {
	case StatusClosed:
		return ErrInvalidState
	case StatusSuspended:
		if !a.HasPlatformRole(authz.PlatformAdmin) {
			return ErrOrgSuspended
		}
	}
	return nil
}

// UpdateOrg implements Service. Order: the org (404) → authz (404/403) →
// status (403 orgs.org_suspended, 409) → If-Match (428/412) → the patch;
// a real change writes org.updated (O) in the transaction (BE-106).
func (s *service) UpdateOrg(ctx context.Context, a authz.Actor, id uuid.UUID, ifMatch string, p OrgPatch) (OrgView, error) {
	var out Org
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		o, err := s.d.Store.LockOrg(ctx, id)
		if err != nil {
			return err
		}
		if err := authz.Can(ctx, a, authz.OrgsOrgUpdate, authz.OrgResource(o.ID, string(o.Kind), string(o.Status))); err != nil {
			return err
		}
		if err := writable(a, o); err != nil {
			return err
		}
		if err := httpx.CheckIfMatch(ifMatch, httpx.ETag(o.UpdatedAt)); err != nil {
			return err
		}
		if p.empty() {
			return ErrEmptyPatch
		}
		next, changed, err := s.applyPatch(ctx, a, o, p)
		if err != nil {
			return err
		}
		if out, err = s.d.Store.UpdateOrg(ctx, next); err != nil {
			return err
		}
		if p.companyFields() {
			cc, err := s.patchCompany(ctx, o, p)
			if err != nil {
				return err
			}
			changed = append(changed, cc...)
		}
		if err := s.emitOrgUpdated(ctx, o, changed); err != nil {
			return err
		}
		return s.d.Audit.Log(ctx, auditEvent(a, authz.OrgsOrgUpdate, o.ID, "", map[string]any{"fields": changed}))
	})
	if err != nil {
		return OrgView{}, err
	}
	return s.view(ctx, out)
}

// applyPatch validates the org fields of p and returns the new row and the
// names of the changed fields (audit metadata: names only, no values).
func (s *service) applyPatch(ctx context.Context, a authz.Actor, o Org, p OrgPatch) (Org, []string, error) {
	next, changed := o, []string{}
	if p.Name != nil {
		n, err := cleanName("body.name", *p.Name)
		if err != nil {
			return Org{}, nil, err
		}
		next.Name, changed = n, append(changed, "name")
	}
	if p.Description != nil {
		d, err := cleanDescription("body.description", *p.Description)
		if err != nil {
			return Org{}, nil, err
		}
		next.Description, changed = d, append(changed, "description")
	}
	if p.WebsiteURL != nil {
		w, err := cleanWebsite("body.website_url", *p.WebsiteURL)
		if err != nil {
			return Org{}, nil, err
		}
		next.WebsiteURL, changed = w, append(changed, "website_url")
	}
	if p.City != nil {
		c, err := cleanLine("body.city", *p.City, maxCity)
		if err != nil {
			return Org{}, nil, err
		}
		next.City, changed = c, append(changed, "city")
	}
	switch {
	case p.Logo != nil:
		if err := s.attachLogo(ctx, a, o, *p.Logo); err != nil {
			return Org{}, nil, err
		}
		logo := *p.Logo
		next.LogoFileID, changed = &logo, append(changed, "logo_file_id")
	case p.ClearLogo:
		next.LogoFileID, changed = nil, append(changed, "logo_file_id")
	}
	if p.companyFields() && o.Kind != KindCompany {
		return Org{}, nil, ErrCompanyFieldOnly
	}
	return next, changed, nil
}

// patchCompany writes the company fields of p.
func (s *service) patchCompany(ctx context.Context, o Org, p OrgPatch) ([]string, error) {
	c, err := s.d.Store.GetCompany(ctx, o.ID)
	if errors.Is(err, ErrNotFound) {
		if err := s.d.Store.InsertCompany(ctx, Company{OrgID: o.ID}); err != nil {
			return nil, err
		}
		c, err = s.d.Store.GetCompany(ctx, o.ID)
	}
	if err != nil {
		return nil, err
	}
	val := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	cf, err := cleanCompany(val(p.SectorCode), val(p.SizeBand), val(p.EmailDomain), val(p.HQCity), false)
	if err != nil {
		return nil, err
	}
	var changed []string
	if p.SectorCode != nil {
		c.SectorCode, changed = cf.sector, append(changed, "sector_code")
	}
	if p.SizeBand != nil {
		c.SizeBand, changed = cf.size, append(changed, "size_band")
	}
	if p.EmailDomain != nil {
		if !equalPtr(c.EmailDomain, cf.domain) {
			c.DomainVerifiedAt = nil // a new domain is not verified
		}
		c.EmailDomain, changed = cf.domain, append(changed, "email_domain")
	}
	if p.HQCity != nil {
		c.HQCity, changed = cf.hq, append(changed, "hq_city")
	}
	if _, err := s.d.Store.UpdateCompany(ctx, c); err != nil {
		return nil, err
	}
	return changed, nil
}

func equalPtr(a, b *string) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }

// attachLogo checks that fileID is an org_logo upload of this org (or of
// the caller) and attaches it to the org in the transaction (files k30:
// the upload was authorised for org_logo already).
func (s *service) attachLogo(ctx context.Context, a authz.Actor, o Org, fileID uuid.UUID) error {
	if s.d.Files == nil {
		return problem.ErrUnavailable
	}
	bad := problem.Field("body.logo_file_id", "must be an org_logo upload of this organization")
	f, err := s.d.Files.Get(ctx, fileID)
	switch {
	case errors.Is(err, files.ErrNotFound):
		return bad
	case err != nil:
		return err
	}
	ownOrg := f.OrgID != nil && *f.OrgID == o.ID
	ownUpload := f.UploadedBy != nil && *f.UploadedBy == a.UserID
	if f.Purpose != files.PurposeOrgLogo || !ownOrg && !ownUpload {
		return bad
	}
	return s.d.Files.Attach(ctx, a, fileID, files.Owner{Module: "orgs", EntityID: o.ID, Purpose: files.PurposeOrgLogo})
}

// orgAggType is the aggregate type of org.updated.
const orgAggType = "org"

// emitOrgUpdated writes org.updated (O) in the caller's transaction: the
// public profile of o changed (BE-106: the web revalidates its page).
// Nothing changed → no event.
func (s *service) emitOrgUpdated(ctx context.Context, o Org, fields []string) error {
	if len(fields) == 0 {
		return nil
	}
	return outbox.Emit(ctx, s.d.Outbox, domainevent.OrgUpdated{OrgID: o.ID, OrgKind: string(o.Kind), Fields: fields}, orgAggType, o.ID)
}

// openCommerce returns the closeOrg checks bound at call time.
func (s *service) openCommerce() []OpenCommerceCheck {
	if s.d.OpenCommerce == nil {
		return nil
	}
	return s.d.OpenCommerce()
}

// CloseOrg implements Service: authz (404/403) → reason → re-authentication
// → in one transaction: status rules, the open-commerce checks, closed +
// closed_at, every open membership ended (org_closed), audit.
func (s *service) CloseOrg(ctx context.Context, a authz.Actor, id uuid.UUID, reason string, proof reauthProof) (OrgView, error) {
	o, err := s.d.Store.GetOrg(ctx, id)
	if err != nil {
		return OrgView{}, err
	}
	if err := authz.Can(ctx, a, authz.OrgsOrgClose, authz.OrgResource(o.ID, string(o.Kind), string(o.Status))); err != nil {
		return OrgView{}, err
	}
	reason, err = cleanReason("body.reason", reason)
	if err != nil {
		return OrgView{}, err
	}
	kind, err := s.reauth(ctx, a, proof, reauthDenied(authz.OrgsOrgClose, o.ID, true, nil))
	if err != nil {
		return OrgView{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op     string
		Org    uuid.UUID
		Reason string
	}{"close", id, reason})
	if err != nil {
		return OrgView{}, err
	}
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		o, err := s.d.Store.LockOrg(ctx, id)
		if err != nil {
			return uuid.Nil(), err
		}
		if o.Kind == KindPlatform {
			return uuid.Nil(), ErrInvalidState
		}
		if err := writable(a, o); err != nil {
			return uuid.Nil(), err
		}
		for _, check := range s.openCommerce() {
			open, err := check(ctx, o.ID)
			if err != nil {
				return uuid.Nil(), err
			}
			if open {
				return uuid.Nil(), ErrOpenCommerce
			}
		}
		now := s.now()
		o.Status, o.ClosedAt = StatusClosed, &now
		if _, err := s.d.Store.UpdateOrg(ctx, o); err != nil {
			return uuid.Nil(), err
		}
		if err := s.emitOrgUpdated(ctx, o, []string{"status"}); err != nil {
			return uuid.Nil(), err
		}
		gone, err := s.d.Store.EndOpenMembers(ctx, o.ID, now, EndOrgClosed)
		if err != nil {
			return uuid.Nil(), err
		}
		// A sensitive club's ended memberships are deleted at once (02 §7.3).
		sens, err := s.sensitive(ctx, o.ID, o.Kind)
		if err != nil {
			return uuid.Nil(), err
		}
		for i := 0; sens && i < len(gone); i++ {
			if err := s.d.Store.DeleteMember(ctx, o.ID, gone[i].ID); err != nil {
				return uuid.Nil(), err
			}
		}
		meta := map[string]any{"reauth": kind, "ended_memberships": len(gone)}
		if err := s.d.Audit.Log(ctx, auditEvent(a, authz.OrgsOrgClose, o.ID, reason, meta)); err != nil {
			return uuid.Nil(), err
		}
		for _, m := range gone {
			if m.Status == MemberActive {
				s.emitEnded(ctx, m, EndOrgClosed)
			}
		}
		return o.ID, nil
	})
	if err != nil {
		return OrgView{}, err
	}
	return s.reload(ctx, id)
}

// TransferOwnership implements Service: authz → re-authentication → in one
// transaction: the current owner's row and the target's company_admin row
// end (role_changed), the target gets a company_owner row and the former
// owner a company_admin row (02 §7.3: a role change keeps the history).
// Both users' sessions are closed after the commit (03 §2.2-6).
func (s *service) TransferOwnership(ctx context.Context, a authz.Actor, id, newOwner uuid.UUID, proof reauthProof) (OrgView, error) {
	o, err := s.d.Store.GetOrg(ctx, id)
	if err != nil {
		return OrgView{}, err
	}
	if err := authz.Can(ctx, a, authz.OrgsOwnershipTransfer, authz.OrgResource(o.ID, string(o.Kind), string(o.Status))); err != nil {
		return OrgView{}, err
	}
	if newOwner == uuid.Nil() {
		return OrgView{}, problem.Field("body.new_owner_user_id", "is required")
	}
	kind, err := s.reauth(ctx, a, proof, reauthDenied(authz.OrgsOwnershipTransfer, o.ID, true, nil))
	if err != nil {
		return OrgView{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op         string
		Org, Owner uuid.UUID
	}{"transfer", id, newOwner})
	if err != nil {
		return OrgView{}, err
	}
	_, _, err = idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		o, err := s.d.Store.LockOrg(ctx, id)
		if err != nil {
			return uuid.Nil(), err
		}
		if o.Kind != KindCompany {
			return uuid.Nil(), ErrInvalidState
		}
		if err := writable(a, o); err != nil {
			return uuid.Nil(), err
		}
		owner, err := s.d.Store.LockOwner(ctx, o.ID)
		if errors.Is(err, ErrNotFound) {
			return uuid.Nil(), ErrNoOwner
		}
		if err != nil {
			return uuid.Nil(), err
		}
		if owner.UserID == newOwner {
			return uuid.Nil(), ErrTransferTarget
		}
		target, err := s.d.Store.LockActiveMember(ctx, o.ID, newOwner)
		if errors.Is(err, ErrNotFound) || err == nil && target.Role != authz.MemberCompanyAdmin {
			return uuid.Nil(), ErrTransferTarget
		}
		if err != nil {
			return uuid.Nil(), err
		}
		now, by := s.now(), a.UserID
		var ended, started []Membership
		for _, m := range []Membership{owner, target} {
			e, err := s.d.Store.EndMember(ctx, o.ID, m.ID, now, EndRoleChanged)
			if err != nil {
				return uuid.Nil(), err
			}
			ended = append(ended, e)
		}
		for _, r := range []struct {
			user uuid.UUID
			role authz.MemberRole
		}{{newOwner, ownerRole}, {owner.UserID, authz.MemberCompanyAdmin}} {
			m, err := s.d.Store.InsertMember(ctx, Membership{
				ID: uuid.NewV7(), OrgID: o.ID, UserID: r.user, Role: r.role, Status: MemberActive, Source: SourceAdmin,
				JoinedAt: &now, ApprovedBy: &by,
			})
			if err != nil {
				return uuid.Nil(), err
			}
			started = append(started, m)
		}
		meta := map[string]any{
			"reauth": kind, "previous_owner_user_id": owner.UserID.String(), "new_owner_user_id": newOwner.String(),
		}
		if err := s.d.Audit.Log(ctx, auditEvent(a, authz.OrgsOwnershipTransfer, o.ID, "", meta)); err != nil {
			return uuid.Nil(), err
		}
		for _, m := range ended {
			s.emitEnded(ctx, m, EndRoleChanged)
		}
		for _, m := range started {
			s.emitStarted(ctx, m)
		}
		s.revokeAfterCommit(ctx, newOwner)
		s.revokeAfterCommit(ctx, owner.UserID)
		return o.ID, nil
	})
	if err != nil {
		return OrgView{}, err
	}
	return s.reload(ctx, id)
}

// reload reads the org after a write and builds its view.
func (s *service) reload(ctx context.Context, id uuid.UUID) (OrgView, error) {
	o, err := s.d.Store.GetOrg(ctx, id)
	if err != nil {
		return OrgView{}, err
	}
	return s.view(ctx, o)
}
