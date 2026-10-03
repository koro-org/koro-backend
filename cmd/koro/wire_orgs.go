package main

import (
	"context"
	"errors"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/identity"
	"koro/internal/orgs"
	"koro/internal/platform/db"
)

// orgsWiring wires the orgs module (L5; 01 §4.7). First owner: BE-017.
// Only tickets of the orgs module edit this file (BE-001 R4).
//
// Seams (nil in production):
//
//	store  nil → orgs.NewGormStore over the app_rw pool; tests preset the in-memory store
//	tx     nil → the app_rw pool's TxManager; tests preset db.NopTx
//	universityOrgs  searchOrgs?university_id: the orgs of a university
//	       (its own org and its clubs). directory (L6) builds after orgs
//	       and sets it in wire_directory.go (BE-020). Read at call time;
//	       nil → the filter matches nothing.
//
// Consumed: c.audit.svc, c.files.svc (org logo), c.identity.svc
// (re-authentication of the R operations; Sessions.RevokeAll after a role
// change, called after the commit) and hooks.openCommerce (read at call
// time: orders BE-040 and ledger BE-044 append to it in their build).
//
// Provided, all set in build: svc (orgs.OrgDirectory / orgs.OrgAdmin for
// directory and later layers), c.entitlements.orgKind (06 H-40: admin
// grants go to companies only) and the orgs KVKK provider. hooks.memberships
// (Actor memberships) and hooks.clubInfo consumption come with BE-019.
type orgsWiring struct {
	store orgs.Store
	tx    db.TxManager

	universityOrgs func(ctx context.Context, universityID uuid.UUID) ([]uuid.UUID, error)

	mod orgs.Module
	svc orgs.Service // set by build
}

func (w *orgsWiring) build(c *container) error {
	store, tx := w.store, w.tx
	if c.infra.db != nil {
		if store == nil {
			store = orgs.NewGormStore(c.infra.db.ORM)
		}
		if tx == nil {
			tx = c.infra.db.Tx
		}
	}
	if store == nil || tx == nil {
		return errors.New("orgs: no store (database not opened)")
	}
	d := orgs.Deps{
		Store: store, Tx: tx, Outbox: c.infra.outbox, Audit: c.audit.svc, Idempotency: c.infra.idempotency,
		Clock: c.infra.clock, Log: c.infra.log.With("module", "orgs"),
		OpenCommerce: func() []orgs.OpenCommerceCheck {
			out := make([]orgs.OpenCommerceCheck, 0, len(c.hooks.openCommerce))
			for _, h := range c.hooks.openCommerce {
				out = append(out, orgs.OpenCommerceCheck(h))
			}
			return out
		},
		UniversityOrgs: func(ctx context.Context, universityID uuid.UUID) ([]uuid.UUID, error) {
			if w.universityOrgs == nil {
				return nil, nil
			}
			return w.universityOrgs(ctx, universityID)
		},
	}
	if svc := c.identity.svc; svc != nil {
		d.Reauth = svc
		d.RevokeSessions = func(ctx context.Context, userID uuid.UUID) (int, error) {
			return svc.RevokeAll(ctx, userID, identity.RevokeRoleChange)
		}
	}
	if c.files.svc != nil {
		d.Files = c.files.svc
	}
	mod, err := orgs.New(d)
	if err != nil {
		return err
	}
	w.mod, w.svc = mod, mod.Service
	c.privacy = append(c.privacy, mod.Privacy)
	svc := mod.Service
	c.entitlements.orgKind = func(ctx context.Context, orgID uuid.UUID) (string, bool, error) {
		o, err := svc.Get(ctx, orgID)
		switch {
		case errors.Is(err, orgs.ErrNotFound):
			return "", false, nil
		case err != nil:
			return "", false, err
		}
		return string(o.Kind), true, nil
	}
	return nil
}

// tasks: none yet (orgs.admin_sweep, orgs.invitations_expire and
// orgs.ended_memberships_purge come with BE-019).
func (w *orgsWiring) tasks(c *container) {}

// routes: the 11 orgs operations of BE-017 (04 §2.2); w.svc is nil under
// `koro openapi`, which Register tolerates.
func (w *orgsWiring) routes(c *container, api huma.API) { orgs.Register(api, w.svc) }
