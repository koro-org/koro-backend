package main

import (
	"context"
	"encoding/json"
	"errors"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/platform/config"
	"koro/internal/platform/db"
	"koro/internal/platform/obs"
	"koro/internal/talent"
)

// talentWiring wires the talent module (L8; 01 §4.11). First owner: BE-047.
// Only tickets of the talent module edit this file (BE-001 R4).
//
// Seams (nil in production):
//
//	store  nil → talent.NewGormStore over the app_rw pool (every talent
//	       transaction switches to app_talent); tests preset the memory store
//	tx     nil → the app_rw pool's TxManager; tests preset db.NopTx
//	cfg    tests only; nil → the talent section of the loaded configuration
//	       (TALENT_DEFAULT_ENABLED, default false: nothing is written, T-32)
//	       and TALENT_AUDITOR_API_ENABLED (default false: every
//	       talent_auditor endpoint answers 404)
//
// Consumed (built in lower layers): c.audit.svc, c.identity.svc (the
// re-authentication proof of the R operations; nil → 503), c.students.svc
// (cohort, minor flag, ranking choice) and c.directory.svc (sensitive
// clubs).
//
// Provided, set in build: mod (talent.Ranking for discovery, BE-051;
// talent.Admin for the talent_auditor endpoints, BE-052), the talent KVKK
// provider, the retention sweeper on c.sweepers (talent_ledger,
// talent_counters, talent_runs, talent_neutralization; BE-079) and
// hooks.talentAuditor (kvkk: the profiling_access report and the
// neutralisation). tasks registers the 14 consumers of 01 §6.2 and the
// nightly talent.recompute (01 §7); routes the seven talent_auditor
// operations of 04 §T; metrics registers koro_talent_ingest_total on the
// worker's registry.
type talentWiring struct {
	store talent.Store
	tx    db.TxManager
	cfg   *config.Talent

	mod talent.Module // set by build
}

func (w *talentWiring) build(c *container) error {
	cfg := c.infra.cfg.Talent
	if w.cfg != nil {
		cfg = *w.cfg
	}
	store, tx := w.store, w.tx
	if c.infra.db != nil {
		if store == nil {
			store = talent.NewGormStore(c.infra.db.ORM)
		}
		if tx == nil {
			tx = c.infra.db.Tx
		}
	}
	if store == nil || tx == nil {
		return errors.New("talent: no store (database not opened)")
	}
	if c.students.svc == nil || c.directory.svc == nil {
		return errors.New("talent: students or directory is not built")
	}
	log := c.infra.log.With("module", "talent")
	d := talent.Deps{
		Store: store, Tx: tx, Audit: c.audit.svc, Clock: c.infra.clock, Log: log,
		Students: c.students.svc, Directory: c.directory.svc,
		CollectionEnabled: cfg.DefaultEnabled, AuditorAPIEnabled: cfg.AuditorAPIEnabled,
	}
	if svc := c.identity.svc; svc != nil {
		d.Reauth = svc
	}
	mod, err := talent.New(d)
	if err != nil {
		return err
	}
	w.mod = mod
	c.privacy = append(c.privacy, mod.Privacy)
	c.sweepers = append(c.sweepers, mod.Sweeper)
	c.hooks.talentAuditor = &hookTalentAuditor{
		Report: func(ctx context.Context, requestedBy, userID uuid.UUID, reason string) (json.RawMessage, error) {
			return mod.Auditor.SubjectReport(ctx, requestedBy, userID, reason)
		},
		Neutralize: func(ctx context.Context, userID uuid.UUID, reason string, dsrID *uuid.UUID) error {
			return mod.Auditor.Neutralize(ctx, userID, talent.NeutralizeReason(reason), dsrID)
		},
	}
	if cfg.DefaultEnabled {
		log.Warn("talent collection is on (TALENT_DEFAULT_ENABLED=true): the legal basis and the privacy notice must be in force (T-32)")
	}
	if cfg.AuditorAPIEnabled {
		log.Warn("talent auditor endpoints are open (TALENT_AUDITOR_API_ENABLED=true): talent_auditor only, every call audited")
	}
	return nil
}

// tasks: registration.confirmed/cancelled/no_show, checkin.recorded/voided,
// membership.started/ended, student.profile_section_completed, post.liked/
// unliked/saved and user.active_day on the low queue;
// student.ranking_choice_changed and dsr.profiling_objection on default;
// the job talent.recompute (03:00, low).
func (w *talentWiring) tasks(c *container) {
	w.mod.Subscribe(c.infra.subs)
	for _, j := range w.mod.Jobs() {
		c.infra.jobs.Schedule(j)
	}
}

// routes: the talent_auditor endpoints (04 §T). w.mod.Admin is nil for
// `koro openapi` (no build): handlers only touch it when called.
func (w *talentWiring) routes(c *container, api huma.API) { talent.Register(api, w.mod.Admin) }

// metrics registers koro_talent_ingest_total{signal, outcome} on the
// worker's registry (the consumers run there); nil m → metrics off.
func (w *talentWiring) metrics(m *obs.Metrics) error {
	return m.CounterFunc("koro_talent_ingest_total",
		"Consumed talent signals by signal and outcome (flag_off: talent.default_enabled is false, nothing written).",
		[]string{"signal", "outcome"}, func() []obs.Sample {
			cs := w.mod.Counters()
			out := make([]obs.Sample, len(cs))
			for i, s := range cs {
				out[i] = obs.Sample{Labels: []string{s.Signal, s.Outcome}, Value: float64(s.Value)}
			}
			return out
		})
}
