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
//	func (w *<module>Wiring) tasks(c *container)             // appends outbox subscriptions and jobs (BE-006)
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
	"fmt"
	"log/slog"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/db/migrations"
	"koro/internal/authz"
	"koro/internal/platform/clock"
	"koro/internal/platform/config"
	"koro/internal/platform/db"
	"koro/internal/platform/httpx"
	"koro/internal/platform/migratekit"
	"koro/internal/platform/privacy"
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
	// BE-006: outbox, queue, limiter.
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

	// privacy and sweepers are the KVKK registries read by kvkk (01 §3.3).
	privacy  []privacy.Provider
	sweepers []privacy.RetentionSweeper

	// readyChecks are added to /readyz; shutdownHooks run on
	// http.Server.Shutdown (WebSocket hubs, 05 §10.9).
	readyChecks   []httpx.Check
	shutdownHooks []func()

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
}

func newContainer(in infra) *container {
	c := &container{infra: in}
	if in.db != nil {
		c.readyChecks = append(c.readyChecks, dbReadyChecks(in.db)...)
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

// modules returns the 22 feature modules in the layer order of 01 §5.1
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
	}
}

// build constructs every module bottom-up, then collects their outbox
// subscriptions and scheduled jobs.
func (c *container) build() error {
	mods := c.modules()
	preset := c.actors // nil in production; the persona resolver in matrix tests
	for _, m := range mods {
		if err := m.w.build(c); err != nil {
			return fmt.Errorf("wire %s: %w", m.name, err)
		}
		if preset != nil && c.actors != preset {
			return fmt.Errorf("wire %s: replaced the preset ActorResolver (set c.actors only when it is nil)", m.name)
		}
	}
	for _, m := range mods {
		m.w.tasks(c)
	}
	return nil
}

// registerRoutes registers every module's operations on the /api/v1 group.
func (c *container) registerRoutes(api huma.API) {
	for _, m := range c.modules() {
		m.w.routes(c, api)
	}
}
