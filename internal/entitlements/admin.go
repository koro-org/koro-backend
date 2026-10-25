package entitlements

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
)

// ListQuery narrows listOrgEntitlements.
type ListQuery struct {
	Filter ListFilter
	Limit  int
	After  *uuid.UUID // the last ID of the previous page
}

// EntitlementPage is a page of entitlements with the org's availability
// summary (04 §2.11: available[{feature, remaining, access_until}]).
type EntitlementPage struct {
	Items     []Entitlement
	Next      *uuid.UUID // nil on the last page
	Available []Availability
}

// UsageQuery narrows listOrgEntitlementUsages.
type UsageQuery struct {
	Filter UsageFilter
	Limit  int
	After  *uuid.UUID
}

// UsagePage is a page of usage rows. ActorUserID of the items is set only
// for finance_admin and platform_admin (04 §2.11).
type UsagePage struct {
	Items []UsageView
	Next  *uuid.UUID
}

// AdminGrantInput is the body of adminGrantEntitlement (04 §2.11).
type AdminGrantInput struct {
	Feature    Feature
	Kind       Kind
	Quantity   *int
	ValidFrom  time.Time
	ValidTo    time.Time
	SourceType SourceType // admin_grant | promo
	Reason     string     // why (the offline sale, the campaign); audited
}

// entitlementResource is the authz view of an org's entitlements (03 §3.5:
// entitlement, OrgID = the holder). The org comes from the path; the org
// management surface hides it from non-members (404, 03 §3.6).
func entitlementResource(orgID uuid.UUID) authz.Resource {
	return authz.Resource{Type: authz.TypeEntitlement, ID: orgID, OrgID: &orgID}
}

// adminSurface is the authz view of /admin/** (03 §3.2: 403 before reading).
var adminSurface = authz.Resource{Type: authz.TypeAdminSurface}

// seesActors reports whether the usage list may show who consumed (04
// §2.11: actor_user_id only for platform_admin and finance_admin).
func seesActors(a authz.Actor) bool {
	return a.HasPlatformRole(authz.PlatformAdmin) || a.HasPlatformRole(authz.PlatformFinanceAdmin)
}

// List implements Service.
func (s *service) List(ctx context.Context, a authz.Actor, orgID uuid.UUID, q ListQuery) (EntitlementPage, error) {
	if err := authz.Can(ctx, a, authz.EntitlementsEntitlementList, entitlementResource(orgID)); err != nil {
		return EntitlementPage{}, err
	}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.List(ctx, orgID, q.Filter, q.After, limit+1)
	if err != nil {
		return EntitlementPage{}, err
	}
	rows, more := page.Cut(rows, limit)
	out := EntitlementPage{Items: rows}
	if more {
		last := rows[len(rows)-1].ID
		out.Next = &last
	}
	now := s.d.Clock.Now()
	for _, f := range Features() {
		usable, err := s.d.Store.Usable(ctx, orgID, f, now)
		if err != nil {
			return EntitlementPage{}, err
		}
		out.Available = append(out.Available, summarize(f, usable))
	}
	return out, nil
}

// Get implements Service.
func (s *service) Get(ctx context.Context, a authz.Actor, orgID, id uuid.UUID) (Entitlement, error) {
	if err := authz.Can(ctx, a, authz.EntitlementsEntitlementRead, entitlementResource(orgID)); err != nil {
		return Entitlement{}, err
	}
	// Scoped by the path's org: another org's entitlement is not found.
	return s.d.Store.Get(ctx, orgID, id)
}

// ListUsages implements Service.
func (s *service) ListUsages(ctx context.Context, a authz.Actor, orgID uuid.UUID, q UsageQuery) (UsagePage, error) {
	if err := authz.Can(ctx, a, authz.EntitlementsUsageList, entitlementResource(orgID)); err != nil {
		return UsagePage{}, err
	}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.ListUsages(ctx, orgID, q.Filter, q.After, limit+1)
	if err != nil {
		return UsagePage{}, err
	}
	rows, more := page.Cut(rows, limit)
	if !seesActors(a) {
		for i := range rows {
			rows[i].ActorUserID = nil
		}
	}
	out := UsagePage{Items: rows}
	if more {
		last := rows[len(rows)-1].ID
		out.Next = &last
	}
	return out, nil
}

// checkAdminReason validates the free-text reason of an admin change.
func checkAdminReason(reason string) (string, error) {
	r := strings.TrimSpace(reason)
	switch n := utf8.RuneCountInString(r); {
	case n < 3:
		return "", problem.ErrValidation.WithFields(problem.FieldError{Location: "body.reason", Message: "must be at least 3 characters"})
	case n > maxReasonRunes:
		return "", problem.ErrValidation.WithFields(problem.FieldError{Location: "body.reason", Message: "must be at most 300 characters"})
	}
	return r, nil
}

// AdminGrant implements Service. The grant, its audit record and the
// Idempotency-Key share one transaction (06 H-5): a retried request
// replays the first entitlement instead of granting twice.
func (s *service) AdminGrant(ctx context.Context, a authz.Actor, orgID uuid.UUID, in AdminGrantInput) (Entitlement, error) {
	if err := authz.Can(ctx, a, authz.EntitlementsEntitlementGrant, adminSurface); err != nil {
		return Entitlement{}, err
	}
	if orgID == uuid.Nil() {
		return Entitlement{}, ErrNotFound
	}
	reason, err := checkAdminReason(in.Reason)
	if err != nil {
		return Entitlement{}, err
	}
	if in.SourceType != SourceAdminGrant && in.SourceType != SourcePromo {
		return Entitlement{}, grantErr("body.source_type", "must be admin_grant or promo")
	}
	if in.Kind != in.Feature.Kind() {
		return Entitlement{}, grantErr("body.kind", "does not match the feature (discovery_access is access, the others quota)")
	}
	now := s.d.Clock.Now()
	if !in.ValidTo.After(now) {
		return Entitlement{}, grantErr("body.valid_to", "must be in the future")
	}
	if err := s.checkCompany(ctx, orgID); err != nil {
		return Entitlement{}, err
	}
	admin := a.UserID
	g := GrantInput{
		OrgID: orgID, Feature: in.Feature, Quantity: in.Quantity, ValidFrom: in.ValidFrom, ValidTo: in.ValidTo,
		Status: StatusActive, SourceType: in.SourceType, GrantedBy: &admin,
	}
	if in.SourceType == SourcePromo {
		// A promotion has no record of its own: the grant is its source
		// (entitlements_source_chk wants a source_id).
		src := uuid.NewV7()
		g.SourceID = &src
	}
	if err := checkGrant(g, "body."); err != nil {
		return Entitlement{}, err
	}
	hash, err := idempotency.Hash(struct {
		Org        uuid.UUID
		Feature    Feature
		Kind       Kind
		Quantity   *int
		From, To   time.Time
		SourceType SourceType
		Reason     string
	}{orgID, in.Feature, in.Kind, in.Quantity, in.ValidFrom.UTC(), in.ValidTo.UTC(), in.SourceType, reason})
	if err != nil {
		return Entitlement{}, err
	}
	out, replayed, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (Entitlement, error) {
		e, err := s.grant(ctx, g)
		if err != nil {
			return Entitlement{}, err
		}
		meta := map[string]any{"feature": string(e.Feature), "kind": string(e.Kind), "source_type": string(e.SourceType),
			"valid_from": e.ValidFrom, "valid_to": e.ValidTo}
		if e.QuantityTotal != nil {
			meta["quantity_total"] = *e.QuantityTotal
		}
		return e, s.d.Audit.Log(ctx, audit.Event{
			Action: string(authz.EntitlementsEntitlementGrant), Outcome: audit.Success, OrgID: &e.OrgID,
			ResourceType: authz.TypeEntitlement, ResourceID: &e.ID, Reason: reason, Metadata: meta,
		}.By(a))
	})
	if err != nil {
		return Entitlement{}, err
	}
	if !replayed {
		// T-137: a single admin can grant paid rights; until a second
		// approval exists every grant is a WARN line for the alerting.
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "entitlement granted by admin",
			slog.String("entitlement_id", out.ID.String()), slog.String("org_id", out.OrgID.String()),
			slog.String("granted_by", admin.String()), slog.String("feature", string(out.Feature)))
	}
	return out, nil
}

// checkCompany is the org kind rule of an admin grant (06 H-40, T-148):
// rights are company features, a club or a university gets none.
func (s *service) checkCompany(ctx context.Context, orgID uuid.UUID) error {
	if s.d.OrgKind == nil {
		return nil
	}
	kind, found, err := s.d.OrgKind(ctx, orgID)
	switch {
	case err != nil:
		return err
	case !found:
		return ErrNotFound
	case kind != "company":
		return ErrOrgNotCompany
	}
	return nil
}

// AdminRevoke implements Service. Revoking an expired right is 409;
// revoking a revoked one returns it unchanged. A right bought with an
// order (order_line) is revoked too, but no refund is started: the
// response carries a warning (04 §2.11).
func (s *service) AdminRevoke(ctx context.Context, a authz.Actor, id uuid.UUID, reason string) (Entitlement, error) {
	if err := authz.Can(ctx, a, authz.EntitlementsEntitlementRevoke, adminSurface); err != nil {
		return Entitlement{}, err
	}
	reason, err := checkAdminReason(reason)
	if err != nil {
		return Entitlement{}, err
	}
	var out Entitlement
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		cur, err := s.d.Store.Lock(ctx, id)
		if err != nil {
			return err
		}
		switch cur.Status {
		case StatusRevoked:
			out = cur
			return nil
		case StatusExpired:
			return ErrInvalidState
		}
		out, err = s.d.Store.RevokeOne(ctx, id, s.d.Clock.Now(), reason)
		if errors.Is(err, ErrNotFound) {
			return ErrInvalidState
		}
		if err != nil {
			return err
		}
		return s.d.Audit.Log(ctx, audit.Event{
			Action: string(authz.EntitlementsEntitlementRevoke), Outcome: audit.Success, OrgID: &out.OrgID,
			ResourceType: authz.TypeEntitlement, ResourceID: &out.ID, Reason: reason,
			Metadata: map[string]any{"feature": string(out.Feature), "source_type": string(out.SourceType),
				"quantity_used": out.QuantityUsed, "previous_status": string(cur.Status)},
		}.By(a))
	})
	if err != nil {
		return Entitlement{}, err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "entitlement revoked by admin",
		slog.String("entitlement_id", out.ID.String()), slog.String("org_id", out.OrgID.String()))
	return out, nil
}
