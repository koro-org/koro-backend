package main

import (
	"context"
	"errors"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/orders"
	"koro/internal/platform/db"
)

// ordersWiring wires the orders module (L10; 01 §4.18). First owner: BE-040.
// Only tickets of the orders module edit this file (BE-001 R4).
//
// Seams (nil in production):
//
//	store     nil → orders.NewGormStore over the app_rw pool
//	newStore  tests: builds the in-memory store over the container clock
//	tx        nil → the app_rw pool's TxManager; tests preset db.NopTx
//
// Consumed: c.audit.svc, c.orgs.svc (the buying company, the seller names,
// the admins of the notices), c.catalog.offerings (ForCheckout,
// ResolvePolicy, Reserve, Release), c.parties.svc (the billed party) and
// c.parties.payees (DefaultFor, IsActive), c.entitlements.svc (Grant,
// ActivateBySource, RevokeBySource), hooks.collectionAccount (payments,
// BE-041; read at call time: nil → 409 payments.no_active_collection_account),
// c.identity.svc (the notice addresses), identity's store (the finance
// admins in effect, 06 H-33), c.notify.svc (read at call time), c.files.svc
// (the delivery evidence: Get, Attach, PresignGet; BE-093),
// c.infra.outbox, c.infra.queue (the notices of a dropped line, BE-134),
// c.infra.idempotency and c.infra.cfg.Orders.
//
// Provided, set in build: svc (orders.Service), orders (orders.Orders for
// payments, ledger and billing), catalog's per-buyer count
// (c.catalog.buyerHeld, BE-038's late binding), the closeOrg check
// (hooks.openCommerce) and the orders KVKK provider.
type ordersWiring struct {
	store    orders.Store
	newStore func(c *container) orders.Store
	tx       db.TxManager

	mod    orders.Module
	svc    orders.Service // set by build
	orders orders.Orders  // set by build
	built  orders.Store   // the store build used (wiring and matrix tests seed it)
	late   *container     // the container (matrix fixtures: the catalog store, the collection account hook)
}

func (w *ordersWiring) build(c *container) error {
	store, tx := w.store, w.tx
	if w.newStore != nil {
		store = w.newStore(c)
	}
	if c.infra.db != nil {
		if store == nil {
			store = orders.NewGormStore(c.infra.db.ORM)
		}
		if tx == nil {
			tx = c.infra.db.Tx
		}
	}
	if store == nil || tx == nil {
		return errors.New("orders: no store (database not opened)")
	}
	switch {
	case c.orgs.svc == nil:
		return errors.New("orders: orgs is not built")
	case c.catalog.offerings == nil:
		return errors.New("orders: catalog is not built")
	case c.parties.svc == nil || c.parties.payees == nil:
		return errors.New("orders: parties is not built")
	case c.entitlements.svc == nil:
		return errors.New("orders: entitlements is not built")
	case c.files.svc == nil:
		return errors.New("orders: files is not built")
	}
	d := orders.Deps{
		Store: store, Tx: tx, Audit: c.audit.svc, Idempotency: c.infra.idempotency, Orgs: c.orgs.svc,
		Offerings: c.catalog.offerings, Parties: c.parties.svc, Payees: c.parties.payees, Entitlements: c.entitlements.svc,
		Outbox: c.infra.outbox, Clock: c.infra.clock, Log: c.infra.log.With("module", "orders"),
		Config: orders.Config{PaymentWindow: c.infra.cfg.Orders.PaymentWindow},
		CollectionAccount: func(ctx context.Context) (orders.CollectionAccount, bool, error) {
			h := c.hooks.collectionAccount
			if h == nil {
				return orders.CollectionAccount{}, false, nil
			}
			a, found, err := h(ctx)
			return orders.CollectionAccount{ID: a.ID, IBAN: a.IBAN, HolderName: a.HolderName, BankName: a.BankName}, found, err
		},
		Sender: ordersSender{c: c}, Staff: ordersStaff{c: c}, Files: c.files.svc, Queue: c.infra.queue,
	}
	if svc := c.identity.svc; svc != nil {
		d.Users = svc
	}
	mod, err := orders.New(d)
	if err != nil {
		return err
	}
	w.mod, w.svc, w.orders, w.built, w.late = mod, mod.Service, mod.Orders, store, c
	c.catalog.buyerHeld = mod.BuyerHeld
	c.hooks.openCommerce = append(c.hooks.openCommerce, mod.OpenCommerce)
	c.privacy = append(c.privacy, mod.Privacy)
	return nil
}

// ordersSender adapts notify (L3), read at call time; a missing sender
// fails the task (retried), it never drops a notice.
type ordersSender struct{ c *container }

func (s ordersSender) Send(ctx context.Context, m notify.Message) (uuid.UUID, error) {
	if s.c.notify.svc == nil {
		return uuid.Nil(), errors.New("orders: notify is not wired")
	}
	return s.c.notify.svc.Send(ctx, m)
}

// ordersStaff lists the finance admins in effect (06 H-33) from identity's
// store through the composition root (the catalog precedent, T-277).
type ordersStaff struct{ c *container }

func (s ordersStaff) FinanceAdmins(ctx context.Context) ([]uuid.UUID, error) {
	st := s.c.identity.store
	if st == nil {
		if s.c.infra.db == nil {
			return nil, errors.New("orders: the finance admin lookup has no identity store")
		}
		st = identity.NewGormStore(s.c.infra.db.ORM)
	}
	return st.UsersWithRole(ctx, authz.PlatformFinanceAdmin)
}

// tasks: offering.cancelled → orders.cancel_offering_orders (critical);
// order.placed, .cancelled, .expired, order_line.fulfillment_marked and
// .fulfilled → the notices (01 §6.2); the tasks without a topic
// orders.notify_line_dropped (BE-134) and orders.notify_evidence_replaced
// (BE-158); the jobs orders.expire_unpaid (:50)
// and orders.close_dispute_windows (:55) (01 §7).
func (w *ordersWiring) tasks(c *container) {
	w.mod.Subscribe(c.infra.subs)
	for _, t := range w.mod.Tasks() {
		c.infra.jobs.Handle(t)
	}
	for _, j := range w.mod.Jobs() {
		c.infra.jobs.Schedule(j)
	}
}

// routes: the 13 orders operations (04 §2.15; getOrderLineEvidence BE-093, quoteOrder BE-136); w.svc is nil under `koro
// openapi`, which Register tolerates.
func (w *ordersWiring) routes(c *container, api huma.API) { orders.Register(api, w.svc) }
