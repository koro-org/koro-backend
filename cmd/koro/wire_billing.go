package main

import (
	"context"
	"errors"
	"fmt"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/billing"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/platform/crypto/rewrap"
	"koro/internal/platform/db"
)

// billingWiring wires the billing module (L11; 01 §4.21). First owner: BE-042.
// Only tickets of the billing module edit this file (BE-001 R4).
//
// Seams (nil in production):
//
//	store    nil → billing.NewGormStore over the app_rw pool; tests preset the in-memory store
//	tx       nil → the app_rw pool's TxManager; tests preset db.NopTx
//	sealer   nil → the shared envelope c.keys.envelope() (the integration
//	         tokens, integration_credentials.*_token_enc; declared for
//	         crypto.rewrap); without a key adminPutIntegration is 503 and
//	         nothing is stored in clear
//
// Consumed: c.orders.orders (ForBilling, PayoutLines), c.catalog.offerings
// (InvoiceTerms), c.parties.svc (Get, Platform) and c.parties.payees (Get),
// c.orgs.svc (the org of the path, the admins of the notices), c.files.svc
// (the invoice_pdf: Get, Attach, PresignGet, Read), c.audit.svc,
// c.identity.svc (re-authentication), identity's store (the finance admins
// in effect, 06 H-33), c.notify.svc (read at call time), c.infra.outbox,
// c.infra.queue and c.infra.idempotency. Subscribed: payment.captured,
// order_line.fulfilled, payout.paid, payment.refunded (the plan) and
// invoice.issued (the PDF e-mail); task billing.notify_invoice.
// Provided: the billing KVKK provider (invoices). The e-invoice provider
// (Paraşüt) is BE-053's: Deps.Provider stays nil (every invoice uploaded).
type billingWiring struct {
	store  billing.Store
	tx     db.TxManager
	sealer billing.Sealer

	mod  billing.Module
	svc  billing.Service // set by build
	late *container      // the container (matrix fixtures: the files store)
}

func (w *billingWiring) build(c *container) error {
	store, tx := w.store, w.tx
	if c.infra.db != nil {
		if store == nil {
			store = billing.NewGormStore(c.infra.db.ORM)
		}
		if tx == nil {
			tx = c.infra.db.Tx
		}
	}
	if store == nil || tx == nil {
		return errors.New("billing: no store (database not opened)")
	}
	switch {
	case c.orders.orders == nil:
		return errors.New("billing: orders is not built")
	case c.catalog.offerings == nil:
		return errors.New("billing: catalog is not built")
	case c.parties.svc == nil || c.parties.payees == nil:
		return errors.New("billing: parties is not built")
	case c.orgs.svc == nil:
		return errors.New("billing: orgs is not built")
	case c.files.svc == nil:
		return errors.New("billing: files is not built")
	}
	d := billing.Deps{
		Store: store, Tx: tx, Clock: c.infra.clock, Log: c.infra.log.With("module", "billing"), Audit: c.audit.svc,
		Outbox: c.infra.outbox, Queue: c.infra.queue, Idempotency: c.infra.idempotency, Orders: c.orders.orders,
		Offerings: c.catalog.offerings, Parties: c.parties.svc, Payees: c.parties.payees, Orgs: c.orgs.svc,
		Files: c.files.svc, Sealer: w.sealer, Sender: billingSender{c: c}, Staff: billingStaff{c: c},
	}
	if svc := c.identity.svc; svc != nil {
		d.Reauth = svc
	}
	if d.Sealer == nil {
		if env, err := c.keys.envelope(); err == nil {
			d.Sealer = env
		}
	}
	for _, col := range []rewrap.Column{
		{Name: "billing.integration_credentials.access_token_enc", Table: "integration_credentials", Key: "provider",
			KeyType: "text", Column: "access_token_enc", PurposePrefix: billing.AccessTokenPurposePrefix, PurposeColumn: "provider"},
		{Name: "billing.integration_credentials.refresh_token_enc", Table: "integration_credentials", Key: "provider",
			KeyType: "text", Column: "refresh_token_enc", PurposePrefix: billing.RefreshTokenPurposePrefix, PurposeColumn: "provider"},
	} {
		if err := c.keys.addRewrap(col); err != nil {
			return fmt.Errorf("billing: %w", err)
		}
	}
	mod, err := billing.New(d)
	if err != nil {
		return err
	}
	w.mod, w.svc, w.late = mod, mod.Service, c
	c.privacy = append(c.privacy, mod.Privacy)
	return nil
}

// billingSender adapts notify (L3), read at call time; a missing sender
// fails the task (retried), it never drops a notice.
type billingSender struct{ c *container }

func (s billingSender) Send(ctx context.Context, m notify.Message) (uuid.UUID, error) {
	if s.c.notify.svc == nil {
		return uuid.Nil(), errors.New("billing: notify is not wired")
	}
	return s.c.notify.svc.Send(ctx, m)
}

// billingStaff lists the finance admins in effect (06 H-33) from identity's
// store through the composition root (the ledger precedent).
type billingStaff struct{ c *container }

func (s billingStaff) FinanceAdmins(ctx context.Context) ([]uuid.UUID, error) {
	st := s.c.identity.store
	if st == nil {
		if s.c.infra.db == nil {
			return nil, errors.New("billing: the finance admin lookup has no identity store")
		}
		st = identity.NewGormStore(s.c.infra.db.ORM)
	}
	return st.UsersWithRole(ctx, authz.PlatformFinanceAdmin)
}

// tasks subscribes the plan consumers and the invoice e-mail (01 §6.2) and
// registers the billing.notify_invoice task.
func (w *billingWiring) tasks(c *container) {
	w.mod.Subscribe(c.infra.subs)
	for _, t := range w.mod.Tasks() {
		c.infra.jobs.Handle(t)
	}
}

// routes: the 9 billing operations of 04 §2.18; w.svc is nil under `koro
// openapi`, which Register tolerates.
func (w *billingWiring) routes(c *container, api huma.API) { billing.Register(api, w.svc) }
