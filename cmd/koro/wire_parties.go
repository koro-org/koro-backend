package main

import (
	"context"
	"errors"
	"fmt"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/parties"
	"koro/internal/platform/crypto"
	"koro/internal/platform/crypto/rewrap"
	"koro/internal/platform/db"
)

// partiesWiring wires the parties module (L7; 01 §4.12). First owner: BE-036
// (legal parties, documents); BE-037 adds payees and bank accounts.
// Only tickets of the parties module edit this file (BE-001 R4).
//
// Seams (nil in production):
//
//	store    nil → parties.NewGormStore over the app_rw pool; tests preset the in-memory store
//	tx       nil → the app_rw pool's TxManager; tests preset db.NopTx
//	sealer   nil → the shared envelope c.keys.envelope() (an individual's
//	         TCKN, legal_parties.tckn_enc); without a key the TCKN path
//	         fails closed (503) and nothing is stored in clear
//	indexer  nil → crypto.Indexer over CRYPTO_BLIND_INDEX_KEY
//	         (legal_parties.tckn_bidx); without a key the TCKN path is 503
//	roles    nil → identity's store (c.identity.store when the tests preset
//	         it, identity.NewGormStore otherwise): the finance admins in
//	         effect (06 H-33) for the bank account notices
//
// Consumed: c.audit.svc, c.files.svc (party_document and
// bank_account_evidence files: Get, Attach, PresignGet), c.orgs.svc
// (orgs.OrgDirectory: the org of the path, the payee org's admins),
// c.identity.svc (re-authentication of submit/approve/revoke, the notice
// addresses), c.notify.svc (the notices, worker), c.infra.outbox and
// c.infra.queue.
// Provided: svc (parties.Parties for orders and billing: Get, Platform,
// BuyerPartyOf), payees (parties.Payees for catalog, orders and ledger:
// Get, DefaultFor, ApprovedAccount, IsActive) and the parties KVKK
// provider. 06 H-54: natural-person payees stay closed
// (Deps.AllowNaturalPersonPayees is never set).
type partiesWiring struct {
	store   parties.Store
	tx      db.TxManager
	sealer  parties.Sealer
	indexer parties.Indexer
	roles   roleHolders

	mod    parties.Module
	svc    parties.Service // set by build
	payees parties.Payees  // set by build
	// late is the container, kept for the matrix fixtures (files store).
	late *container
}

// roleHolders is the part of identity's store the bank account notices
// read: who holds a platform role in effect.
type roleHolders interface {
	UsersWithRole(ctx context.Context, role authz.PlatformRole) ([]uuid.UUID, error)
}

func (w *partiesWiring) build(c *container) error {
	store, tx := w.store, w.tx
	if c.infra.db != nil {
		if store == nil {
			store = parties.NewGormStore(c.infra.db.ORM)
		}
		if tx == nil {
			tx = c.infra.db.Tx
		}
	}
	if store == nil || tx == nil {
		return errors.New("parties: no store (database not opened)")
	}
	if c.orgs.svc == nil {
		return errors.New("parties: orgs is not built")
	}
	roles := w.roles
	switch {
	case roles != nil:
	case c.identity.store != nil:
		roles = c.identity.store
	case c.infra.db != nil:
		roles = identity.NewGormStore(c.infra.db.ORM)
	}
	d := parties.Deps{
		Store: store, Tx: tx, Audit: c.audit.svc, Idempotency: c.infra.idempotency, Orgs: c.orgs.svc,
		Clock: c.infra.clock, Log: c.infra.log.With("module", "parties"), Sealer: w.sealer, Indexer: w.indexer,
		Outbox: c.infra.outbox, Queue: c.infra.queue, Sender: partiesSender{c: c},
	}
	if roles != nil {
		d.FinanceAdmins = func(ctx context.Context) ([]uuid.UUID, error) {
			return roles.UsersWithRole(ctx, authz.PlatformFinanceAdmin)
		}
	}
	if svc := c.identity.svc; svc != nil {
		d.Reauth, d.Users = svc, svc
	}
	if c.files.svc != nil {
		d.Files = c.files.svc
	}
	if d.Sealer == nil {
		if env, err := c.keys.envelope(); err == nil {
			d.Sealer = env
		}
	}
	if d.Indexer == nil {
		if key := c.infra.platform.Crypto.BlindIndexKey; !key.IsZero() {
			k, err := crypto.ParseKey(key.Reveal())
			if err != nil {
				return fmt.Errorf("parties: blind index key: %w", err)
			}
			x, err := crypto.NewIndexer(k)
			if err != nil {
				return fmt.Errorf("parties: blind index key: %w", err)
			}
			d.Indexer = x
		}
	}
	if err := c.keys.addRewrap(rewrap.Column{Name: "parties.legal_parties.tckn_enc", Table: "legal_parties", Key: "id",
		Column: "tckn_enc", PurposePrefix: parties.TCKNSealPurposePrefix, PurposeColumn: "id"}); err != nil {
		return fmt.Errorf("parties: %w", err)
	}
	mod, err := parties.New(d)
	if err != nil {
		return err
	}
	w.mod, w.svc, w.payees, w.late = mod, mod.Service, mod.Payees, c
	c.privacy = append(c.privacy, mod.Privacy)
	return nil
}

// partiesSender adapts notify's service, read at call time (the worker
// sends the bank account notices).
type partiesSender struct{ c *container }

func (s partiesSender) Send(ctx context.Context, m notify.Message) (uuid.UUID, error) {
	if s.c.notify.svc == nil {
		return uuid.Nil(), errors.New("parties: notify is not wired")
	}
	return s.c.notify.svc.Send(ctx, m)
}

// tasks: payee_bank_account.submitted → parties.notify_bank_account_submitted,
// .approved → parties.notify_bank_account_approved, .rejected →
// parties.notify_bank_account_rejected (01 §6.2) and the
// parties.notify_bank_account_revoked task (A). file.rejected is not
// consumed: documents and evidence are financial records and keep their
// row; a rejected file answers 409 on download and verification.
func (w *partiesWiring) tasks(c *container) {
	w.mod.Subscribe(c.infra.subs)
	for _, t := range w.mod.Tasks() {
		c.infra.jobs.Handle(t)
	}
}

// routes: the 29 parties operations (04 §2.13: BE-036's 11, BE-037's 13,
// the evidence download, and 06 H-60 / T-234's 4 admin party operations);
// w.svc is nil under `koro openapi`, which Register tolerates.
func (w *partiesWiring) routes(c *container, api huma.API) { parties.Register(api, w.svc) }
