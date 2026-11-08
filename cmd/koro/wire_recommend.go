package main

import (
	"fmt"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/recommend"
)

// recommendWiring wires the recommend module (L13, next to stats: it reads
// lower layers and nothing imports it; BE-174). Only tickets of the
// recommend module edit this file (BE-001 R4).
//
// recommend owns no table, job, event or seam: its ports are system reads
// asserted on the services of the modules that own the rows (each read is
// a method of the concrete service next to its Service contract, like
// students.DiscoveryIndex): students (students.Recommendations: the choice
// and the popular clubs), events (events.Recommendations: the signals and
// the candidates), directory (Catalog.SensitiveOrgIDs,
// Service.UniversityOrgs and directory.Recommendations: the club cards) and
// feed (feed.Follows; feed.Opportunities: the opportunity calls and their
// cards, BE-191) and messaging (messaging.ContactedOpportunities: the calls
// a company already contacted the student about, left out; BE-198; L10,
// built before recommend). A module that does not offer its read stops the
// build: without messaging's read recommend would recommend those calls
// again, so it does not start rather than answer without it.
type recommendWiring struct {
	svc recommend.Service // set by build
}

func (w *recommendWiring) build(c *container) error {
	d := recommend.Deps{Clock: c.infra.clock, Log: c.infra.log.With("module", "recommend")}
	var missing []string
	port := func(name string, svc any, set func(any) bool) {
		if svc == nil || !set(svc) {
			missing = append(missing, name)
		}
	}
	port("students recommendations", c.students.svc, func(v any) (ok bool) { d.Students, ok = v.(recommend.Students); return })
	port("events recommendations", c.events.svc, func(v any) (ok bool) { d.Events, ok = v.(recommend.Events); return })
	port("directory recommendations", c.directory.svc, func(v any) (ok bool) { d.Clubs, ok = v.(recommend.Clubs); return })
	port("feed follows", c.feed.svc, func(v any) (ok bool) { d.Follows, ok = v.(recommend.Follows); return })
	port("feed opportunities", c.feed.svc, func(v any) (ok bool) { d.Opportunities, ok = v.(recommend.Opportunities); return })
	port("messaging contacted opportunities", c.messaging.svc, func(v any) (ok bool) {
		d.ContactRequests, ok = v.(recommend.ContactRequests)
		return
	})
	if len(missing) > 0 {
		return fmt.Errorf("recommend: %v not built or without the read recommend needs", missing)
	}
	mod, err := recommend.New(d)
	if err != nil {
		return err
	}
	w.svc = mod.Service
	return nil
}

// tasks: none (recommend owns no table, event or job).
func (w *recommendWiring) tasks(c *container) {}

// routes: listMyRecommendations (04 §2.24); w.svc is nil under `koro
// openapi`, which Register tolerates.
func (w *recommendWiring) routes(c *container, api huma.API) { recommend.Register(api, w.svc) }
