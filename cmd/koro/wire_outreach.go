package main

import (
	"errors"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/outreach"
	"koro/internal/platform/db"
)

// outreachWiring wires the outreach module (L8; 01 §4.13). First owner:
// BE-030. Only tickets of the outreach module edit this file (BE-001 R4).
//
// Seams (nil in production):
//
//	store  nil → outreach.NewGormStore over the app_rw pool; tests preset the in-memory store
//	tx     nil → the app_rw pool's TxManager; tests preset db.NopTx
//
// Consumed (built in lower layers): c.audit.svc, c.students.svc
// (Audience.Count: count_segment, k ≥ 10), c.directory.svc (sensitive
// clubs, a university's org), c.orgs.svc (the org's status), the
// platform's rate limiter (the daily count budget per org,
// OUTREACH_COUNT_DAILY_LIMIT) and the idempotency keeper.
//
// Provided, set in build: svc (outreach.Service: Targeting.Compile for
// feed's targeted posts, BE-032; ScheduleDispatch joins with BE-035).
type outreachWiring struct {
	store outreach.Store
	tx    db.TxManager

	mod outreach.Module
	svc outreach.Service // set by build
}

func (w *outreachWiring) build(c *container) error {
	store, tx := w.store, w.tx
	if c.infra.db != nil {
		if store == nil {
			store = outreach.NewGormStore(c.infra.db.ORM)
		}
		if tx == nil {
			tx = c.infra.db.Tx
		}
	}
	if store == nil || tx == nil {
		return errors.New("outreach: no store (database not opened)")
	}
	d := outreach.Deps{
		Store: store, Tx: tx, Audit: c.audit.svc, Limiter: c.infra.limiter, Idempotency: c.infra.idempotency,
		Clock: c.infra.clock, Log: c.infra.log.With("module", "outreach"),
		DailyCountLimit: c.infra.cfg.Outreach.CountDailyLimit,
	}
	if svc := c.students.svc; svc != nil {
		d.Audience = svc
	}
	if svc := c.directory.svc; svc != nil {
		d.Catalog = svc
	}
	if svc := c.orgs.svc; svc != nil {
		d.Orgs = svc
	}
	mod, err := outreach.New(d)
	if err != nil {
		return err
	}
	w.mod, w.svc = mod, mod.Service
	return nil
}

// tasks schedules outreach.recount_segments (01 §7: 06:00, low).
func (w *outreachWiring) tasks(c *container) {
	for _, j := range w.mod.Jobs() {
		c.infra.jobs.Schedule(j)
	}
}

// routes: the 6 segment and audience count operations of BE-030 (04 §2.8);
// w.svc is nil under `koro openapi`, which Register tolerates.
func (w *outreachWiring) routes(c *container, api huma.API) { outreach.Register(api, w.svc) }
