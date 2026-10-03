package main

import (
	"errors"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/events"
	"koro/internal/platform/db"
)

// eventsWiring wires the events module (L8; 01 §4.10). First owner: BE-024.
// Only tickets of the events module edit this file (BE-001 R4).
// WebSocket hub shutdown goes to c.shutdownHooks (BE-054).
//
// Seams (nil in production):
//
//	store     nil → events.NewGormStore over the app_rw pool
//	newStore  tests: builds the in-memory store over the container clock
//	tx        nil → the app_rw pool's TxManager; tests preset db.NopTx
//
// Consumed: c.audit.svc, c.orgs.svc (the organiser's kind, status and
// name), c.directory.svc (sensitive clubs, a club's university, the orgs
// of a university), c.identity.svc (staff accounts and names; nil → staff
// cannot be added), c.files.svc (the cover; nil → no cover).
//
// Provided, set in build: svc (events.Events for feed and catalog) and the
// events KVKK provider (staff assignments).
type eventsWiring struct {
	store    events.Store
	newStore func(c *container) events.Store
	tx       db.TxManager

	mod   events.Module
	svc   events.Service // set by build
	built events.Store   // the store build used (wiring and matrix tests seed it)
}

func (w *eventsWiring) build(c *container) error {
	store, tx := w.store, w.tx
	if w.newStore != nil {
		store = w.newStore(c)
	}
	if c.infra.db != nil {
		if store == nil {
			store = events.NewGormStore(c.infra.db.ORM)
		}
		if tx == nil {
			tx = c.infra.db.Tx
		}
	}
	if store == nil || tx == nil {
		return errors.New("events: no store (database not opened)")
	}
	if c.orgs.svc == nil || c.directory.svc == nil {
		return errors.New("events: orgs or directory is not built")
	}
	d := events.Deps{
		Store: store, Tx: tx, Outbox: c.infra.outbox, Audit: c.audit.svc, Orgs: c.orgs.svc, Catalog: c.directory.svc,
		Idempotency: c.infra.idempotency, Clock: c.infra.clock, Log: c.infra.log.With("module", "events"),
	}
	if svc := c.identity.svc; svc != nil {
		d.Users = svc
	}
	if svc := c.files.svc; svc != nil {
		d.Files = svc
	}
	mod, err := events.New(d)
	if err != nil {
		return err
	}
	w.mod, w.svc, w.built = mod, mod.Service, store
	c.privacy = append(c.privacy, mod.Privacy)
	return nil
}

// tasks: the event core registers nothing; event.cancelled and
// event.schedule_changed are consumed by BE-028/BE-031 and catalog.
func (w *eventsWiring) tasks(c *container) {}

// routes: the 12 operations of BE-024 (04 §2.6); w.svc is nil under
// `koro openapi`, which Register tolerates.
func (w *eventsWiring) routes(c *container, api huma.API) { events.Register(api, w.svc) }
