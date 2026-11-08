package main

import (
	"fmt"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/stats"
)

// statsWiring wires the stats module (L13, next to kvkk: it reads every
// lower layer; BE-163). Only tickets of the stats module edit this file
// (BE-001 R4).
//
// stats owns no table, job, event or seam: its ports are the Statistics
// reads of the modules that own the rows, asserted on their services
// (the reads are methods of each concrete service next to its Service
// contract, like students.DiscoveryIndex): orgs (OrgDirectory and
// Statistics), directory (Catalog and Statistics), identity, feed,
// events (Statistics and Events: a sponsored event's summary, BE-189),
// orders (sales and purchases), outreach, messaging, entitlements,
// students and catalog, and the queue counters of kvkk and identity
// (BE-192: gated by the queue's own list action; getAdminQueues, BE-196,
// reads the same ports).
// A module that does not offer its read stops the build.
type statsWiring struct {
	svc stats.Service // set by build
}

func (w *statsWiring) build(c *container) error {
	d := stats.Deps{Clock: c.infra.clock, Log: c.infra.log.With("module", "stats")}
	var missing []string
	port := func(name string, svc any, set func(any) bool) {
		if svc == nil || !set(svc) {
			missing = append(missing, name)
		}
	}
	port("orgs", c.orgs.svc, func(v any) (ok bool) { d.Orgs, ok = v.(stats.Orgs); return })
	port("orgs statistics", c.orgs.svc, func(v any) (ok bool) { d.OrgCounts, ok = v.(stats.OrgCounts); return })
	port("directory catalog", c.directory.svc, func(v any) (ok bool) { d.Clubs, ok = v.(stats.Clubs); return })
	port("directory statistics", c.directory.svc, func(v any) (ok bool) { d.Universities, ok = v.(stats.Universities); return })
	port("identity statistics", c.identity.svc, func(v any) (ok bool) { d.Users, ok = v.(stats.Users); return })
	port("feed statistics", c.feed.svc, func(v any) (ok bool) { d.Content, ok = v.(stats.Content); return })
	port("events statistics", c.events.svc, func(v any) (ok bool) { d.Events, ok = v.(stats.Events); return })
	port("orders statistics", c.orders.svc, func(v any) (ok bool) { d.Sales, ok = v.(stats.Sales); return })
	port("orders purchases", c.orders.svc, func(v any) (ok bool) { d.Purchases, ok = v.(stats.Purchases); return })
	port("events summaries", c.events.svc, func(v any) (ok bool) { d.EventInfo, ok = v.(stats.EventInfo); return })
	port("outreach statistics", c.outreach.svc, func(v any) (ok bool) { d.Outreach, ok = v.(stats.Outreach); return })
	port("messaging statistics", c.messaging.svc, func(v any) (ok bool) { d.Contacts, ok = v.(stats.Contacts); return })
	port("entitlements statistics", c.entitlements.svc, func(v any) (ok bool) { d.Packages, ok = v.(stats.Packages); return })
	port("students statistics", c.students.svc, func(v any) (ok bool) { d.Students, ok = v.(stats.Students); return })
	port("catalog statistics", c.catalog.svc, func(v any) (ok bool) { d.Offerings, ok = v.(stats.Offerings); return })
	port("kvkk queue statistics", c.kvkk.svc, func(v any) (ok bool) { d.DataRequests, ok = v.(stats.DataRequests); return })
	port("identity edu queue statistics", c.identity.svc, func(v any) (ok bool) { d.StudentDocs, ok = v.(stats.StudentDocuments); return })
	if len(missing) > 0 {
		return fmt.Errorf("stats: %v not built or without the read stats needs", missing)
	}
	mod, err := stats.New(d)
	if err != nil {
		return err
	}
	w.svc = mod.Service
	return nil
}

// tasks: none (stats owns no table, event or job).
func (w *statsWiring) tasks(c *container) {}

// routes: getAdminStats, getAdminQueues (BE-196), getClubStats,
// getCompanyStats, getUniversityStats (04 §2.23); w.svc is nil under
// `koro openapi`, which Register tolerates.
func (w *statsWiring) routes(c *container, api huma.API) { stats.Register(api, w.svc) }
