package main

import (
	"context"
	"errors"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/messaging"
	"koro/internal/notify"
	"koro/internal/platform/db"
)

// messagingWiring wires the messaging module (L10; 01 §4.15). First owner: BE-056.
// Only tickets of the messaging module edit this file (BE-001 R4).
//
// Seams (nil in production):
//
//	store  nil → messaging.NewGormStore over the app_rw pool; tests preset the in-memory store
//	tx     nil → the app_rw pool's TxManager; tests preset db.NopTx
//
// Consumed (built in lower layers): hooks.anonResolver (discovery, BE-051:
// the company's pseudonym → a discoverable student; read at call time, nil
// → every pseudonym is 404), c.discovery.svc (AnonID: the company sees a
// request under its pseudonym), c.students.svc (Discoverable,
// AccessGrants.Grant in the acceptance's transaction), c.entitlements.svc
// (the contact_request unit, same transaction), c.orgs.svc (the company's
// status and name, notice recipients), c.feed.svc (an opportunity call),
// c.audit.svc, c.infra.outbox, c.infra.idempotency and c.notify.svc (the
// in-app notices, read at call time).
//
// Provided, set in build: svc (messaging.Service), the KVKK provider
// (contact_requests, contact_blocks) and the retention sweeper of
// contact_request_closed. tasks: messaging.expire_requests (01 §7, :40
// hourly) and the two notice subscriptions (contact_request.created,
// contact_request.closed).
type messagingWiring struct {
	store messaging.Store
	tx    db.TxManager

	mod  messaging.Module
	svc  messaging.Service // set by build
	late *container        // the container, for the matrix fixtures
}

func (w *messagingWiring) build(c *container) error {
	store, tx := w.store, w.tx
	if c.infra.db != nil {
		if store == nil {
			store = messaging.NewGormStore(c.infra.db.ORM)
		}
		if tx == nil {
			tx = c.infra.db.Tx
		}
	}
	if store == nil || tx == nil {
		return errors.New("messaging: no store (database not opened)")
	}
	if c.students.svc == nil || c.entitlements.svc == nil || c.orgs.svc == nil || c.audit.svc == nil ||
		c.discovery.svc == nil {
		return errors.New("messaging: students, entitlements, orgs, audit or discovery is not built")
	}
	d := messaging.Deps{
		Store: store, Tx: tx, Audit: c.audit.svc, Idempotency: c.infra.idempotency, Clock: c.infra.clock,
		Log: c.infra.log.With("module", "messaging"), Entitlements: c.entitlements.svc, Grants: c.students.svc,
		Students: c.students.svc, Orgs: c.orgs.svc, Pseudonyms: c.discovery.svc,
		Resolve: func(ctx context.Context, companyOrgID uuid.UUID, anonID string) (uuid.UUID, bool, error) {
			if h := c.hooks.anonResolver; h != nil {
				return h(ctx, companyOrgID, anonID)
			}
			return uuid.Nil(), false, nil
		},
		Sender: messagingSender{c: c},
	}
	if c.infra.outbox != nil {
		d.Outbox = c.infra.outbox
	}
	if svc := c.feed.svc; svc != nil {
		d.Posts = svc
	}
	mod, err := messaging.New(d)
	if err != nil {
		return err
	}
	w.mod, w.svc, w.late = mod, mod.Service, c
	c.privacy = append(c.privacy, mod.Privacy)
	c.sweepers = append(c.sweepers, mod.Sweeper)
	return nil
}

// messagingSender adapts notify (L3) to messaging.Sender, read at call
// time; a missing sender fails the task (retried), it never drops a notice.
type messagingSender struct{ c *container }

func (s messagingSender) Send(ctx context.Context, m notify.Message) (uuid.UUID, error) {
	if s.c.notify.svc == nil {
		return uuid.Nil(), errors.New("messaging: notify is not wired")
	}
	return s.c.notify.svc.Send(ctx, m)
}

// tasks schedules messaging.expire_requests (01 §7: every hour at :40,
// default) and subscribes messaging.notify_request_received
// (contact_request.created) and messaging.notify_request_closed
// (contact_request.closed).
func (w *messagingWiring) tasks(c *container) {
	w.mod.Subscribe(c.infra.subs)
	for _, j := range w.mod.Jobs() {
		c.infra.jobs.Schedule(j)
	}
}

// routes: the 10 operations of 04 §2.9; w.svc is nil under `koro openapi`,
// which Register tolerates.
func (w *messagingWiring) routes(c *container, api huma.API) { messaging.Register(api, w.svc) }
