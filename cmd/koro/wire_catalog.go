package main

import (
	"context"
	"errors"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/catalog"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/platform/db"
)

// catalogWiring wires the catalog module (L9; 01 §4.17). First owner: BE-038.
// Only tickets of the catalog module edit this file (BE-001 R4).
//
// Seams (nil in production):
//
//	store     nil → catalog.NewGormStore over the app_rw pool
//	newStore  tests: builds the in-memory store over the container clock
//	tx        nil → the app_rw pool's TxManager; tests preset db.NopTx
//
// Consumed: c.audit.svc, c.orgs.svc (the seller org's kind, status and
// name; the seller's admins and the university's reps of the notices),
// c.directory.svc (a club's university, the orgs of a university; nil →
// no university approval, an empty university filter), c.events.svc (the
// bound event; nil → binding an event is 503), c.parties.payees (the
// default payee, BE-037's active payee with an approved account at
// submission and approval; nil → those are 503), c.identity.svc
// (re-authentication of catalog.policy.manage; the notice addresses),
// identity's store (the finance admins in effect, 06 H-33) and
// c.notify.platformAdmins (the platform admins), c.notify.svc (the
// notices, read at call time), c.infra.outbox and c.infra.queue.
//
// Provided, set in build: svc (catalog.Service) and offerings
// (catalog.Offerings for orders, BE-040: ForCheckout, Reserve, Release,
// ResolvePolicy). buyerHeld is the late binding of the per-buyer limit:
// orders (BE-040, a higher layer) sets it in its build; catalog reads it
// at call time (nil → 0: without orders nothing was ever reserved).
// No KVKK provider: the catalog tables hold no personal data (02 §18;
// personal_tables.txt classes them none/staff).
type catalogWiring struct {
	store    catalog.Store
	newStore func(c *container) catalog.Store
	tx       db.TxManager

	mod       catalog.Module
	svc       catalog.Service   // set by build
	offerings catalog.Offerings // set by build
	built     catalog.Store     // the store build used (wiring and matrix tests seed it)
	buyerHeld func(ctx context.Context, offeringID, buyerOrgID uuid.UUID) (int, error)
}

func (w *catalogWiring) build(c *container) error {
	store, tx := w.store, w.tx
	if w.newStore != nil {
		store = w.newStore(c)
	}
	if c.infra.db != nil {
		if store == nil {
			store = catalog.NewGormStore(c.infra.db.ORM)
		}
		if tx == nil {
			tx = c.infra.db.Tx
		}
	}
	if store == nil || tx == nil {
		return errors.New("catalog: no store (database not opened)")
	}
	if c.orgs.svc == nil {
		return errors.New("catalog: orgs is not built")
	}
	d := catalog.Deps{
		Store: store, Tx: tx, Audit: c.audit.svc, Idempotency: c.infra.idempotency, Orgs: c.orgs.svc,
		Outbox: c.infra.outbox, Queue: c.infra.queue, Clock: c.infra.clock, Log: c.infra.log.With("module", "catalog"),
		Sender: catalogSender{c: c}, Staff: catalogStaff{c: c},
		BuyerHeld: func(ctx context.Context, offeringID, buyerOrgID uuid.UUID) (int, error) {
			if w.buyerHeld == nil {
				return 0, nil
			}
			return w.buyerHeld(ctx, offeringID, buyerOrgID)
		},
	}
	if svc := c.directory.svc; svc != nil {
		d.Directory = svc
	}
	if svc := c.events.svc; svc != nil {
		d.Events = svc
	}
	if p := c.parties.payees; p != nil {
		d.Payees = p
	}
	if svc := c.identity.svc; svc != nil {
		d.Reauth, d.Users = svc, svc
	}
	mod, err := catalog.New(d)
	if err != nil {
		return err
	}
	w.mod, w.svc, w.offerings, w.built = mod, mod.Service, mod.Offerings, store
	return nil
}

// catalogSender adapts notify (L3), read at call time; a missing sender
// fails the task (retried), it never drops a notice.
type catalogSender struct{ c *container }

func (s catalogSender) Send(ctx context.Context, m notify.Message) (uuid.UUID, error) {
	if s.c.notify.svc == nil {
		return uuid.Nil(), errors.New("catalog: notify is not wired")
	}
	return s.c.notify.svc.Send(ctx, m)
}

// catalogStaff lists the platform staff in effect (06 H-33) for
// catalog.Staff: the platform admins from the notify hook identity fills,
// the finance admins from identity's store through the composition root
// (identity exposes PlatformAdmins only; T-277, the parties precedent).
type catalogStaff struct{ c *container }

func (s catalogStaff) PlatformAdmins(ctx context.Context) ([]uuid.UUID, error) {
	if h := s.c.notify.platformAdmins; h != nil {
		return h(ctx)
	}
	return nil, errors.New("catalog: the platform admin lookup is not wired")
}

func (s catalogStaff) FinanceAdmins(ctx context.Context) ([]uuid.UUID, error) {
	st := s.c.identity.store
	if st == nil {
		if s.c.infra.db == nil {
			return nil, errors.New("catalog: the finance admin lookup has no identity store")
		}
		st = identity.NewGormStore(s.c.infra.db.ORM)
	}
	return st.UsersWithRole(ctx, authz.PlatformFinanceAdmin)
}

// tasks: event.cancelled → catalog.cancel_event_offerings (critical);
// offering.submitted → catalog.notify_offering_submitted, .decided →
// catalog.notify_offering_decided, .cancelled →
// catalog.notify_offering_cancelled (01 §6.2); the task
// catalog.notify_policy_changed (06 H-17: every platform and finance admin).
func (w *catalogWiring) tasks(c *container) {
	w.mod.Subscribe(c.infra.subs)
	for _, t := range w.mod.Tasks() {
		c.infra.jobs.Handle(t)
	}
}

// routes: the 17 catalog operations (04 §2.14); w.svc is nil under `koro
// openapi`, which Register tolerates.
func (w *catalogWiring) routes(c *container, api huma.API) { catalog.Register(api, w.svc) }
