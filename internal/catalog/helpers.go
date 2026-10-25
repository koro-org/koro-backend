package catalog

import (
	"context"
	"errors"
	"strconv"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/directory"
	"koro/internal/events"
	"koro/internal/orgs"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
	"koro/internal/platform/problem"
)

// Audit resource types and outbox aggregate.
const (
	auditOffering = authz.TypeOffering
	auditService  = "service"
	auditPolicy   = "commerce_policy"
)

// adminSurface is the authz view of /admin/** (03 §3.2: 403 before reading).
var adminSurface = authz.Resource{Type: authz.TypeAdminSurface}

// offeringFacts are what authz needs beyond the row: the seller club's
// university (03 §3.5 university_id; the org ID for the U cells).
type offeringFacts struct {
	universityID    uuid.UUID // directory universities.id ("" when none)
	universityOrgID uuid.UUID
}

// offeringResource is the authz view of an offering (03 §3.5, BE-038 Attrs).
func offeringResource(o Offering, f offeringFacts) authz.Resource {
	org := o.OrgID
	str := func(id uuid.UUID) string {
		if id == uuid.Nil() {
			return ""
		}
		return id.String()
	}
	return authz.Resource{Type: authz.TypeOffering, ID: o.ID, OrgID: &org, Attrs: map[string]string{
		authz.AttrStatus:                   string(o.Status),
		"university_id":                    str(f.universityID),
		"seller_kind":                      string(o.SellerKind),
		authz.AttrOfferingPublished:        strconv.FormatBool(o.Published()),
		authz.AttrOfferingUniversityOrg:    str(f.universityOrgID),
		authz.AttrOfferingUniversityReview: strconv.FormatBool(o.UniversityReview()),
		authz.AttrCreatedBy:                o.CreatedBy.String(),
	}}
}

// serviceResource is a catalog_service (k23: accounting is finance's).
func serviceResource(id uuid.UUID, accounting bool) authz.Resource {
	return authz.Resource{Type: authz.TypeCatalogService, ID: id, Attrs: map[string]string{
		authz.AttrAccountingReview: strconv.FormatBool(accounting),
	}}
}

// staff reports whether a acts as platform staff of the catalogue
// (platform or finance admin): every service, the management fields of
// every offering.
func staff(a authz.Actor) bool {
	return a.HasPlatformRole(authz.PlatformAdmin) || a.HasPlatformRole(authz.PlatformFinanceAdmin)
}

// manager reports whether a manages the seller org's offerings (its
// club_admin, university_rep or career_staff) or is platform staff: the
// caller sees the management fields (payee, stock, approval).
func manager(a authz.Actor, o Offering) bool {
	return staff(a) || a.HasOrgRole(o.OrgID, authz.MemberClubAdmin, authz.MemberUniversityRep, authz.MemberCareerStaff)
}

// facts resolves the seller club's university (none for a university, the
// platform, or without directory).
func (s *service) facts(ctx context.Context, o Offering) (offeringFacts, error) {
	return s.universityOf(ctx, o.OrgID)
}

// universityOf returns the university of a club org (zero for any other org).
func (s *service) universityOf(ctx context.Context, orgID uuid.UUID) (offeringFacts, error) {
	if s.d.Directory == nil {
		return offeringFacts{}, nil
	}
	c, err := s.d.Directory.GetClub(ctx, orgID)
	switch {
	case errors.Is(err, directory.ErrNotFound):
		return offeringFacts{}, nil
	case err != nil:
		return offeringFacts{}, err
	}
	u, err := s.d.Directory.GetUniversity(ctx, c.UniversityID)
	switch {
	case errors.Is(err, directory.ErrNotFound):
		return offeringFacts{}, nil
	case err != nil:
		return offeringFacts{}, err
	}
	return offeringFacts{universityID: u.ID, universityOrgID: u.OrgID}, nil
}

// org returns the org of an org-surface request (404 when it is unknown).
func (s *service) org(ctx context.Context, orgID uuid.UUID) (orgs.OrgSummary, error) {
	o, err := s.d.Orgs.Get(ctx, orgID)
	if errors.Is(err, orgs.ErrNotFound) {
		return orgs.OrgSummary{}, ErrNotFound
	}
	return o, err
}

// writableOrg is the org status rule of writes on behalf of a seller (03
// §2.3): a closed org takes no change, a suspended one only platform
// staff's.
func writableOrg(a authz.Actor, o orgs.OrgSummary) error {
	switch o.Status {
	case orgs.StatusClosed:
		return orgs.ErrInvalidState
	case orgs.StatusSuspended:
		if !staff(a) {
			return orgs.ErrOrgSuspended
		}
	}
	return nil
}

// offeringEvent is the audit record of an offering write by a. Metadata
// carries states and field names, never free text (05 §11.2).
func offeringEvent(a authz.Actor, act authz.Action, o Offering, reason string, meta map[string]any) audit.Event {
	id, org := o.ID, o.OrgID
	if meta == nil {
		meta = map[string]any{}
	}
	meta["status"] = string(o.Status)
	return audit.Event{
		Action: string(act), Outcome: audit.Success, OrgID: &org, ResourceType: auditOffering, ResourceID: &id,
		Reason: reason, Metadata: meta,
	}.By(a)
}

// emitAfterCommit publishes an A event of the offering (01 §6.2:
// offering.submitted, offering.decided) once the transaction commits.
func (s *service) emitAfterCommit(ctx context.Context, o Offering, p domainevent.Payload) error {
	return outbox.EmitAfterCommit(ctx, s.d.Outbox, p, auditOffering, o.ID)
}

// emitOutbox writes an O event of the offering (offering.cancelled) in the
// transaction carried by ctx.
func (s *service) emitOutbox(ctx context.Context, o Offering, p domainevent.Payload) error {
	return outbox.Emit(ctx, s.d.Outbox, p, auditOffering, o.ID)
}

func fieldError(loc, msg string) problem.FieldError {
	return problem.FieldError{Location: loc, Message: msg}
}

func isEventNotFound(err error) bool { return errors.Is(err, events.ErrNotFound) }

func isOrgNotFound(err error) bool { return errors.Is(err, orgs.ErrNotFound) }
