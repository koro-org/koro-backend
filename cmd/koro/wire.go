package main

// Composition root (05 §3, BE-001 R4). This file is frozen after P0: only
// BE-002 (infra.db, readyz DB check), BE-004 (hooks.memberships, test
// ActorResolver) and BE-006 (infra.outbox/queue/limiter, subscription and
// job lists, worker registration) may add to it. Feature modules never
// edit it; each module edits only cmd/koro/wire_<module>.go and
// cmd/koro/wire_<module>_test.go.
//
// Pattern for wire_<module>.go (see wire_audit.go for the stub):
//
//	type <module>Wiring struct {      // module state on the container
//		store <module>.Store          // nil → GORM store in build; tests preset the in-memory store
//		svc   <module>.Service        // set by build; consumers in higher layers read it
//	}
//	func (w *<module>Wiring) build(c *container) error       // New(Deps) with stores, lower-layer services,
//	                                                         // hooks; registers privacy providers/sweepers,
//	                                                         // fills the hooks it provides
//	func (w *<module>Wiring) tasks(c *container)             // c.infra.subs.Subscribe(topic, "<module>.<verb>_<object>", h, opts...),
//	                                                         // c.infra.jobs.Schedule(queue.Job{...}) (01 §7; Unique mandatory),
//	                                                         // c.infra.jobs.Handle(queue.Task{...}), c.infra.inbox.Register(...)
//	func (w *<module>Wiring) routes(c *container, api huma.API) // <module>.Register(api, w.svc); w.svc may be
//	                                                         // nil (koro openapi): Register must not touch it
//
// and wire_<module>_test.go:
//
//	func (w *<module>Wiring) useMemStore()                   // installs the in-memory store (05 §13.2)
//
// build, tasks and routes run in the fixed layer order of 01 §5.1
// (TestWire_LayerOrderMatchesDesign), so a module may use the services of
// lower layers in build. Cross-layer calls upwards go through hooks.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"github.com/danielgtaylor/huma/v2"
	"github.com/redis/go-redis/v9"

	"koro/db/migrations"
	"koro/internal/authz"
	"koro/internal/platform/clock"
	"koro/internal/platform/config"
	"koro/internal/platform/db"
	"koro/internal/platform/httpx"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/inbox"
	"koro/internal/platform/migratekit"
	"koro/internal/platform/outbox"
	"koro/internal/platform/privacy"
	"koro/internal/platform/queue"
	"koro/internal/platform/ratelimit"
)

// infra is the shared infrastructure handed to every module.
type infra struct {
	log     *slog.Logger
	clock   clock.Clock
	version string         // binary version (git sha)
	cfg     config.Modules // zero value under `koro openapi`
	// db is the app_rw pool (BE-002); nil under `koro openapi` and in the
	// wiring tests (modules then keep their in-memory stores).
	db *db.DB

	// BE-006. Production: newInfra over db and Redis. Wiring tests: the
	// in-memory variants (wire_test.go). `koro openapi`: nil except the
	// registries (no module is built there).
	redis       redis.UniversalClient // nil without Redis (tests, openapi)
	queue       queue.Enqueuer        // services: AfterCommit tasks without a topic (mechanism A)
	outbox      *outbox.Outbox        // services: outbox.Writer (Add = O, AfterCommit = A); worker: relay
	idempotency *idempotency.Keeper   // services: idempotency.Do in the business transaction (06 H-5)
	inbox       *inbox.Inbox          // webhook handlers: Receive; provider modules: Register
	limiter     ratelimit.Limiter     // API rate limit middleware
	platform    config.Platform       // queue/outbox/idempotency/inbox settings (zero in tests)
	webcache    config.Webcache       // BE-106: the worker's publication webhook (zero outside `koro worker`)

	// subs and jobs collect what modules declare in tasks(c): outbox
	// subscriptions, scheduled jobs and task handlers. Both api and worker
	// build them (AfterCommit fans out by subscription in the api); only
	// `koro worker` mounts them.
	subs *outbox.Registry
	jobs *queue.Registry
}

// newInfra completes base with the BE-006 infrastructure of a long-running
// process: asynq client, outbox, idempotency keeper, webhook inbox and rate
// limiter over base.db and rdb.
func newInfra(base infra, rdb redis.UniversalClient, p config.Platform) infra {
	in := base
	in.redis, in.platform = rdb, p
	in.subs, in.jobs = outbox.NewRegistry(), queue.NewRegistry()
	in.queue = queue.NewClient(rdb, in.log.With("component", "queue"))
	in.outbox = outbox.New(outbox.Deps{
		Store: outbox.NewGormStore(in.db.ORM), Tx: in.db.Tx, Subs: in.subs, Queue: in.queue,
		Clock: in.clock, Log: in.log.With("component", "outbox"),
	})
	in.idempotency = idempotency.New(idempotency.Deps{
		Tx: in.db.Tx, Store: idempotency.NewGormStore(in.db.ORM), Clock: in.clock,
		Log: in.log.With("component", "idempotency"), TTL: p.Idempotency.TTL,
	})
	in.inbox = inbox.New(inbox.Deps{
		Store: inbox.NewGormStore(in.db.ORM), Tx: in.db.Tx, Queue: in.queue, Clock: in.clock,
		Log: in.log.With("component", "inbox"),
	})
	in.limiter = ratelimit.NewRedis(rdb)
	return in
}

// hooks are late bindings between modules that may not import each other
// (01 §5.3, T-77). Hook types use only stdlib, platform/* and authz types;
// the consumer adapts a hook to its own port in its wire file. A nil hook
// (or an empty list) means "provider not wired yet": the consumer must fall
// back to the safe default documented on each field.
type hooks struct {
	// domainResolver maps an e-mail domain to a university.
	// Provider: directory (BE-020). Consumer: identity (BE-018).
	// nil → 422 identity.edu_domain_unknown (document upload path).
	domainResolver func(ctx context.Context, emailDomain string) (universityID uuid.UUID, found bool, err error)

	// clubInfo returns the join-relevant facts of a club org.
	// Provider: directory (BE-020). Consumer: orgs joinClub (BE-019).
	// nil or not found → 404.
	clubInfo func(ctx context.Context, clubOrgID uuid.UUID) (club hookClub, found bool, err error)

	// anonResolver maps a company-scoped anon_id to a user.
	// Provider: discovery (BE-051). Consumer: messaging (BE-056).
	// nil or not found → 404.
	anonResolver func(ctx context.Context, companyOrgID uuid.UUID, anonID string) (userID uuid.UUID, found bool, err error)

	// collectionAccount returns Koro's active default collection account
	// for payment instructions. Provider: payments (BE-041). Consumer:
	// orders (BE-040). nil or not found → 409 payments.no_active_collection_account.
	collectionAccount func(ctx context.Context) (acct hookCollectionAccount, found bool, err error)

	// openCommerce lists checks that block closing an org: orders adds
	// "open orders" (BE-040), ledger adds "open payouts" (BE-044).
	// Consumer: orgs closeOrg (BE-017). Empty → no check.
	openCommerce []func(ctx context.Context, orgID uuid.UUID) (open bool, err error)

	// talentAuditor exposes the talent auditor report and neutralisation.
	// Provider: talent (BE-047/BE-052). Consumer: kvkk (BE-021).
	// nil → the DSR report has no talent section and nothing is neutralised.
	talentAuditor *hookTalentAuditor

	// memberships returns the active org memberships of a user for Actor
	// resolution (03 §2.1; orgs.OrgDirectory.MembershipsOf), sensitive clubs
	// included. Provider: orgs (BE-019). Consumer: cmd/koro/actor.go
	// (BE-014). nil → the actor has no memberships (personal and public
	// actions only; every org surface is 404).
	memberships func(ctx context.Context, userID uuid.UUID) ([]authz.Membership, error)

	// retention returns the period of a kvkk.retention_policies code for the
	// platform cleanup jobs (01 §7: "süreler koddan değil
	// retention_policies'ten"): outbox_published, webhook_payload,
	// webhook_row. Provider: kvkk (BE-049). Consumer: platformTasks (BE-006).
	// nil or not found → the config default (OUTBOX_RETENTION,
	// WEBHOOK_PAYLOAD_RETENTION, WEBHOOK_ROW_RETENTION).
	retention func(ctx context.Context, code string) (period time.Duration, found bool, err error)

	// destruction appends one destruction_logs row in the caller's
	// transaction (02 §16.3) for a destruction a module job runs on its own
	// clock (T-318: events.purge_exports). Provider: kvkk (BE-083).
	// Consumer: events (BE-083). nil → the consumer refuses to destroy (the
	// row must exist; the files wait for the next run).
	destruction func(ctx context.Context, d privacy.Destruction) error
}

// hookClub is the result of hooks.clubInfo.
type hookClub struct {
	UniversityID uuid.UUID
	JoinPolicy   string // "open" | "approval" (02 clubs.join_policy)
	Sensitive    bool   // clubs.is_sensitive
}

// hookCollectionAccount is the result of hooks.collectionAccount.
type hookCollectionAccount struct {
	ID         uuid.UUID
	IBAN       string
	HolderName string
	BankName   string
}

// hookTalentAuditor is the talent auditor surface used by kvkk (01 §4.11).
type hookTalentAuditor struct {
	// Report returns the auditor report of userID as opaque JSON (never a
	// score in any other DTO); requestedBy is audited as talent.read.
	Report func(ctx context.Context, requestedBy, userID uuid.UUID, reason string) (json.RawMessage, error)
	// Neutralize applies a neutralisation (reason: "objection", "minor", ...).
	Neutralize func(ctx context.Context, userID uuid.UUID, reason string, dsrID *uuid.UUID) error
}

// container holds infrastructure, hooks, registries and the per-module
// wiring state. Modules reach each other only through these fields.
type container struct {
	infra infra
	hooks hooks

	// actors resolves credentials into actors for the HTTP auth middleware
	// and any non-huma route (WebSocket). BE-014 sets it in build
	// (cmd/koro/actor.go); nil → protected operations 401.
	//
	// Tests preset it before build (authz matrix: authztest.Resolver, persona
	// token → Actor; BE-004). build keeps a preset resolver: a module that
	// replaces it fails the build, so no wiring can bypass the test resolver.
	// A module therefore sets actors only when it is nil.
	actors httpx.ActorResolver

	// terms answers the terms gate (httpx.TermsGate, BE-124): has the
	// signed-in user accepted the terms of use in force. identity sets it in
	// build (only when nil: tests may preset one); nil → apiMiddleware
	// installs no gate.
	terms httpx.TermsChecker

	// privacy and sweepers are the KVKK registries read by kvkk (01 §3.3).
	privacy  []privacy.Provider
	sweepers []privacy.RetentionSweeper

	// readyChecks are added to /readyz; shutdownHooks run on
	// http.Server.Shutdown (WebSocket hubs, 05 §10.9).
	readyChecks   []httpx.Check
	shutdownHooks []func()

	// keys is platform/crypto (BE-055, wire_crypto.go): the shared Envelope
	// modules seal with (c.keys.envelope()), the openbao readiness check and
	// the crypto.* jobs. Built before the modules.
	keys cryptoKeys

	// web is the publication webhook (BE-106, wire_webcache.go): the
	// webcache.revalidate_* subscriptions, registered with the platform's
	// tasks once the modules are built (it reads events and orgs).
	web webRevalidation

	// Feature modules, in layer order (01 §5.1).
	audit        auditWiring        // L1
	files        filesWiring        // L2
	consent      consentWiring      // L2
	entitlements entitlementsWiring // L2
	notify       notifyWiring       // L3
	identity     identityWiring     // L4
	orgs         orgsWiring         // L5
	directory    directoryWiring    // L6
	students     studentsWiring     // L7
	parties      partiesWiring      // L7
	events       eventsWiring       // L8
	talent       talentWiring       // L8
	outreach     outreachWiring     // L8
	feed         feedWiring         // L9
	catalog      catalogWiring      // L9
	discovery    discoveryWiring    // L9
	messaging    messagingWiring    // L10
	orders       ordersWiring       // L10
	ledger       ledgerWiring       // L11
	billing      billingWiring      // L11
	payments     paymentsWiring     // L12
	kvkk         kvkkWiring         // L13
	stats        statsWiring        // L13 (BE-163: reads every lower layer)
}

func newContainer(in infra) *container {
	if in.subs == nil {
		in.subs = outbox.NewRegistry()
	}
	if in.jobs == nil {
		in.jobs = queue.NewRegistry()
	}
	c := &container{infra: in}
	if in.db != nil {
		c.readyChecks = append(c.readyChecks, dbReadyChecks(in.db)...)
	}
	if in.redis != nil {
		c.readyChecks = append(c.readyChecks, httpx.Check{Name: "redis", Run: func(ctx context.Context) error {
			return queue.Ping(ctx, in.redis)
		}})
	}
	return c
}

// openDB opens the application pool (app_rw, DATABASE_URL) of a
// long-running process (BE-002). app is the application_name.
func openDB(ctx context.Context, cfg config.DB, app string, clk clock.Clock, log *slog.Logger) (*db.DB, error) {
	return db.Open(ctx, db.Config{
		URL:              cfg.URL.Reveal(),
		AppName:          app,
		MaxConns:         cfg.MaxConns,
		MinConns:         cfg.MinConns,
		StatementTimeout: cfg.StatementTimeout,
		IdleInTxTimeout:  cfg.IdleInTxTimeout,
		Clock:            clk,
	}, log)
}

// dbReadyChecks are the /readyz checks of the database (BE-002): the pool
// answers, and the schema is clean and at least at the newest migration
// embedded in this binary (a newer schema is fine: the migrate job runs
// before a rolling deploy, 05 §7.3).
func dbReadyChecks(d *db.DB) []httpx.Check {
	want, wantErr := migratekit.Latest(migrations.FS)
	return []httpx.Check{
		{Name: "db", Run: d.Ping},
		{Name: "migrations", Run: func(ctx context.Context) error {
			if wantErr != nil {
				return wantErr
			}
			return migratekit.CheckVersion(ctx, d.Pool, want)
		}},
	}
}

// moduleWiring is implemented by every <module>Wiring.
type moduleWiring interface {
	build(c *container) error
	tasks(c *container)
	routes(c *container, api huma.API)
}

// moduleEntry is one row of the fixed module table.
type moduleEntry struct {
	name  string
	layer int
	w     moduleWiring
}

// modules returns the 23 feature modules in the layer order of 01 §5.1
// (lower layers first). The order is the build, task and route order.
func (c *container) modules() []moduleEntry {
	return []moduleEntry{
		{"audit", 1, &c.audit},
		{"files", 2, &c.files},
		{"consent", 2, &c.consent},
		{"entitlements", 2, &c.entitlements},
		{"notify", 3, &c.notify},
		{"identity", 4, &c.identity},
		{"orgs", 5, &c.orgs},
		{"directory", 6, &c.directory},
		{"students", 7, &c.students},
		{"parties", 7, &c.parties},
		{"events", 8, &c.events},
		{"talent", 8, &c.talent},
		{"outreach", 8, &c.outreach},
		{"feed", 9, &c.feed},
		{"catalog", 9, &c.catalog},
		{"discovery", 9, &c.discovery},
		{"messaging", 10, &c.messaging},
		{"orders", 10, &c.orders},
		{"ledger", 11, &c.ledger},
		{"billing", 11, &c.billing},
		{"payments", 12, &c.payments},
		{"kvkk", 13, &c.kvkk},
		{"stats", 13, &c.stats},
	}
}

// build constructs every module bottom-up, then collects the platform's
// and the modules' outbox subscriptions, scheduled jobs and task handlers.
// A bad registration (unknown topic, job without Unique, duplicate task
// type) stops the process at start.
func (c *container) build() error {
	mods := c.modules()
	preset := c.actors // nil in production; the persona resolver in matrix tests
	if err := c.keys.build(c); err != nil {
		return fmt.Errorf("wire crypto: %w", err)
	}
	for _, m := range mods {
		if err := m.w.build(c); err != nil {
			return fmt.Errorf("wire %s: %w", m.name, err)
		}
		if preset != nil && c.actors != preset {
			return fmt.Errorf("wire %s: replaced the preset ActorResolver (set c.actors only when it is nil)", m.name)
		}
	}
	c.platformTasks()
	for _, m := range mods {
		m.w.tasks(c)
	}
	if err := errors.Join(c.infra.subs.Err(), c.infra.jobs.Err()); err != nil {
		return fmt.Errorf("wire tasks: %w", err)
	}
	if _, err := c.workerMux(c.infra.log); err != nil {
		return fmt.Errorf("wire tasks: %w", err)
	}
	return nil
}

// platformTasks registers the platform's own jobs and tasks (01 §7):
// outbox.cleanup, idempotency.cleanup, webhook_inbox.purge_payloads, the
// webhook_inbox.process task, the crypto jobs and the publication webhook's
// subscriptions.
func (c *container) platformTasks() {
	in, p := c.infra, c.infra.platform
	if in.outbox != nil {
		in.jobs.Schedule(in.outbox.CleanupJob(c.retention("outbox_published", p.Outbox.Retention, outbox.DefaultRetention)))
	}
	if in.idempotency != nil {
		in.jobs.Schedule(in.idempotency.CleanupJob())
	}
	if in.inbox != nil {
		in.jobs.Handle(in.inbox.Task())
		in.jobs.Schedule(in.inbox.PurgeJob(
			c.retention("webhook_payload", p.Inbox.PayloadRetention, inbox.DefaultPayloadRetention),
			c.retention("webhook_row", p.Inbox.RowRetention, inbox.DefaultRowRetention)))
	}
	c.keys.tasks(c) // crypto.rewrap, crypto.renew_token (BE-055)
	c.web.tasks(c)  // webcache.revalidate_* (BE-106)
}

// retention resolves a retention period at run time: hooks.retention
// (kvkk.retention_policies) first, then the configured value, then def.
func (c *container) retention(code string, configured, def time.Duration) func(context.Context) (time.Duration, error) {
	return func(ctx context.Context) (time.Duration, error) {
		if h := c.hooks.retention; h != nil {
			d, found, err := h(ctx, code)
			if err != nil {
				return 0, fmt.Errorf("retention %s: %w", code, err)
			}
			if found && d > 0 {
				return d, nil
			}
		}
		if configured > 0 {
			return configured, nil
		}
		return def, nil
	}
}

// workerMux routes every subscription, task and job by task type; a type
// registered twice across the three lists is an error.
func (c *container) workerMux(log *slog.Logger) (*queue.Mux, error) {
	mux := queue.NewMux(log)
	var errs []error
	for _, s := range c.infra.subs.All() {
		errs = append(errs, mux.Handle(s.Name, s.HandleTask))
	}
	for _, t := range c.infra.jobs.Tasks() {
		errs = append(errs, mux.Handle(t.Name, t.Handle))
	}
	for _, j := range c.infra.jobs.Jobs() {
		errs = append(errs, mux.Handle(j.Name, queue.JobHandler(j)))
	}
	return mux, errors.Join(errs...)
}

// apiMiddleware installs the operation middlewares on the root API, after
// authentication and before any route is registered: the
// x-koro-rate-limit class (unless disabled in dev; BE-006), the terms gate
// (BE-124: a refused write still counts against its rate limit and is
// refused before its Idempotency-Key is read) and Idempotency-Key parsing
// (06 H-5: the key is stored by the service, not here).
func (c *container) apiMiddleware(api huma.API, rateLimit bool) {
	if rateLimit && c.infra.limiter != nil {
		api.UseMiddleware(ratelimit.Middleware(api, c.infra.limiter, c.infra.log))
	}
	if c.terms != nil {
		api.UseMiddleware(httpx.TermsGate(api, c.terms))
	}
	api.UseMiddleware(idempotency.Middleware(api))
}

// registerRoutes registers every module's operations on the /api/v1 group.
func (c *container) registerRoutes(api huma.API) {
	for _, m := range c.modules() {
		m.w.routes(c, api)
	}
}
