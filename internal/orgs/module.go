// Package orgs is the organisations module, first half (01 §4.7; BE-017):
// the single tenant table organizations (platform, university, club,
// company), the company profile, company registration (the founder becomes
// company_owner), profile updates with optimistic concurrency (ETag /
// If-Match), closing, ownership transfer and the platform admin's verify,
// suspend and reactivate. Memberships, invitations and role changes come
// with BE-019.
//
// Rules the package enforces:
//
//   - Org isolation (03 §3.6, §7.6): the management operations of an org
//     (updateOrg, closeOrg, transferOrgOwnership) are invisible (404) to
//     anyone who is not an active member of that org or platform staff; a
//     member without the role gets 403. The public profile of a
//     pending_review or closed org is visible only to its members and to
//     platform_admin, support and moderator.
//   - Scope columns come from the parent row (06 H-38): a company profile
//     and a membership take org_id and org_kind from their organizations
//     row, never from the request.
//   - A suspended org reads normally, but writes on its behalf are 403
//     orgs.org_suspended (03 §2.3); a platform admin may still act on it.
//   - R operations (closeOrg, transferOrgOwnership, adminSuspendOrg,
//     adminReactivateOrg) verify the re-authentication proof after
//     authz.Can and before any business data (03 §6.2).
//   - Personal data: organizations and companies hold none (02 §7.1–7.2;
//     tax number and MERSİS live in parties). A membership is personal
//     data; logs carry IDs only, never names or addresses.
package orgs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/outbox"
	"koro/internal/platform/privacy"
)

// OrgDirectory is the read contract of other modules (01 §4.7). This
// ticket ships Get and GetMany; MembershipsOf, HasRole and ActiveMemberIDs
// come with the membership lifecycle (BE-019).
type OrgDirectory interface {
	// Get returns the summary of an org (any status) or ErrNotFound.
	Get(ctx context.Context, orgID uuid.UUID) (OrgSummary, error)
	// GetMany returns the summaries of the ids that exist.
	GetMany(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]OrgSummary, error)
}

// OrgAdmin is the write contract of other modules (01 §4.7). This ticket
// ships Create (directory opens club and university orgs, 01 §6.3 T-78);
// GrantRole and EndRoles come with BE-019.
type OrgAdmin interface {
	// Create writes a new org (and the company profile of a company) in
	// the transaction carried by ctx, joining the caller's: a rollback of
	// the caller removes the org too. The caller audits its own action.
	Create(ctx context.Context, in CreateOrg) (uuid.UUID, error)
}

// Service is the orgs service: the cross-module contracts plus the HTTP use
// cases (04 §2.2). Every method that takes an actor calls authz.Can first
// (05 §5.3).
type Service interface {
	OrgDirectory
	OrgAdmin

	// GetOrg is getOrg (orgs.org.read, P).
	GetOrg(ctx context.Context, a authz.Actor, id uuid.UUID) (OrgView, error)
	// GetOrgBySlug is getOrgBySlug (orgs.org.read, P).
	GetOrgBySlug(ctx context.Context, a authz.Actor, slug string) (OrgView, error)
	// SearchOrgs is searchOrgs (orgs.org.search, P; rate class search).
	SearchOrgs(ctx context.Context, a authz.Actor, q SearchQuery) (OrgPage, error)
	// CreateCompany is createCompany (orgs.company.create): the company
	// starts pending_review, the caller becomes its company_owner and the
	// caller's sessions are closed with role_change after the commit.
	CreateCompany(ctx context.Context, a authz.Actor, in CompanyInput) (OrgView, error)
	// UpdateOrg is updateOrg (orgs.org.update): ifMatch is the If-Match
	// header (04 §1.13: missing → 428, stale → 412).
	UpdateOrg(ctx context.Context, a authz.Actor, id uuid.UUID, ifMatch string, p OrgPatch) (OrgView, error)
	// CloseOrg is closeOrg (orgs.org.close, R): 409 orgs.open_commerce
	// while an open-commerce check reports open orders or payouts.
	CloseOrg(ctx context.Context, a authz.Actor, id uuid.UUID, reason string, proof identity.ReauthProof) (OrgView, error)
	// TransferOwnership is transferOrgOwnership (orgs.ownership.transfer, R):
	// the new owner must be an active company_admin of the company.
	TransferOwnership(ctx context.Context, a authz.Actor, id, newOwner uuid.UUID, proof identity.ReauthProof) (OrgView, error)
	// AdminListOrgs is adminListOrgs (orgs.org.list_admin).
	AdminListOrgs(ctx context.Context, a authz.Actor, q AdminQuery) (OrgPage, error)
	// AdminVerifyOrg is adminVerifyOrg (orgs.org.verify): pending_review →
	// active with verified_at.
	AdminVerifyOrg(ctx context.Context, a authz.Actor, id uuid.UUID, note string) (OrgView, error)
	// AdminSuspendOrg is adminSuspendOrg (orgs.org.suspend, R): active →
	// suspended.
	AdminSuspendOrg(ctx context.Context, a authz.Actor, id uuid.UUID, reason string, proof identity.ReauthProof) (OrgView, error)
	// AdminReactivateOrg is adminReactivateOrg (orgs.org.suspend, R):
	// suspended → active.
	AdminReactivateOrg(ctx context.Context, a authz.Actor, id uuid.UUID, reason string, proof identity.ReauthProof) (OrgView, error)
}

// Deps are the dependencies of the orgs module. Store, Tx, Outbox, Audit,
// Idempotency, Clock and Log are required (New returns an error otherwise).
type Deps struct {
	Store       Store
	Tx          db.TxManager
	Outbox      outbox.Writer       // membership.started / membership.ended (A)
	Audit       AuditLogger         // every write, same transaction
	Idempotency *idempotency.Keeper // optional Idempotency-Key of the POST operations (06 H-5)
	Clock       clock.Clock
	Log         *slog.Logger

	// Reauth verifies the proof of the R operations (identity). nil →
	// they fail closed with 503 (problem.ErrUnavailable).
	Reauth Reauthenticator
	// Files attaches and serves the org logo. nil → logo_file_id is
	// refused (503) and no logo_url is returned.
	Files Files
	// RevokeSessions closes a user's sessions after a role change
	// (role_change). nil → nothing is revoked (logged).
	RevokeSessions SessionRevoker
	// OpenCommerce returns the open-commerce checks of closeOrg at call
	// time (hooks.openCommerce is filled by later layers). nil or empty →
	// no check.
	OpenCommerce func() []OpenCommerceCheck
	// UniversityOrgs resolves searchOrgs?university_id. nil → the filter
	// matches nothing.
	UniversityOrgs UniversityOrgs
}

func (d Deps) validate() error {
	var errs []error
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"Store", d.Store != nil}, {"Tx", d.Tx != nil}, {"Outbox", d.Outbox != nil}, {"Audit", d.Audit != nil},
		{"Idempotency", d.Idempotency != nil}, {"Clock", d.Clock != nil}, {"Log", d.Log != nil},
	} {
		if !c.ok {
			errs = append(errs, fmt.Errorf("orgs.Deps.%s is missing", c.name))
		}
	}
	return errors.Join(errs...)
}

// Module is what orgs hands to the composition root.
type Module struct {
	Service Service
	// Privacy is the KVKK provider (memberships and registered orgs of a
	// user; the org rows themselves hold no personal data).
	Privacy privacy.Provider
}

// New builds the orgs module; a missing dependency is a wiring bug that
// stops the process at start.
func New(d Deps) (Module, error) {
	if err := d.validate(); err != nil {
		return Module{}, err
	}
	s := &service{d: d}
	return Module{Service: s, Privacy: privacyProvider{s: s}}, nil
}

type service struct{ d Deps }

var _ Service = (*service)(nil)

// now is the service clock in the database's precision (microseconds).
func (s *service) now() time.Time { return s.d.Clock.Now().UTC().Truncate(time.Microsecond) }
