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
	"koro/internal/platform/config"
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
// (BE-053) is chosen by BILLING_INVOICE_PROVIDER (invoiceProviders): none →
// Deps.Provider nil (every invoice uploaded); parasut also wires the token
// rotation (billing.refresh_token, every 30 minutes) over the shared
// envelope and the shared rate limiter. Subscribed too: invoice.requested
// (the provider flow) and invoice.failed (finance); task billing.poll_jobs.
//
//	provider nil → the adapter BILLING_INVOICE_PROVIDER names; tests preset one
type billingWiring struct {
	store    billing.Store
	tx       db.TxManager
	sealer   billing.Sealer
	provider *invoiceAdapter

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
	adapter := w.provider
	if adapter == nil {
		name := c.infra.cfg.Billing.InvoiceProvider
		if name == "" { // a container built without the configuration loader
			name = config.BillingProviderNone
		}
		a, err := invoiceProviders(c, c.infra.cfg.Billing, d).pick("BILLING_INVOICE_PROVIDER", name)
		if err != nil {
			return fmt.Errorf("billing: %w", err)
		}
		adapter = &a
	}
	if adapter.provider != nil {
		series := c.infra.cfg.Billing.Parasut.Series
		if series == "" {
			series = "KRO"
		}
		d.Provider, d.ProviderName, d.ProviderSeries, d.Tokens = adapter.provider, billing.ProviderParasut, series, adapter.tokens
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

// invoiceAdapter is an e-invoice adapter and, for Paraşüt, its token
// rotation.
type invoiceAdapter struct {
	provider billing.InvoiceProvider
	tokens   *billing.ProviderTokens
}

// invoiceProviders is the selection map of BILLING_INVOICE_PROVIDER
// (BE-053; config/provider.go): none wires no adapter (every invoice is
// uploaded), mem the in-memory FakeProvider (development; refused in
// staging and prod by the configuration), parasut Paraşüt's API v4 with
// its OAuth token rotation over integration_credentials (the shared
// envelope opens and seals the tokens; no envelope: the build stops).
// Every adapter passes billing's InvoiceProvider contract suite. The
// invoices of mem are labelled parasut (invoices_provider_chk).
func invoiceProviders(c *container, cfg config.Billing, d billing.Deps) adapters[invoiceAdapter] {
	return adapters[invoiceAdapter]{
		config.BillingProviderNone: func() (invoiceAdapter, error) { return invoiceAdapter{}, nil },
		config.BillingProviderMem: func() (invoiceAdapter, error) {
			return invoiceAdapter{provider: billing.NewFakeProvider()}, nil
		},
		config.BillingProviderParasut: func() (invoiceAdapter, error) {
			if d.Sealer == nil {
				return invoiceAdapter{}, errors.New("BILLING_INVOICE_PROVIDER=parasut needs the shared envelope (the tokens are sealed)")
			}
			p := cfg.Parasut
			log := c.infra.log.With("module", "billing", "provider", "parasut")
			oauth, err := billing.NewParasutOAuth(billing.ParasutOAuthConfig{TokenURL: p.TokenURL, ClientID: p.ClientID,
				ClientSecret: p.ClientSecret.Reveal(), Timeout: p.Timeout, Log: log})
			if err != nil {
				return invoiceAdapter{}, err
			}
			tokens, err := billing.NewProviderTokens(billing.TokenDeps{Provider: billing.ProviderParasut, Store: d.Store, Tx: d.Tx,
				Sealer: d.Sealer, Refresher: oauth, Clock: d.Clock, Log: log, Audit: d.Audit})
			if err != nil {
				return invoiceAdapter{}, err
			}
			prov, err := billing.NewParasutProvider(billing.ParasutConfig{BaseURL: p.BaseURL, Tokens: tokens,
				Limiter: c.infra.limiter, RequestsPer10s: p.RequestsPer10s, Scenario: p.Scenario, Timeout: p.Timeout, Log: log})
			if err != nil {
				return invoiceAdapter{}, err
			}
			return invoiceAdapter{provider: prov, tokens: tokens}, nil
		},
	}
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

// tasks subscribes the plan consumers, the invoice e-mail and the provider
// flow (01 §6.2), registers the billing.notify_invoice and
// billing.poll_jobs tasks and, with a provider's token rotation, schedules
// billing.refresh_token (01 §7).
func (w *billingWiring) tasks(c *container) {
	w.mod.Subscribe(c.infra.subs)
	for _, t := range w.mod.Tasks() {
		c.infra.jobs.Handle(t)
	}
	for _, j := range w.mod.Jobs() {
		c.infra.jobs.Schedule(j)
	}
}

// routes: the 9 billing operations of 04 §2.18; w.svc is nil under `koro
// openapi`, which Register tolerates.
func (w *billingWiring) routes(c *container, api huma.API) { billing.Register(api, w.svc) }
