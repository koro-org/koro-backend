// Package talent is the talent point module (01 §4.11; ADR-0015; BE-047):
// the signal catalogue, the append-only point ledger with its caps and
// dedupe key, the consumers of the activity events, the neutralisations
// (opt-out, KVKK objection, auditor decision) and the band that discovery
// multiplies its relevance with (BE-047); the nightly score with its
// cohorts (talent.recompute) and the talent_auditor endpoints (04 §T,
// BE-052). The retention sweeper (talent_ledger, talent_counters,
// talent_runs, talent_neutralization) is a later ticket: its policies are
// "todo" in cmd/koro/testdata/kvkk/retention_policies.txt.
//
// Rules the package enforces:
//
//   - Nobody sees a talent value (toplantı kararı #3, 03 §0, §3.4 Y1): no
//     DTO, log, event or API response carries one. Band panics in every
//     encoder and prints redacted; the huma schema guard rejects talent*,
//     band, score* and point* members outside the talent operations
//     (internal/platform/httpx/talentguard.go, whose only exemption is the
//     seven talent_auditor operations of 04 §T); only discovery and kvkk
//     import the package (internal/arch). The deliberate exceptions are the
//     talent_auditor endpoints (Admin) and the four-eyes approved KVKK
//     profiling_access export (Auditor.SubjectReport).
//   - The talent_auditor endpoints are closed while
//     talent.auditor_api_enabled is false: 404 for every signed-in user,
//     before any authorisation (03 §5.12 k-flag). Open, only the
//     talent_auditor role (MFA, at most two holders: identity) passes
//     authz (03 Y1: never platform_admin); every call carries
//     X-Koro-Access-Reason (10–500 characters) and is audited under its
//     action with that reason; the R operations check the proof after
//     authz and before any business data, a refused proof is the
//     operation's denied record (audit.ReauthFailed).
//   - The nightly score (talent.recompute, 03:00) is off with collection
//     (T-32). It decays every ledger row with its signal's half-life,
//     sums by category (floored at 0, capped by the config version),
//     ranks the users within their cohort (university + class year, the
//     university below cohort_min, then global), shrinks the percentile
//     towards the median with few signals and lifts only upwards: no data,
//     no points or a low percentile is band 0, never a penalty; a
//     neutralised user is band 0 and outside every cohort ranking.
//   - The talent schema is reachable only as app_talent: every store
//     statement runs in a transaction the service opened and switched with
//     SET LOCAL ROLE app_talent (Store.Enter); app_rw gets 42501. A talent
//     transaction never joins another module's transaction (the role would
//     leak into it), and the public reads (students, directory) happen
//     before it opens, because app_talent reads no public table. The audit
//     row of a neutralisation or a report is written in the same
//     transaction on the session role (Store.AsApp).
//   - Collection is off until the legal basis is decided (T-32):
//     talent.default_enabled (TALENT_DEFAULT_ENABLED) is false by default
//     and then every signal consumer is a no-op counted in
//     koro_talent_ingest_total{outcome="flag_off"}; Bands answers 0 for
//     everyone. The neutralisation consumers (opt-out, objection) always
//     apply: they restrict the processing and must hold the moment the
//     flag turns on.
//   - No signal from a minor (T-22, T-23: an 18-year-old starts from zero,
//     nothing is back-filled), from a user without a student profile, from
//     a student who turned activity based ranking off or objected, from a
//     sensitive event (the event's flag) or a sensitive club (membership,
//     a post of the club; directory.SensitiveOrgIDs). Sensitivity always
//     excludes, whatever signal_defs.excluded_when_sensitive says (special
//     category data, KVKK md. 6).
//   - Idempotent consumers (01 §6.1): one ledger row per (user, signal,
//     source) (point_events_dedupe_uq), one reversal per row. A
//     cancellation, a void, an unlike or a membership end writes a
//     reversal (points negated, occurred_at of the original, its config
//     version). Caps: daily and weekly magnitudes of signal_defs through
//     talent.daily_counters (the row lock serialises one user's signal),
//     "at most 2 clubs" and the self check-in as per_source (recorded with
//     0 points). Every row is frozen with the config version in force;
//     without one nothing is written (outcome no_config).
//   - An objection neutralisation is permanent: no staff member lifts it
//     (Neutralization.Liftable); an opt-out follows the student's own
//     privacy setting only.
//   - Personal data: logs carry IDs only; the KVKK provider erases every
//     talent row of the user (the ledger through
//     talent.point_events_erase_user) and its export states only whether
//     profiling data is held: the content goes out through the four-eyes
//     profiling_access report (03 §5.12).
package talent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/privacy"
)

// Ranking is the band read of discovery (01 §4.11), its only consumer.
type Ranking interface {
	// Bands returns the band of every user among userIDs whose band is
	// above neutral; a missing user is BandNeutral (no score, neutralised,
	// collection off). It must not be called inside a transaction.
	Bands(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]Band, error)
}

// Auditor is the KVKK surface of talent (kvkk through hooks.talentAuditor;
// 01 §4.11). The talent_auditor's endpoints are Admin. No method may be
// called inside a transaction.
type Auditor interface {
	// SubjectReport is the report of a four-eyes approved profiling_access
	// request (kvkk, 03 §5.12), as JSON for the export file. kvkk authorised
	// the request; the read is audited as talent.report.read on behalf of
	// requestedBy (its first approver; uuid.Nil → the system job).
	SubjectReport(ctx context.Context, requestedBy, userID uuid.UUID, reason string) (json.RawMessage, error)
	// Neutralize applies a neutralisation (system call; idempotent: an
	// active neutralisation of the same reason is kept). An objection needs
	// its data subject request.
	Neutralize(ctx context.Context, userID uuid.UUID, reason NeutralizeReason, dsrID *uuid.UUID) error
}

// Deps are the dependencies of the talent module; all but Reauth and the
// two flags are required (New returns an error otherwise).
type Deps struct {
	Store Store
	Tx    db.TxManager
	Audit AuditLogger // neutralisations and reports, same transaction (Store.AsApp)
	Clock clock.Clock
	Log   *slog.Logger

	Students  Students  // cohort (minor), ranking choice
	Directory Directory // sensitive clubs

	// Reauth verifies the proof of the R operations (identity). nil →
	// every R operation fails closed (503).
	Reauth Reauthenticator

	// CollectionEnabled is talent.default_enabled (TALENT_DEFAULT_ENABLED,
	// T-32): false → no ledger row is written, the nightly score does not
	// run and every band is neutral.
	CollectionEnabled bool
	// AuditorAPIEnabled is talent.auditor_api_enabled
	// (TALENT_AUDITOR_API_ENABLED, 03 §5.12 k-flag): false → every
	// talent_auditor endpoint answers 404 to everyone.
	AuditorAPIEnabled bool
}

func (d Deps) validate() error {
	var errs []error
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"Store", d.Store != nil}, {"Tx", d.Tx != nil}, {"Audit", d.Audit != nil}, {"Clock", d.Clock != nil},
		{"Log", d.Log != nil}, {"Students", d.Students != nil}, {"Directory", d.Directory != nil},
	} {
		if !c.ok {
			errs = append(errs, fmt.Errorf("talent.Deps.%s is missing", c.name))
		}
	}
	return errors.Join(errs...)
}

// Module is what talent hands to the composition root.
type Module struct {
	Ranking Ranking // discovery (BE-051)
	Auditor Auditor // kvkk hook (profiling_access report, neutralisation)
	Admin   Admin   // the talent_auditor endpoints (04 §T, BE-052)
	// Privacy is the KVKK provider (ledger, counters, neutralisations,
	// cohort, score).
	Privacy privacy.Provider

	svc *service
}

// New builds the talent module; a missing dependency is a wiring bug that
// stops the process at start.
func New(d Deps) (Module, error) {
	if err := d.validate(); err != nil {
		return Module{}, err
	}
	s := &service{d: d, counts: newCounters()}
	return Module{Ranking: s, Auditor: s, Admin: s, Privacy: privacyProvider{s: s}, svc: s}, nil
}

// CollectionEnabled reports talent.default_enabled.
func (m Module) CollectionEnabled() bool { return m.svc.d.CollectionEnabled }

// AuditorAPIEnabled reports talent.auditor_api_enabled.
func (m Module) AuditorAPIEnabled() bool { return m.svc.d.AuditorAPIEnabled }

type service struct {
	d      Deps
	counts *counters
}

var (
	_ Ranking = (*service)(nil)
	_ Auditor = (*service)(nil)
	_ Admin   = (*service)(nil)
)

// now is the service clock in the database's precision (microseconds).
func (s *service) now() time.Time { return s.d.Clock.Now().UTC().Truncate(time.Microsecond) }

// inTx runs fn in a talent transaction: a new transaction switched to
// app_talent. It refuses to run inside another transaction (errForeignTx).
func (s *service) inTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if db.InTx(ctx) {
		return errForeignTx
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		ctx, err := s.d.Store.Enter(ctx)
		if err != nil {
			return err
		}
		return fn(ctx)
	})
}

// system authorises a ledger job of talent (03 §3.7: talent.ledger.write).
func system(ctx context.Context, job string) error {
	return systemAction(ctx, job, authz.TalentLedgerWrite)
}

// systemAction authorises system job job on act (03 §3.7).
func systemAction(ctx context.Context, job string, act authz.Action) error {
	if err := authz.Can(ctx, authz.SystemActor(job), act, authz.Resource{}); err != nil {
		return fmt.Errorf("talent: %s: %w", job, err)
	}
	return nil
}
