package main

import (
	"errors"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/ledger"
	"koro/internal/platform/db"
)

// ledgerWiring wires the ledger module (L11; 01 §4.19). First owner: BE-039.
// Only tickets of the ledger module edit this file (BE-001 R4).
// Appends to hooks.openCommerce (BE-044).
//
// Seams (nil in production):
//
//	store  nil → ledger.NewGormStore over the app_rw pool; tests preset the in-memory store
//	tx     nil → the app_rw pool's TxManager; tests preset db.NopTx
//
// Consumers (01 §6.3, same transaction): payments (BE-041/BE-043) adapt svc
// to their Poster port ("// sync-write: ledger.Poster.Post"); the payout
// flow (BE-044) is ledger's own. payments is a higher layer and reads
// c.ledger.svc in its build.
//
// Alarm: nil → ERROR log with alarm=true for each integrity finding until
// GlitchTip/finance paging is wired (BE-058).
type ledgerWiring struct {
	store ledger.Store
	tx    db.TxManager

	mod ledger.Module
	svc ledger.Service // set by build
}

func (w *ledgerWiring) build(c *container) error {
	store, tx := w.store, w.tx
	if c.infra.db != nil {
		if store == nil {
			store = ledger.NewGormStore(c.infra.db.ORM)
		}
		if tx == nil {
			tx = c.infra.db.Tx
		}
	}
	if store == nil || tx == nil {
		return errors.New("ledger: no store (database not opened)")
	}
	mod, err := ledger.New(ledger.Deps{
		Store: store, Tx: tx, Clock: c.infra.clock, Log: c.infra.log.With("module", "ledger"),
	})
	if err != nil {
		return err
	}
	w.mod, w.svc = mod, mod.Service
	return nil
}

// tasks schedules ledger.integrity_check (01 §7: daily at 05:15, low).
func (w *ledgerWiring) tasks(c *container) {
	for _, j := range w.mod.Jobs() {
		c.infra.jobs.Schedule(j)
	}
}

// routes: adminListLedgerTransactions (04 §2.17; the payee balance and
// payout endpoints come with BE-044).
func (w *ledgerWiring) routes(c *container, api huma.API) { ledger.Register(api, w.svc) }
