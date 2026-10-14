package main

import (
	"context"
	"errors"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/ledger"
	"koro/internal/notify"
	"koro/internal/platform/db"
)

// ledgerWiring wires the ledger module (L11; 01 §4.19). First owner: BE-039.
// Only tickets of the ledger module edit this file (BE-001 R4).
//
// Seams (nil in production):
//
//	store    nil → ledger.NewGormStore over the app_rw pool; tests preset the in-memory store
//	payouts  nil → the store when it is also a PayoutStore (the in-memory one),
//	         else ledger.NewGormPayoutStore over the app_rw pool
//	tx       nil → the app_rw pool's TxManager; tests preset db.NopTx
//
// Consumers (01 §6.3, same transaction): payments (BE-041/BE-043) adapt svc
// to their Poster port ("// sync-write: ledger.Poster.Post"); the payout
// flow (BE-044) is ledger's own. payments is a higher layer and reads
// c.ledger.svc in its build.
//
// Payout flow (BE-044), consumed: c.orders.orders (PayableLines,
// MarkLinesPaidOut in the mark-paid transaction, GetLine, ForPayment),
// c.parties.payees (Get, ApprovedAccount: the IBAN frozen at creation,
// IsActive), c.parties.svc (the payee party's contact address),
// c.orgs.svc (the org of the path, the seller org's admins), c.files.svc
// (the payout_receipt: Get, Attach, PresignGet), c.audit.svc,
// c.identity.svc (re-authentication of the approval), identity's store
// (the finance admins in effect, 06 H-33), c.notify.svc (read at call
// time), c.infra.outbox and c.infra.idempotency.
// Provided: the closeOrg check "open payouts" appended to
// hooks.openCommerce (after orders' "open orders"; never replacing it)
// and the ledger KVKK provider (payouts).
//
// Alarm: nil → ERROR log with alarm=true for each integrity finding until
// GlitchTip/finance paging is wired (BE-058).
//
// build adds the financial_records retention sweeper (06 H-57; BE-057) to
// c.sweepers.
type ledgerWiring struct {
	store   ledger.Store
	payouts ledger.PayoutStore
	tx      db.TxManager

	mod  ledger.Module
	svc  ledger.Service // set by build
	late *container     // the container (matrix fixtures: the orders and files stores)
}

func (w *ledgerWiring) build(c *container) error {
	store, payouts, tx := w.store, w.payouts, w.tx
	if payouts == nil {
		if ps, ok := store.(ledger.PayoutStore); ok {
			payouts = ps
		}
	}
	if c.infra.db != nil {
		if store == nil {
			store = ledger.NewGormStore(c.infra.db.ORM)
		}
		if payouts == nil {
			payouts = ledger.NewGormPayoutStore(c.infra.db.ORM)
		}
		if tx == nil {
			tx = c.infra.db.Tx
		}
	}
	if store == nil || payouts == nil || tx == nil {
		return errors.New("ledger: no store (database not opened)")
	}
	switch {
	case c.orders.orders == nil:
		return errors.New("ledger: orders is not built")
	case c.parties.svc == nil || c.parties.payees == nil:
		return errors.New("ledger: parties is not built")
	case c.orgs.svc == nil:
		return errors.New("ledger: orgs is not built")
	case c.files.svc == nil:
		return errors.New("ledger: files is not built")
	}
	d := ledger.Deps{
		Store: store, Tx: tx, Clock: c.infra.clock, Log: c.infra.log.With("module", "ledger"),
		Payouts: payouts, Orders: c.orders.orders, Payees: c.parties.payees, Parties: c.parties.svc, Orgs: c.orgs.svc,
		Files: c.files.svc, Audit: c.audit.svc, Outbox: c.infra.outbox, Idempotency: c.infra.idempotency,
		Sender: ledgerSender{c: c}, Staff: ledgerStaff{c: c},
	}
	if svc := c.identity.svc; svc != nil {
		d.Reauth = svc
	}
	mod, err := ledger.New(d)
	if err != nil {
		return err
	}
	w.mod, w.svc, w.late = mod, mod.Service, c
	c.sweepers = append(c.sweepers, mod.Sweeper)
	c.privacy = append(c.privacy, mod.Privacy)
	c.hooks.openCommerce = append(c.hooks.openCommerce, mod.OpenCommerce)
	return nil
}

// ledgerSender adapts notify (L3), read at call time; a missing sender
// fails the task (retried), it never drops a notice.
type ledgerSender struct{ c *container }

func (s ledgerSender) Send(ctx context.Context, m notify.Message) (uuid.UUID, error) {
	if s.c.notify.svc == nil {
		return uuid.Nil(), errors.New("ledger: notify is not wired")
	}
	return s.c.notify.svc.Send(ctx, m)
}

// ledgerStaff lists the finance admins in effect (06 H-33) from identity's
// store through the composition root (the orders precedent).
type ledgerStaff struct{ c *container }

func (s ledgerStaff) FinanceAdmins(ctx context.Context) ([]uuid.UUID, error) {
	st := s.c.identity.store
	if st == nil {
		if s.c.infra.db == nil {
			return nil, errors.New("ledger: the finance admin lookup has no identity store")
		}
		st = identity.NewGormStore(s.c.infra.db.ORM)
	}
	return st.UsersWithRole(ctx, authz.PlatformFinanceAdmin)
}

// tasks schedules ledger.integrity_check (01 §7: daily at 05:15, low), the
// late recognition of Koro's own sales before it (05:05, low; BE-067) and
// subscribes the payout notices (payout.created, .approved, .paid).
func (w *ledgerWiring) tasks(c *container) {
	for _, j := range w.mod.Jobs() {
		c.infra.jobs.Schedule(j)
	}
	w.mod.Subscribe(c.infra.subs)
}

// routes: the 12 ledger operations of 04 §2.17 (the journal, BE-039; the
// payee balance and the payout flow, BE-044).
func (w *ledgerWiring) routes(c *container, api huma.API) { ledger.Register(api, w.svc) }
