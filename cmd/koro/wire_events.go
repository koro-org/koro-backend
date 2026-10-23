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
	"koro/internal/platform/privacy"
	"koro/internal/platform/realtime"
	"koro/internal/students"
)

// eventsWiring wires the events module (L8; 01 §4.10). First owner: BE-024.
// Only tickets of the events module edit this file (BE-001 R4).
// WebSocket hub shutdown goes to c.shutdownHooks (BE-054).
//
// BE-054 builds the live board's realtime.Hub (Redis pub/sub over
// c.infra.redis; without Redis — wiring and matrix tests — an in-process
// bus; Origin check against TRUSTED_ORIGINS, c.infra.platform.Realtime),
// hands it to events as Deps.Live, mounts GET
// /api/v1/events/{event_id}/check-ins/live (a gin route outside OpenAPI,
// authenticated by c.actors) and registers hub.CloseAll as a shutdown hook
// (1001 on every socket, 05 §10.9).
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
// BE-031 consumes c.identity.svc as events.Contacts (the registrant's
// address of the ticket mails, read in the worker; nil → the mail tasks
// fail and are retried), c.infra.queue (the resend task, mechanism A),
// the web origin of the mail links (IDENTITY_LINK_BASE_URL) and SMTP_FROM
// as the .ics ORGANIZER. The QR renderer is events.NewQRRenderer (yeqown/go-qrcode/v2).
//
// BE-050 consumes c.identity.svc as events.Reauthenticator (the proof of
// createEventExport; nil → 503) and c.files.svc for the export files
// (PutGenerated, Attach, PresignGet, Delete: the 7-day purge deletes the
// file in the transaction that clears its reference, 01 §6.3).
//
// BE-083 (T-318) consumes hooks.destruction (kvkk, L13, read at call time)
// as events.Destructions: the purge records each deleted export file in
// destruction_logs in its transaction; a nil hook refuses the purge.
//
// Provided, set in build: svc (events.Events for feed and catalog), the
// students hook c.students.registrations (T-241: events.Registrations
// adapted to students.Registrations), the events KVKK provider (staff
// assignments, registrations, tickets, check-ins) and the crypto.rewrap
// declaration of event_registrations.answers_sensitive_enc.
// Worker (tasks): event.cancelled → events.cancel_registrations, the
// registration.* notices, events.waitlist_expire and
// events.reconcile_counts (01 §6.2, §7); BE-031: registration.confirmed →
// events.send_ticket_email, ticket.reissued →
// events.send_reissued_ticket_email (critical), event.schedule_changed →
// events.send_schedule_update, registration.cancelled →
// events.send_cancellation_email, attendance.finalized →
// events.mark_no_shows, the task events.resend_ticket_email and the job
// events.auto_finalize_attendance; BE-050: the task events.build_export
// and the job events.purge_exports (hourly :25, low).
type eventsWiring struct {
	store    events.Store
	newStore func(c *container) events.Store
	tx       db.TxManager
	sealer   events.Sealer
	tickets  events.TicketKeys

	mod   events.Module
	svc   events.Service    // set by build
	built events.Store      // the store build used (wiring and matrix tests seed it)
	keys  events.TicketKeys // the ticket key ring build used (matrix fixtures derive the tickets' tokens)
	files events.Files      // c.files.svc, kept for the matrix fixtures (export files)
	hub   *realtime.Hub     // the live board's WebSocket hub (BE-054); nil under `koro openapi`
	// live overrides the hub's limits and timings (tests); Bus, Clock, Log
	// and TrustedOrigins are always set by build.
	live realtime.Config
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
		QR: events.NewQRRenderer(), Queue: c.infra.queue, ICSOrganizer: c.infra.cfg.Notify.SMTPFrom,
		LinkBaseURL: c.infra.cfg.Identity.LinkBaseURL, // the web application origin (IDENTITY_LINK_BASE_URL)
	}
	if c.identity.cfg != nil {
		d.LinkBaseURL = c.identity.cfg.LinkBaseURL
	}
	if svc := c.identity.svc; svc != nil {
		d.Users, d.Contacts, d.Reauth = svc, svc, svc
	}
	if svc := c.files.svc; svc != nil {
		d.Files = svc
	}
	d.Destructions = eventsDestructions{c: c} // T-318 (BE-083)
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
	hub, err := w.newHub(c)
	if err != nil {
		return err
	}
	d.Live = hub
	mod, err := events.New(d)
	if err != nil {
		return err
	}
	w.mod, w.svc, w.built, w.keys, w.files, w.hub = mod, mod.Service, store, d.Tickets, d.Files, hub
	c.shutdownHooks = append(c.shutdownHooks, hub.CloseAll)
	c.privacy = append(c.privacy, mod.Privacy)
	c.students.registrations = eventsRegistrations{svc: mod.Service} // T-241
	c.sweepers = append(c.sweepers, mod.Sweeper)
	return nil
}

// newHub builds the live board's hub: Redis pub/sub between the API
// replicas, an in-process bus without Redis (wiring and matrix tests).
func (w *eventsWiring) newHub(c *container) (*realtime.Hub, error) {
	cfg := w.live
	cfg.Bus = realtime.NewMemBus()
	if c.infra.redis != nil {
		cfg.Bus = realtime.NewRedisBus(c.infra.redis)
	}
	cfg.Clock, cfg.Log = c.infra.clock, c.infra.log.With("component", "realtime")
	cfg.TrustedOrigins = c.infra.platform.Realtime.TrustedOrigins
	hub, err := realtime.NewHub(cfg)
	if err != nil {
		return nil, fmt.Errorf("events: %w", err)
	}
	return hub, nil
}

// eventsRegistrations adapts events.Registrations to students.Registrations
// (T-241: the member card's club_event_registrations_count and the admin
// view's registrations[]; sensitive events are left out by events).
type eventsRegistrations struct{ svc events.Registrations }

func (r eventsRegistrations) CountInOrg(ctx context.Context, orgID, userID uuid.UUID) (int, error) {
	return r.svc.CountInOrg(ctx, orgID, userID)
}

func (r eventsRegistrations) SummaryOf(ctx context.Context, userID uuid.UUID) ([]students.RegistrationSummary, error) {
	rows, err := r.svc.SummaryOf(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]students.RegistrationSummary, len(rows))
	for i, x := range rows {
		out[i] = students.RegistrationSummary{EventID: x.EventID, EventTitle: x.EventTitle, Status: string(x.Status),
			RegisteredAt: x.RegisteredAt}
	}
	return out, nil
}

// tasks: event.cancelled → events.cancel_registrations, the
// registration.submitted/waitlisted/rejected/cancelled notices (BE-028),
// the ticket, schedule and cancellation mails and the no-shows (BE-031;
// event.cancelled's offerings are catalog's), the resend and export build
// tasks and the jobs events.waitlist_expire, events.reconcile_counts,
// events.auto_finalize_attendance and events.purge_exports (BE-050).
func (w *eventsWiring) tasks(c *container) {
	w.mod.Subscribe(c.infra.subs)
	for _, t := range w.mod.Tasks() {
		c.infra.jobs.Handle(t)
	}
	for _, j := range w.mod.Jobs() {
		c.infra.jobs.Schedule(j)
	}
}

// routes: the 12 operations of BE-024, the 7 of BE-026, the 8 of BE-028,
// the 9 of BE-031 and the 3 of BE-050 (04 §2.6), and the live board's
// WebSocket (BE-054; a gin route, not an operation). w.svc and w.hub are
// nil under `koro openapi`, which both tolerate.
func (w *eventsWiring) routes(c *container, api huma.API) {
	events.Register(api, w.svc)
	events.RegisterLive(api, w.svc, w.hub, c.actors)
}

// eventsSender adapts notify (L3) to events.Sender, read at call time; a
// missing sender fails the task (retried), it never drops the notice.
type eventsSender struct{ c *container }

func (n eventsSender) Send(ctx context.Context, m notify.Message) (uuid.UUID, error) {
	if n.c.notify.svc == nil {
		return uuid.Nil(), errors.New("events: notify is not wired")
	}
	return n.c.notify.svc.Send(ctx, m)
}

// eventsDestructions adapts hooks.destruction (kvkk, L13; filled after
// events is built) to events.Destructions, read at call time. A missing
// hook is an error: the purge rolls back rather than destroy unrecorded.
type eventsDestructions struct{ c *container }

func (r eventsDestructions) Record(ctx context.Context, d privacy.Destruction) error {
	h := r.c.hooks.destruction
	if h == nil {
		return errors.New("events: kvkk destruction record is not wired")
	}
	return h(ctx, d)
}
