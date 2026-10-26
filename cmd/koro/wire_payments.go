package main

import (
	"context"
	"errors"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/payments"
	"koro/internal/platform/db"
)

// paymentsWiring wires the payments module (L12; 01 §4.20). First owner: BE-041.
// Only tickets of the payments module edit this file (BE-001 R4).
//
// Seams (nil in production):
//
//	store     nil → payments.NewGormStore over the app_rw pool
//	newStore  tests: builds the in-memory store over the container clock
//	tx        nil → the app_rw pool's TxManager (the one orders, ledger and
//	          the idempotency keeper join: the capture is one transaction)
//
// Consumed: c.orders.orders (ForPayment, FindByReference, ApplyPayment,
// ConfirmDirectPayment, GetLine, ApplyRefund, RefundsDue; 01 §6.3 same
// transaction), c.ledger.svc (Poster: payment_received, order_allocated,
// refund_completed), c.ledger.mod (BE-043: the live payouts of a line and
// the shares refunds took back from it, 06 H-59), c.audit.svc,
// c.parties.svc (Koro's own party), c.files.svc (receipts and the direct
// payment evidence), c.identity.svc (re-authentication, notice
// addresses), identity's store (the platform and finance admins in effect,
// 06 H-33), c.orgs.svc (the admins of buyer and seller orgs; the org and the
// names of a refund request, BE-132), c.infra.cfg.Payments (the refund
// request window), c.notify.svc
// (read at call time), c.infra.outbox, c.infra.queue and
// c.infra.idempotency.
//
// Provided, set in build: svc (payments.Service), payments (payments.Payments
// for billing and BE-043), hooks.collectionAccount (orders' payment
// instructions: Koro's active default account) and the payments KVKK
// provider.
type paymentsWiring struct {
	store    payments.Store
	newStore func(c *container) payments.Store
	tx       db.TxManager

	mod      payments.Module
	svc      payments.Service  // set by build
	payments payments.Payments // set by build
	built    payments.Store    // the store build used (matrix fixtures seed it)
	late     *container        // the container (matrix fixtures: the files and orders stores)
}

func (w *paymentsWiring) build(c *container) error {
	store, tx := w.store, w.tx
	if w.newStore != nil {
		store = w.newStore(c)
	}
	if c.infra.db != nil {
		if store == nil {
			store = payments.NewGormStore(c.infra.db.ORM)
		}
		if tx == nil {
			tx = c.infra.db.Tx
		}
	}
	if store == nil || tx == nil {
		return errors.New("payments: no store (database not opened)")
	}
	switch {
	case c.orders.orders == nil:
		return errors.New("payments: orders is not built")
	case c.ledger.svc == nil:
		return errors.New("payments: ledger is not built")
	}
	ledgerLines := c.ledger.mod
	d := payments.Deps{
		Store: store, Tx: tx, Audit: c.audit.svc, Idempotency: c.infra.idempotency, Orders: c.orders.orders, Ledger: c.ledger.svc,
		Outbox: c.infra.outbox, Queue: c.infra.queue, Clock: c.infra.clock, Log: c.infra.log.With("module", "payments"),
		Config: payments.Config{RefundRequestWindow: c.infra.cfg.Payments.RefundRequestWindow},
		Sender: paymentsSender{c: c}, Staff: paymentsStaff{c: c}, LedgerLines: ledgerLines,
	}
	if c.parties.svc != nil {
		d.Parties = c.parties.svc
	}
	if c.files.svc != nil {
		d.Files = c.files.svc
	}
	if svc := c.identity.svc; svc != nil {
		d.Reauth, d.Users = svc, svc
	}
	if c.orgs.svc != nil {
		d.Orgs = c.orgs.svc
	}
	mod, err := payments.New(d)
	if err != nil {
		return err
	}
	w.mod, w.svc, w.payments, w.built, w.late = mod, mod.Service, mod.Payments, store, c
	c.hooks.collectionAccount = func(ctx context.Context) (hookCollectionAccount, bool, error) {
		a, found, err := mod.DefaultAccount(ctx)
		if err != nil || !found {
			return hookCollectionAccount{}, false, err
		}
		return hookCollectionAccount{ID: a.ID, IBAN: a.IBAN, HolderName: a.HolderName, BankName: a.BankName}, true, nil
	}
	c.privacy = append(c.privacy, mod.Privacy)
	return nil
}

// paymentsSender adapts notify (L3), read at call time; a missing sender
// fails the task (retried), it never drops a notice.
type paymentsSender struct{ c *container }

func (s paymentsSender) Send(ctx context.Context, m notify.Message) (uuid.UUID, error) {
	if s.c.notify.svc == nil {
		return uuid.Nil(), errors.New("payments: notify is not wired")
	}
	return s.c.notify.svc.Send(ctx, m)
}

// paymentsStaff lists the platform and finance admins in effect (06 H-33)
// from identity's store through the composition root (T-277).
type paymentsStaff struct{ c *container }

func (s paymentsStaff) holders(ctx context.Context, r authz.PlatformRole) ([]uuid.UUID, error) {
	st := s.c.identity.store
	if st == nil {
		if s.c.infra.db == nil {
			return nil, errors.New("payments: the admin lookup has no identity store")
		}
		st = identity.NewGormStore(s.c.infra.db.ORM)
	}
	return st.UsersWithRole(ctx, r)
}

func (s paymentsStaff) FinanceAdmins(ctx context.Context) ([]uuid.UUID, error) {
	return s.holders(ctx, authz.PlatformFinanceAdmin)
}

func (s paymentsStaff) PlatformAdmins(ctx context.Context) ([]uuid.UUID, error) {
	return s.holders(ctx, authz.PlatformAdmin)
}

// tasks: collection_account.approved → payments.notify_collection_account_approved
// (critical), payment.captured → payments.notify_payment_captured,
// payment.refunded → payments.notify_payment_refunded (BE-043),
// incoming_transfer.unmatched → payments.notify_unmatched_transfers (01
// §6.2); the plain task payments.notify_collection_account_changed (A); the
// job payments.unmatched_digest (01 §7: working days 09:30).
func (w *paymentsWiring) tasks(c *container) {
	w.mod.Subscribe(c.infra.subs)
	for _, t := range w.mod.Tasks() {
		c.infra.jobs.Handle(t)
	}
	for _, j := range w.mod.Jobs() {
		c.infra.jobs.Schedule(j)
	}
}

// routes: the 15 payments operations of BE-041 (04 §2.16's 12, the return's
// request and second eye (06 H-56, T-79, T-228) and the model C direct
// confirmation), the 8 of BE-043 (04 §2.16's 7 payments and refunds and
// the refunds due) and the 12 refund request operations of BE-132; w.svc is
// nil under `koro openapi`, which Register tolerates.
func (w *paymentsWiring) routes(c *container, api huma.API) { payments.Register(api, w.svc) }
