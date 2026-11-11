package main

import (
	"errors"
	"log/slog"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/ingest"
	"koro/internal/platform/config"
	"koro/internal/platform/db"
	"koro/internal/platform/obs"
)

// ingestWiring wires the ingest module (L13, next to stats and recommend:
// it reads directory and orgs, opens clubs through directory and drafts
// through events, and nothing imports it; BE-210). Only tickets of the
// ingest module edit this file (BE-001 R4).
//
// Seams (nil in production):
//
//	store      nil → ingest.NewGormStore over the app_rw pool (newStore: tests' in-memory
//	           store); build keeps the store it used (the matrix fixtures seed it)
//	tx         nil → the app_rw pool's TxManager; tests preset db.NopTx
//	fetcher    nil → ingest.NewHTTPFetcher (the SSRF guard, robots.txt, the
//	           KoroBot User-Agent); tests preset an in-memory fetcher (no network)
//	extractor  nil → the INGEST_EXTRACTOR_PROVIDER adapter (extractors)
//
// Consumed: c.directory.svc (universities, clubs, the club import of
// directory.ClubImports, in ingest's transaction), c.orgs.svc (the club of
// a suggestion, the club names of the cards), c.events.svc (the draft of a
// promoted suggestion, in ingest's transaction), c.notify.svc (the new
// admin's notice), c.audit.svc and hooks.retention (imported_event, read
// at run time through c.retention).
//
// Provided: nothing to other modules. Tasks: ingest.fetch_due, ingest.purge_expired
// and the club_claim.decided subscription ingest.pause_claimed_sources.
type ingestWiring struct {
	store     ingest.Store
	newStore  func(c *container) ingest.Store // tests: the in-memory store over the container's clock
	tx        db.TxManager
	fetcher   ingest.Fetcher
	extractor ingest.Extractor

	mod ingest.Module
	svc ingest.Service // set by build
}

// extractors is the INGEST_EXTRACTOR_PROVIDER selection map (BE-113). The
// claude adapter without a key is the fake one (the ticket's rule), said
// at start.
func extractors(cfg config.Ingest, log *slog.Logger) adapters[ingest.Extractor] {
	return adapters[ingest.Extractor]{
		config.ExtractorFake: func() (ingest.Extractor, error) { return ingest.FakeExtractor{}, nil },
		config.ExtractorClaude: func() (ingest.Extractor, error) {
			if cfg.AnthropicAPIKey.IsZero() {
				log.Warn("INGEST_EXTRACTOR_PROVIDER=claude without INGEST_ANTHROPIC_API_KEY: the fake extractor runs")
				return ingest.FakeExtractor{}, nil
			}
			return ingest.NewClaudeExtractor(ingest.ClaudeConfig{
				APIKey: cfg.AnthropicAPIKey.Reveal(), Model: cfg.ExtractorModel, MaxTokens: cfg.ExtractorMaxTokens,
				Timeout: cfg.ExtractorTimeout,
			})
		},
	}
}

func (w *ingestWiring) build(c *container) error {
	store, tx := w.store, w.tx
	if store == nil && w.newStore != nil {
		store = w.newStore(c)
	}
	if c.infra.db != nil {
		if store == nil {
			store = ingest.NewGormStore(c.infra.db.ORM)
		}
		if tx == nil {
			tx = c.infra.db.Tx
		}
	}
	if store == nil || tx == nil {
		return errors.New("ingest: no store (database not opened)")
	}
	if c.directory.svc == nil || c.orgs.svc == nil || c.audit.svc == nil {
		return errors.New("ingest: directory, orgs and audit must be built first")
	}
	cfg := c.infra.cfg.Ingest
	log := c.infra.log.With("module", "ingest")
	fetcher := w.fetcher
	if fetcher == nil {
		fetcher = ingest.NewHTTPFetcher(ingest.FetcherConfig{
			UserAgent: cfg.UserAgent(), Timeout: cfg.FetchTimeout, MaxBytes: cfg.MaxBytes, RobotsTTL: 24 * time.Hour,
			Clock: c.infra.clock,
		})
	}
	extractor := w.extractor
	if extractor == nil {
		name := cfg.ExtractorProvider
		if name == "" {
			name = config.ExtractorFake // zero configuration (wiring tests)
		}
		var err error
		if extractor, err = extractors(cfg, log).pick("INGEST_EXTRACTOR_PROVIDER", name); err != nil {
			return err
		}
	}
	d := ingest.Deps{
		Store: store, Tx: tx, Directory: c.directory.svc, Orgs: c.orgs.svc, Fetcher: fetcher, Extractor: extractor,
		Audit: c.audit.svc, Clock: c.infra.clock, Log: log, Idempotency: c.infra.idempotency,
		Config: ingest.Config{
			FetchEnabled: cfg.FetchEnabled, SourceDefaultEnabled: cfg.SourceDefaultEnabled(),
			ListInterval: cfg.ListInterval, EventsInterval: cfg.EventsInterval, HostGap: cfg.HostGap,
			FailLimit: cfg.FailLimit, ZeroYieldAlarm: cfg.ZeroYieldAlarm, MaxTextChars: cfg.MaxTextChars,
			MinConfidence: cfg.MinConfidence, EventRetention: cfg.EventRetention,
		},
		EventRetention: c.retention("imported_event", cfg.EventRetention, 7*24*time.Hour),
	}
	if c.events.svc != nil {
		d.Drafts = c.events.svc
	}
	if c.notify.svc != nil {
		d.Sender = c.notify.svc
	}
	mod, err := ingest.New(d)
	if err != nil {
		return err
	}
	w.store, w.mod, w.svc = store, mod, mod.Service
	return nil
}

// tasks: ingest.fetch_due (every minute, low), ingest.purge_expired (03:40,
// low; the imported_event retention, operational) and
// ingest.pause_claimed_sources (club_claim.decided).
func (w *ingestWiring) tasks(c *container) {
	w.mod.Subscribe(c.infra.subs)
	for _, j := range w.mod.Jobs() {
		c.infra.jobs.Schedule(j)
	}
}

// routes: the 17 ingest operations (04 §2.25 doc patch); w.svc is nil under
// `koro openapi`, which Register tolerates.
func (w *ingestWiring) routes(c *container, api huma.API) { ingest.Register(api, w.svc) }

// metrics registers the ingest counters on the worker's registry (the runs
// happen there; BE-210): koro_ingest_runs_total{outcome},
// koro_ingest_tokens_total{kind}, koro_ingest_items_total{kind} and
// koro_ingest_alarms_total{reason}. nil m → metrics off.
func (w *ingestWiring) metrics(m *obs.Metrics) error {
	for _, metric := range []struct{ name, label, help string }{
		{"runs", "outcome", "Ingest source runs by outcome."},
		{"tokens", "kind", "Extractor tokens by kind (input, output): the language model's cost."},
		{"items", "kind", "Ingest items by kind (event_created, event_rejected, club_created, club_matched, removal_request)."},
		{"alarms", "reason", "Ingest alarms by reason (zero_yield: a source that yielded comes back empty; paused_failures)."},
	} {
		name := metric.name
		if err := m.CounterFunc("koro_ingest_"+name+"_total", metric.help, []string{metric.label}, func() []obs.Sample {
			var out []obs.Sample
			for _, c := range w.mod.Counters() {
				if c.Name == name {
					out = append(out, obs.Sample{Labels: []string{c.Label}, Value: float64(c.Value)})
				}
			}
			return out
		}); err != nil {
			return err
		}
	}
	return nil
}
