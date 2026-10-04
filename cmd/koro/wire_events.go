package main

import (
	"context"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/events"
	"koro/internal/notify"
	"koro/internal/platform/crypto"
	"koro/internal/platform/crypto/rewrap"
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
//	sealer    nil → the shared envelope c.keys.envelope() (the sensitive
//	          answers of registrations; none configured → such answers are
//	          refused, 503)
//	tickets   nil → a crypto.Deriver over CRYPTO_TOKEN_KEYS (ticket tokens)
//
// Consumed: c.audit.svc, c.orgs.svc (the organiser's kind, status and
// name; the managers notified of an application), c.directory.svc
// (sensitive clubs, a club's university, the orgs of a university,
// universities and departments), c.identity.svc (staff accounts and
// names, registrants' names; nil → staff cannot be added), c.files.svc
// (the cover; nil → no cover), c.consent.svc (BE-026: the current consent
// texts of a form's consent fields; BE-028: consent.Recorder of the
// consent fields answered yes, 01 §6.3; nil → such a form cannot be
// published, such a registration is refused), c.students.svc (BE-028:
// the registrant card of the organiser's views), c.notify.svc (BE-028:
// the in-app notices of the registration events, read at call time) and
// c.hooks.retention (BE-028: the answer purge periods of
// kvkk.retention_policies, read at call time).
//
// Provided, set in build: svc (events.Events for feed and catalog;
// events.Registrations, T-241: the students hook once BE-025 is merged),
// the events KVKK provider (staff assignments, registrations, tickets) and
// the crypto.rewrap declaration of event_registrations.answers_sensitive_enc.
// Worker (tasks): event.cancelled → events.cancel_registrations, the
// registration.* notices, events.waitlist_expire and
// events.reconcile_counts (01 §6.2, §7).
type eventsWiring struct {
	store    events.Store
	newStore func(c *container) events.Store
	tx       db.TxManager
	sealer   events.Sealer
	tickets  events.TicketKeys

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
		Sealer: w.sealer, Tickets: w.tickets, Sender: eventsSender{c: c},
		Retention: func(ctx context.Context, code string) (time.Duration, bool, error) {
			if h := c.hooks.retention; h != nil {
				return h(ctx, code)
			}
			return 0, false, nil
		},
	}
	if svc := c.identity.svc; svc != nil {
		d.Users = svc
	}
	if svc := c.files.svc; svc != nil {
		d.Files = svc
	}
	if svc := c.consent.svc; svc != nil {
		d.Notices, d.Consents = svc, svc
	}
	if svc := c.students.svc; svc != nil {
		d.Students = svc
	}
	if d.Sealer == nil {
		if env, err := c.keys.envelope(); err == nil {
			d.Sealer = env
		}
	}
	if d.Tickets == nil {
		keys, err := crypto.ParseKeyring(c.infra.platform.Crypto.TokenKeys.Reveal())
		if err != nil {
			return fmt.Errorf("events: CRYPTO_TOKEN_KEYS: %w", err)
		}
		deriver, err := crypto.NewDeriver(keys)
		if err != nil {
			return fmt.Errorf("events: CRYPTO_TOKEN_KEYS: %w", err)
		}
		d.Tickets = deriver
	}
	if err := c.keys.addRewrap(rewrap.Column{Name: "events.event_registrations.answers_sensitive_enc",
		Table: "event_registrations", Key: "id", Column: "answers_sensitive_enc",
		PurposePrefix: events.SensitiveAnswersPurposePrefix, PurposeColumn: "id"}); err != nil {
		return fmt.Errorf("events: %w", err)
	}
	mod, err := events.New(d)
	if err != nil {
		return err
	}
	w.mod, w.svc, w.built = mod, mod.Service, store
	c.privacy = append(c.privacy, mod.Privacy)
	return nil
}

// tasks: event.cancelled → events.cancel_registrations, the
// registration.submitted/waitlisted/rejected/cancelled notices (BE-028;
// registration.confirmed's ticket e-mail and event.schedule_changed are
// BE-031's, event.cancelled's offerings catalog's), and the jobs
// events.waitlist_expire and events.reconcile_counts.
func (w *eventsWiring) tasks(c *container) {
	w.mod.Subscribe(c.infra.subs)
	for _, j := range w.mod.Jobs() {
		c.infra.jobs.Schedule(j)
	}
}

// routes: the 12 operations of BE-024, the 7 of BE-026 and the 8 of
// BE-028 (04 §2.6); w.svc is nil under `koro openapi`, which Register
// tolerates.
func (w *eventsWiring) routes(c *container, api huma.API) { events.Register(api, w.svc) }

// eventsSender adapts notify (L3) to events.Sender, read at call time; a
// missing sender fails the task (retried), it never drops the notice.
type eventsSender struct{ c *container }

func (n eventsSender) Send(ctx context.Context, m notify.Message) (uuid.UUID, error) {
	if n.c.notify.svc == nil {
		return uuid.Nil(), errors.New("events: notify is not wired")
	}
	return n.c.notify.svc.Send(ctx, m)
}
