// Package ingest collects the clubs and events of the universities' and
// the clubs' own public pages (BE-210; user decision 2026-11-11, koro-docs#11;
// docs/research/kulup-etkinlik-toplama-2026-11-11.md, Aşama 1): first each
// pilot university's official club list (unclaimed club profiles), then
// each club's own sources one by one (its site, its events page) and the
// universities' event pages. It sits in L13 (01 §5.1): it reads directory
// and makes events drafts; nothing imports it.
//
// The pipeline (pipeline.go), per due source: robots.txt (a refusal stops
// and pauses the source), a conditional GET through the SSRF-guarded
// fetcher (fetch.go), the adapter of the source's type (adapter.go), and
//
//   - for a club list: each entry matched or opened as an unclaimed club
//     (directory.ClubImports, the special categories never automatic), its
//     imported_clubs row, its own site as a new events source; profile links
//     to Instagram, X, LinkedIn … are kept as links and never fetched;
//   - for an events source: the page text masked (mask.go: e-mail, phone,
//     the person-name heuristic), sent to the Extractor (Claude, structured
//     output, no tool; or the fake), every candidate validated in code
//     (extract.go: a verbatim evidence quote, a date in [yesterday, +180
//     days], the year rule), normalised and deduplicated by fingerprint
//     (club or university + day + title) into imported_events.
//
// An unchanged body (same SHA-256) is never sent to the extractor.
//
// Rules the package enforces:
//
//   - Only facts (title, time, place text, category, source link) and the
//     evidence quote are kept; no person, no contact, no image, no page
//     text (only its hash). An imported event is shown as "Otomatik
//     toplandı · kaynak" with its source_url, takes no registration, earns
//     no talent point and is no recommendation or audience signal: it lives
//     in its own table and its own operations. It is deleted at expires_at
//     (its end + the imported_event retention) by ingest.purge_expired.
//   - Anyone may ask, without an account, that an imported event be
//     removed: it is hidden at once and waits in the admins' list.
//   - When a club's claim is approved, its sources stop (paused_reason
//     claimed), and its admin is told that its imported events wait as
//     suggestions: promoting one makes an ordinary draft event with the
//     ordinary authorisation (events.event.create), rejecting one hides it.
//   - Sources start disabled unless the environment's default enables
//     them (INGEST_SOURCE_DEFAULT: dev/staging on, prod off until the
//     lawyer's review); a source pauses itself after INGEST_FAIL_LIMIT
//     failures in a row (alarm log), and a source that used to yield and
//     yields nothing INGEST_ZERO_YIELD_ALARM runs in a row raises an alarm
//     ("getiri sıfırlandı": the site changed).
//   - Politeness: robots.txt, one fetch per host per tick, at least
//     INGEST_HOST_GAP (and the host's Crawl-delay) between two fetches of a
//     host, the KoroBot/1.0 User-Agent with the bot page URL.
//   - Authorisation (03 §5.25 doc patch): the public reads and the removal
//     request are P; the suggestions are the club_admin's (org surface);
//     everything else is the platform admin's (admin surface).
package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/events"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/idempotency"
)

// Service is the ingest service: the HTTP use cases (every one calls
// authz.Can first, 05 §5.3) and the worker's runs.
type Service interface {
	// ListImportedEvents is listImportedEvents (ingest.imported_event.list, P).
	ListImportedEvents(ctx context.Context, a authz.Actor, q EventQuery) (EventPage, error)
	// GetImportedEvent is getImportedEvent (ingest.imported_event.read, P).
	GetImportedEvent(ctx context.Context, a authz.Actor, id uuid.UUID) (EventView, error)
	// RequestRemoval is requestImportedEventRemoval
	// (ingest.imported_event.request_removal, P): the event is hidden.
	RequestRemoval(ctx context.Context, a authz.Actor, id uuid.UUID, reason RemovalReason) error

	// ListSuggestions is listClubImportedEvents (ingest.suggestion.list).
	ListSuggestions(ctx context.Context, a authz.Actor, orgID uuid.UUID, q EventQuery) (EventPage, error)
	// PromoteSuggestion is promoteImportedEvent (ingest.suggestion.promote):
	// an ordinary draft event of the club.
	PromoteSuggestion(ctx context.Context, a authz.Actor, orgID, id uuid.UUID) (events.EventView, error)
	// RejectSuggestion is rejectImportedEvent (ingest.suggestion.reject).
	RejectSuggestion(ctx context.Context, a authz.Actor, orgID, id uuid.UUID) error

	// AdminListSources is adminListIngestSources (ingest.source.list).
	AdminListSources(ctx context.Context, a authz.Actor, q SourceQuery) (SourcePage, error)
	// AdminGetSource is adminGetIngestSource (ingest.source.read): the source
	// with its latest runs.
	AdminGetSource(ctx context.Context, a authz.Actor, id uuid.UUID) (SourceView, error)
	// AdminCreateSource is adminCreateIngestSource (ingest.source.create).
	AdminCreateSource(ctx context.Context, a authz.Actor, in SourceInput) (SourceView, error)
	// AdminAddPilotSources is adminAddPilotIngestSources (ingest.source.create):
	// the built-in sources of a pilot university.
	AdminAddPilotSources(ctx context.Context, a authz.Actor, in PilotInput) (PilotResult, error)
	// AdminUpdateSource is adminUpdateIngestSource (ingest.source.update).
	AdminUpdateSource(ctx context.Context, a authz.Actor, id uuid.UUID, p SourcePatch) (SourceView, error)
	// AdminRunSource is adminRunIngestSource (ingest.source.run): due now.
	AdminRunSource(ctx context.Context, a authz.Actor, id uuid.UUID) (SourceView, error)
	// AdminListImportedEvents is adminListImportedEvents
	// (ingest.imported_event.list_admin).
	AdminListImportedEvents(ctx context.Context, a authz.Actor, q AdminEventQuery) (EventPage, error)
	// AdminSetEventStatus is adminSetImportedEventStatus
	// (ingest.imported_event.moderate): hidden or imported.
	AdminSetEventStatus(ctx context.Context, a authz.Actor, id uuid.UUID, st Status) (EventView, error)
	// AdminDeleteImportedEvent is adminDeleteImportedEvent
	// (ingest.imported_event.delete).
	AdminDeleteImportedEvent(ctx context.Context, a authz.Actor, id uuid.UUID) error
	// AdminListImportedClubs is adminListImportedClubs (ingest.imported_club.list).
	AdminListImportedClubs(ctx context.Context, a authz.Actor, q ClubQuery) (ClubPage, error)
	// AdminMarkClubReviewed is adminMarkImportedClubReviewed
	// (ingest.imported_club.review): the category was checked.
	AdminMarkClubReviewed(ctx context.Context, a authz.Actor, clubOrgID uuid.UUID) error

	// RunDue runs the due sources within the tick budget (ingest.fetch_due).
	RunDue(ctx context.Context) (TickResult, error)
}

// Config is the module's settings (config.Ingest, read by cmd/koro).
type Config struct {
	// FetchEnabled is the global switch of the fetch job (INGEST_FETCH_ENABLED).
	FetchEnabled bool
	// SourceDefaultEnabled: a new source starts enabled (dev/staging) or
	// disabled (prod until the lawyer's review).
	SourceDefaultEnabled bool
	ListInterval         time.Duration // club lists (default 24h)
	EventsInterval       time.Duration // events sources (default 6h)
	HostGap              time.Duration // ≥ 10s between two fetches of a host
	TickBudget           time.Duration // time one fetch_due run may spend
	TickBatch            int           // due sources read per tick
	FailLimit            int           // failures in a row that pause a source
	ZeroYieldAlarm       int           // empty runs in a row that raise the alarm
	MaxTextChars         int           // masked text sent per extraction
	MinConfidence        float64       // candidates below are dropped
	EventRetention       time.Duration // default of the imported_event policy
	RunRetention         time.Duration // ingest_runs kept (operational)
	ChildRetention       time.Duration // one-off sitemap pages kept after their fetch
}

func (c Config) withDefaults() Config {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&c.ListInterval, 24*time.Hour)
	def(&c.EventsInterval, 6*time.Hour)
	def(&c.HostGap, 10*time.Second)
	def(&c.TickBudget, 45*time.Second)
	def(&c.EventRetention, 7*24*time.Hour)
	def(&c.RunRetention, 90*24*time.Hour)
	def(&c.ChildRetention, 30*24*time.Hour)
	if c.HostGap < 10*time.Second {
		c.HostGap = 10 * time.Second
	}
	if c.TickBatch <= 0 {
		c.TickBatch = 50
	}
	if c.FailLimit <= 0 {
		c.FailLimit = 5
	}
	if c.ZeroYieldAlarm <= 0 {
		c.ZeroYieldAlarm = 3
	}
	if c.MaxTextChars <= 0 {
		c.MaxTextChars = 30000
	}
	if c.MinConfidence <= 0 {
		c.MinConfidence = 0.6
	}
	return c
}

// Deps are the dependencies of the ingest module. Store, Tx, Directory,
// Orgs, Fetcher, Extractor, Audit, Clock and Log are required.
type Deps struct {
	Store     Store
	Tx        db.TxManager
	Directory Directory
	Orgs      Orgs
	Fetcher   Fetcher
	Extractor Extractor
	Audit     AuditLogger
	Clock     clock.Clock
	Log       *slog.Logger
	Config    Config
	// Idempotency stores the optional Idempotency-Key of the POST
	// operations (06 H-5). nil → the key is ignored.
	Idempotency *idempotency.Keeper
	// Drafts makes promoted suggestions draft events (events). nil →
	// promoteImportedEvent answers 503.
	Drafts Drafts
	// Sender tells a club's new admin that suggestions wait. nil → the
	// notice task retries.
	Sender Sender
	// EventRetention reads the imported_event policy at run time
	// (hooks.retention); nil → Config.EventRetention.
	EventRetention func(ctx context.Context) (time.Duration, error)
	// Adapters replaces the built-in adapters (tests). nil → defaultAdapters.
	Adapters map[AdapterName]SourceAdapter
}

func (d Deps) validate() error {
	var errs []error
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"Store", d.Store != nil}, {"Tx", d.Tx != nil}, {"Directory", d.Directory != nil}, {"Orgs", d.Orgs != nil},
		{"Fetcher", d.Fetcher != nil},
		{"Extractor", d.Extractor != nil}, {"Audit", d.Audit != nil}, {"Clock", d.Clock != nil}, {"Log", d.Log != nil},
	} {
		if !c.ok {
			errs = append(errs, fmt.Errorf("ingest.Deps.%s is missing", c.name))
		}
	}
	return errors.Join(errs...)
}

// Module is what ingest hands to the composition root.
type Module struct {
	Service Service
	svc     *service
}

// New builds the ingest module; a missing dependency is a wiring bug that
// stops the process at start.
func New(d Deps) (Module, error) {
	if err := d.validate(); err != nil {
		return Module{}, err
	}
	d.Config = d.Config.withDefaults()
	if d.Adapters == nil {
		d.Adapters = defaultAdapters()
	}
	loc, err := time.LoadLocation("Europe/Istanbul")
	if err != nil {
		return Module{}, fmt.Errorf("ingest: Europe/Istanbul: %w", err)
	}
	s := &service{d: d, loc: loc}
	return Module{Service: s, svc: s}, nil
}

type service struct {
	d   Deps
	loc *time.Location
	m   counters
}

var _ Service = (*service)(nil)

// now is the service clock in the database's precision (microseconds).
func (s *service) now() time.Time { return s.d.Clock.Now().UTC().Truncate(time.Microsecond) }

// counters are the module's metrics (monotonic, read by cmd/koro).
type counters struct {
	runs                                   [len(allOutcomes)]atomic.Int64
	inputTokens, outputTokens              atomic.Int64
	created, rejected                      atomic.Int64
	zeroYieldAlarms, pausedByFailures      atomic.Int64
	clubsCreated, clubsMatched, removalReq atomic.Int64
}

// allOutcomes fixes the metric label order.
var allOutcomes = [...]Outcome{OutcomeOK, OutcomeUnchanged, OutcomeNotModified, OutcomeRobotsDenied, OutcomeBlocked,
	OutcomeHTTPError, OutcomeFetchError, OutcomeParseError, OutcomeExtractError, OutcomeUnsupported}

// Counter is one metric sample of Module.Counters.
type Counter struct {
	Name  string // koro_ingest_<name>_total
	Label string // outcome / kind label ("" none)
	Value int64
}

// Counters returns the module's counters (monotonic) for
// koro_ingest_runs_total{outcome}, koro_ingest_tokens_total{kind},
// koro_ingest_items_total{kind} and koro_ingest_alarms_total{reason}.
func (m Module) Counters() []Counter {
	if m.svc == nil {
		return nil
	}
	c := &m.svc.m
	var out []Counter
	for i, o := range allOutcomes {
		out = append(out, Counter{"runs", string(o), c.runs[i].Load()})
	}
	return append(out,
		Counter{"tokens", "input", c.inputTokens.Load()}, Counter{"tokens", "output", c.outputTokens.Load()},
		Counter{"items", "event_created", c.created.Load()}, Counter{"items", "event_rejected", c.rejected.Load()},
		Counter{"items", "club_created", c.clubsCreated.Load()}, Counter{"items", "club_matched", c.clubsMatched.Load()},
		Counter{"items", "removal_request", c.removalReq.Load()},
		Counter{"alarms", "zero_yield", c.zeroYieldAlarms.Load()}, Counter{"alarms", "paused_failures", c.pausedByFailures.Load()},
	)
}

func (c *counters) run(o Outcome) {
	for i, x := range allOutcomes {
		if x == o {
			c.runs[i].Add(1)
		}
	}
}
