// Package entitlements holds the feature rights of organisations (01 §4.4;
// BE-013): quotas of targeted posts, notification dispatches and contact
// requests, and time-boxed discovery access. Deliverables (stand, logo) are
// order lines, not entitlements (01 §2.1).
//
// Race-free quota (02 §10.1): Consume runs one conditional UPDATE per
// candidate entitlement (active, inside its validity window, enough units
// left; earliest valid_to first). Concurrent consumptions serialise on the
// row, so N callers on K remaining units get exactly K successes. The usage
// row and the quota change are written in the CALLER's transaction (01 §6.3:
// feed publishing a targeted post, outreach scheduling a dispatch,
// messaging creating a contact request): the caller opens WithTx, calls
// Consume with that ctx and writes its own record; a rollback gives the
// unit back.
//
// Idempotency: a Ref (the caller's record) consumes at most once
// (entitlement_usages_ref_uq); consuming it again returns the first usage.
// Release appends a negative reversal row and gives the units back.
//
// In the pilot rights are granted by hand (adminGrantEntitlement, packages
// sold offline, R-004d); orders grant pending rights and activate them on
// payment from BE-040 on (Grant + ActivateBySource).
package entitlements

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/queue"
)

// Entitlements is the cross-module contract (01 §4.4). Consumers declare a
// port with the methods they use ("// sync-write:
// entitlements.Entitlements.<Method>", 01 §6.3); cmd/koro binds this
// service to it. Every method joins the transaction carried by ctx.
type Entitlements interface {
	// Grant writes a new entitlement (status pending or active) and returns
	// its ID. A source (order line) grants a feature once: granting it
	// again returns the first entitlement.
	Grant(ctx context.Context, in GrantInput) (uuid.UUID, error)
	// ActivateBySource activates the pending entitlements of a source (the
	// order was paid). Already active ones are left alone.
	ActivateBySource(ctx context.Context, t SourceType, sourceID uuid.UUID) error
	// RevokeBySource revokes the pending, active or exhausted entitlements
	// of a source (refund, unpaid order). Units already used stay used.
	RevokeBySource(ctx context.Context, t SourceType, sourceID uuid.UUID, reason string) error
	// Consume uses qty units of feature for ref and returns the usage ID.
	// No usable entitlement → ErrNoEntitlement (403
	// entitlements.not_entitled). The same ref again → the first usage ID;
	// a released ref → ErrUsageReleased. The actor of ctx (if a user) is
	// recorded as actor_user_id.
	Consume(ctx context.Context, orgID uuid.UUID, f Feature, qty int, ref Ref) (uuid.UUID, error)
	// Release reverses the consumption of ref (a negative usage row) and
	// gives its units back. A ref that consumed nothing, or was already
	// released, is a no-op.
	Release(ctx context.Context, ref Ref) error
	// Available returns what the org may still use of feature now: units
	// left (quota) or the end of its access (discovery_access). Read
	// endpoints gated by an access right check Entitled() and return
	// ErrNoEntitlement otherwise.
	Available(ctx context.Context, orgID uuid.UUID, f Feature) (Availability, error)
}

// Service is the entitlements service: the contract plus the HTTP use
// cases (04 §2.11). Every HTTP method calls authz.Can first (05 §5.3).
type Service interface {
	Entitlements
	// List is listOrgEntitlements (entitlements.entitlement.list).
	List(ctx context.Context, a authz.Actor, orgID uuid.UUID, q ListQuery) (EntitlementPage, error)
	// Get is getOrgEntitlement (entitlements.entitlement.read).
	Get(ctx context.Context, a authz.Actor, orgID, id uuid.UUID) (Entitlement, error)
	// ListUsages is listOrgEntitlementUsages (entitlements.usage.list).
	ListUsages(ctx context.Context, a authz.Actor, orgID uuid.UUID, q UsageQuery) (UsagePage, error)
	// AdminGrant is adminGrantEntitlement (entitlements.entitlement.grant;
	// Idempotency-Key required).
	AdminGrant(ctx context.Context, a authz.Actor, orgID uuid.UUID, in AdminGrantInput) (Entitlement, error)
	// AdminRevoke is adminRevokeEntitlement (entitlements.entitlement.revoke).
	AdminRevoke(ctx context.Context, a authz.Actor, id uuid.UUID, reason string) (Entitlement, error)
}

// Deps are the dependencies of the entitlements module; all but OrgKind
// are required.
type Deps struct {
	Store       Store
	Tx          db.TxManager
	Audit       AuditLogger         // admin grant/revoke and expiry, same transaction
	Idempotency *idempotency.Keeper // Idempotency-Key of adminGrantEntitlement (06 H-5)
	Clock       clock.Clock
	Log         *slog.Logger

	// OrgKind checks the target org of adminGrantEntitlement (06 H-40):
	// a missing org is 404, an org that is not a company 422
	// entitlements.org_not_company. Optional: nil keeps the pre-T-148
	// behaviour (only the foreign key: a missing org is 404); cmd/koro
	// always binds it.
	OrgKind OrgKindFunc
}

func (d Deps) validate() error {
	var errs []error
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"Store", d.Store != nil}, {"Tx", d.Tx != nil}, {"Audit", d.Audit != nil},
		{"Idempotency", d.Idempotency != nil}, {"Clock", d.Clock != nil}, {"Log", d.Log != nil},
	} {
		if !c.ok {
			errs = append(errs, fmt.Errorf("entitlements.Deps.%s is missing", c.name))
		}
	}
	return errors.Join(errs...)
}

// Module is what entitlements hands to the composition root.
type Module struct {
	Service Service

	svc *service
}

// New builds the entitlements module; a missing dependency is a wiring bug
// that stops the process at start.
func New(d Deps) (Module, error) {
	if err := d.validate(); err != nil {
		return Module{}, err
	}
	s := &service{d: d}
	return Module{Service: s, svc: s}, nil
}

// Jobs returns the scheduled jobs of entitlements (01 §7): entitlements.expire.
func (m Module) Jobs() []queue.Job { return []queue.Job{m.svc.expireJob()} }
