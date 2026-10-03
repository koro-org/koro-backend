package main

import (
	"context"
	"errors"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/entitlements"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/problem"
)

// entitlementsWiring wires the entitlements module (L2; 01 §4.4). Owner:
// BE-013. Only tickets of the entitlements module edit this file (BE-001 R4).
//
// Seams (nil in production):
//
//	store  nil → entitlements.NewGormStore over the app_rw pool; tests preset the in-memory store
//	tx     nil → the app_rw pool's TxManager; tests preset db.NopTx
//	orgKind the org kind lookup of adminGrantEntitlement (06 H-40, T-148:
//	       rights go to companies only). orgs (L5) builds after
//	       entitlements, so the binding is late: BE-017 sets
//	       c.entitlements.orgKind in wire_orgs.go build. Read at call time;
//	       nil → the grant fails closed (503).
//
// Consumers (01 §6.3, same transaction): feed (targeted_post), outreach
// (targeted_dispatch) and messaging (contact_request) adapt svc to their
// Consume port; discovery reads Available (discovery_access); orders
// (BE-040) Grant/ActivateBySource/RevokeBySource. All of them are higher
// layers and read c.entitlements.svc in their own build.
type entitlementsWiring struct {
	store   entitlements.Store
	tx      db.TxManager
	orgKind func(ctx context.Context, orgID uuid.UUID) (kind string, found bool, err error)

	mod entitlements.Module
	svc entitlements.Service // set by build
	clk clock.Clock          // the container's clock (matrix fixtures date their rights with it)
}

func (w *entitlementsWiring) build(c *container) error {
	store, tx := w.store, w.tx
	if c.infra.db != nil {
		if store == nil {
			store = entitlements.NewGormStore(c.infra.db.ORM)
		}
		if tx == nil {
			tx = c.infra.db.Tx
		}
	}
	if store == nil || tx == nil {
		return errors.New("entitlements: no store (database not opened)")
	}
	mod, err := entitlements.New(entitlements.Deps{
		Store: store, Tx: tx, Audit: c.audit.svc, Idempotency: c.infra.idempotency,
		Clock: c.infra.clock, Log: c.infra.log.With("module", "entitlements"),
		OrgKind: func(ctx context.Context, orgID uuid.UUID) (string, bool, error) {
			if w.orgKind == nil {
				return "", false, problem.ErrUnavailable
			}
			return w.orgKind(ctx, orgID)
		},
	})
	if err != nil {
		return err
	}
	w.mod, w.svc, w.clk = mod, mod.Service, c.infra.clock
	return nil
}

// tasks schedules entitlements.expire (01 §7: hourly at :45, default).
func (w *entitlementsWiring) tasks(c *container) {
	for _, j := range w.mod.Jobs() {
		c.infra.jobs.Schedule(j)
	}
}

// routes: listOrgEntitlements, getOrgEntitlement, listOrgEntitlementUsages,
// adminGrantEntitlement, adminRevokeEntitlement (04 §2.11).
func (w *entitlementsWiring) routes(c *container, api huma.API) { entitlements.Register(api, w.svc) }
