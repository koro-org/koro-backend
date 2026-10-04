package main

import (
	"errors"
	"fmt"

	"github.com/danielgtaylor/huma/v2"

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
//
// Consumed: c.audit.svc, c.files.svc (party_document files: Get, Attach,
// PresignGet) and c.orgs.svc (orgs.OrgDirectory: the org of the path).
// Provided: svc (parties.Parties for orders and billing: Get, Platform,
// BuyerPartyOf) and the parties KVKK provider. 06 H-54: natural-person
// payee parties stay closed (Deps.AllowNaturalPersonPayees is never set).
type partiesWiring struct {
	store   parties.Store
	tx      db.TxManager
	sealer  parties.Sealer
	indexer parties.Indexer

	mod parties.Module
	svc parties.Service // set by build
	// late is the container, kept for the matrix fixtures (files store).
	late *container
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
	d := parties.Deps{
		Store: store, Tx: tx, Audit: c.audit.svc, Idempotency: c.infra.idempotency, Orgs: c.orgs.svc,
		Clock: c.infra.clock, Log: c.infra.log.With("module", "parties"), Sealer: w.sealer, Indexer: w.indexer,
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
	w.mod, w.svc, w.late = mod, mod.Service, c
	c.privacy = append(c.privacy, mod.Privacy)
	return nil
}

// tasks: none (BE-036 has no job or subscription; file.rejected is not
// consumed: documents are financial records and keep their row, a rejected
// file answers 409 on download and verification).
func (w *partiesWiring) tasks(c *container) {}

// routes: the 11 legal party and document operations of BE-036 (04 §2.13);
// w.svc is nil under `koro openapi`, which Register tolerates.
func (w *partiesWiring) routes(c *container, api huma.API) { parties.Register(api, w.svc) }
