package talent_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/civil"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/obs"
	"koro/internal/platform/privacy"
	"koro/internal/students"
	"koro/internal/talent"
)

// Retention (BE-079; 02 §15, §24.2): the talent sweeper over the memory
// store; retention_integration_test.go runs the same world on PostgreSQL.

// talentPeriods are the seeded periods (000090): years, months, days.
var talentPeriods = map[string][3]int{
	talent.PolicyLedger:         {0, 24, 0},
	talent.PolicyCounters:       {0, 0, 14},
	talent.PolicyRuns:           {1, 0, 0},
	talent.PolicyNeutralization: {3, 0, 0},
}

// sweepOrder is kvkk.SweepOrder of the four codes (talent_ledger after
// talent_runs).
var sweepOrder = []string{talent.PolicyCounters, talent.PolicyNeutralization, talent.PolicyRuns, talent.PolicyLedger}

// retentionPolicy is the policy as kvkk.retention_sweep hands it over at
// now (Period.Duration: calendar exact at now).
func retentionPolicy(code string, now time.Time, approved bool) privacy.RetentionPolicy {
	p := talentPeriods[code]
	return privacy.RetentionPolicy{Code: code, OwnerModule: "talent", Retention: now.Sub(now.AddDate(-p[0], -p[1], -p[2])),
		Anchor: "created_at", Method: privacy.MethodHardDelete, LegalApproved: approved, Enabled: true}
}

// errRollback ends kvkk's dry-run transaction.
var errRollback = errors.New("rollback")

// retentionWorld is the module under test with its store, seen the same
// way on both stores.
type retentionWorld struct {
	mod   talent.Module
	store talent.Store
	tx    db.TxManager
	now   time.Time
	cfg   talent.ConfigVersion // the version in force
	user  func(t *testing.T) uuid.UUID
	// point appends a ledger row created at created (the database stamps
	// created_at, so each store backdates it its own way).
	point func(t *testing.T, p talent.PointEvent, created time.Time)
	// rows counts the rows of a talent table.
	rows func(t *testing.T, table string) int
	// role is current_user inside ctx's transaction ("" without a database).
	role func(t *testing.T, ctx context.Context) string
	// approve marks the talent policies legally approved where the store
	// keeps them (PostgreSQL's retention_policies; nil: nothing to mark).
	approve func(t *testing.T)
}

// memWorld is the retention world over the memory store (collection off).
func memWorld(t *testing.T) retentionWorld {
	t.Helper()
	e := newEnv(t, false)
	return retentionWorld{mod: e.mod, store: e.store, tx: db.NopTx{}, now: t0, cfg: e.cfg,
		user: func(*testing.T) uuid.UUID { return uuid.NewV7() },
		point: func(t *testing.T, p talent.PointEvent, created time.Time) {
			t.Helper()
			e.clk.Set(created)
			defer e.clk.Set(t0)
			e.inTalentTx(t, func(ctx context.Context) error {
				_, ok, err := e.store.InsertPoint(ctx, p)
				if err == nil && !ok {
					err = errors.New("duplicate ledger row")
				}
				return err
			})
		},
		rows: func(t *testing.T, table string) int {
			t.Helper()
			switch table {
			case "point_events":
				return len(e.store.AllPoints())
			case "daily_counters":
				return e.store.Counters()
			case "score_runs":
				return len(e.store.Runs())
			case "neutralizations":
				return len(e.store.AllNeutralizations())
			case "config_versions":
				return len(e.store.Configs())
			}
			t.Fatalf("no table %s", table)
			return 0
		},
		role: func(*testing.T, context.Context) string { return "" }}
}

// retentionFixture is what the world holds before the first sweep, for
// every policy: rows past their period and rows inside it.
type retentionFixture struct {
	before map[string]int // rows per table
}

// seedRetention writes the fixture:
//
//	ledger            a (3 years) and its reversal (26 months): go;
//	                  b (3 years) with a reversal of 1 year: both stay (a
//	                  younger reversal keeps its original); c (1 year): stays
//	config versions   old (4 years), superseded by mid (3 years) and only
//	                  used by a: goes once a is gone; mid (b) and the
//	                  current version stay
//	daily counters    20 days ago: goes; exactly 14 days and 2 days ago: stay
//	score runs        1 year and 10 days ago: goes; 6 months ago: stays
//	neutralisations   an opt-out lifted 3 years and 10 days ago: goes; an
//	                  auditor's lifted a year ago and an active objection:
//	                  stay
func seedRetention(t *testing.T, w retentionWorld) retentionFixture {
	t.Helper()
	ce := contractEnv{store: w.store, tx: w.tx}
	creator, student := w.user(t), w.user(t)
	var cfgOld, cfgMid talent.ConfigVersion
	ce.must(t, func(ctx context.Context) error {
		var err error
		if cfgOld, err = w.store.InsertConfigVersion(ctx, talent.ConfigVersion{ID: uuid.NewV7(), EffectiveFrom: w.now.AddDate(-4, 0, 0),
			CreatedBy: creator}); err != nil {
			return err
		}
		cfgMid, err = w.store.InsertConfigVersion(ctx, talent.ConfigVersion{ID: uuid.NewV7(), EffectiveFrom: w.now.AddDate(-3, 0, 0),
			CreatedBy: creator})
		return err
	})
	row := func(cfg talent.ConfigVersion, created time.Time, of *talent.PointEvent) talent.PointEvent {
		p := talent.PointEvent{ID: uuid.NewV7(), UserID: student, Signal: talent.SignalFeedLike, SourceType: talent.SourcePost,
			SourceID: uuid.NewV7(), OccurredAt: created, RawPointsCP: 10, PointsCP: 10, ReasonCode: talent.SignalFeedLike,
			ConfigVersionID: cfg.ID, SourceEventID: uuid.NewV7()}
		if of != nil {
			p.SourceType, p.SourceID, p.ReversalOf, p.OccurredAt = talent.SourceReversal, of.ID, &of.ID, of.OccurredAt
			p.RawPointsCP, p.PointsCP = -of.PointsCP, -of.PointsCP
		}
		w.point(t, p, created)
		return p
	}
	a := row(cfgOld, w.now.AddDate(-3, 0, 0), nil)
	row(cfgOld, w.now.AddDate(0, -26, 0), &a)
	b := row(cfgMid, w.now.AddDate(-3, 0, 0), nil)
	row(cfgMid, w.now.AddDate(-1, 0, 0), &b)
	row(w.cfg, w.now.AddDate(-1, 0, 0), nil)

	today := civil.Today(w.now)
	auditor := w.user(t)
	ce.must(t, func(ctx context.Context) error {
		for _, ago := range []int{20, 14, 2} {
			if _, err := w.store.LockCounter(ctx, student, today.AddDays(-ago), talent.SignalFeedLike); err != nil {
				return err
			}
			if err := w.store.AddCounter(ctx, student, today.AddDays(-ago), talent.SignalFeedLike, 10); err != nil {
				return err
			}
		}
		for _, started := range []time.Time{w.now.AddDate(-1, 0, -10), w.now.AddDate(0, -6, 0)} {
			if err := w.store.InsertRun(ctx, talent.ScoreRun{ID: uuid.NewV7(), ConfigVersionID: w.cfg.ID, StartedAt: started}); err != nil {
				return err
			}
		}
		for _, n := range []struct {
			reason talent.NeutralizeReason
			lifted time.Time
		}{{talent.NeutralizeOptOut, w.now.AddDate(-3, 0, -10)}, {talent.NeutralizeAdmin, w.now.AddDate(-1, 0, 0)}} {
			row, _, err := w.store.InsertNeutralization(ctx, talent.Neutralization{ID: uuid.NewV7(), UserID: student, Reason: n.reason,
				CreatedBy: &auditor})
			if err != nil {
				return err
			}
			if _, ok, err := w.store.LiftNeutralization(ctx, row.ID, &auditor, n.lifted); err != nil || !ok {
				return errors.Join(err, errors.New("neutralisation not lifted"))
			}
		}
		dsr := uuid.NewV7()
		_, _, err := w.store.InsertNeutralization(ctx, talent.Neutralization{ID: uuid.NewV7(), UserID: student,
			Reason: talent.NeutralizeObjection, DSRID: &dsr})
		return err
	})
	f := retentionFixture{before: map[string]int{}}
	for _, table := range []string{"point_events", "config_versions", "daily_counters", "score_runs", "neutralizations"} {
		f.before[table] = w.rows(t, table)
	}
	return f
}

// sweepAll runs the four policies in kvkk's order and returns the counts.
func (w retentionWorld) sweepAll(t *testing.T, approved bool) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, code := range sweepOrder {
		res, err := w.mod.Sweeper.Sweep(t.Context(), retentionPolicy(code, w.now, approved), w.now)
		if err != nil {
			t.Fatalf("%s: %v", code, err)
		}
		if res.PolicyCode != code || res.Method != privacy.MethodHardDelete || res.Table == "" {
			t.Errorf("%s: result %+v", code, res)
		}
		out[code] = res.Count
	}
	return out
}

// dryRunInKvkkTx runs every policy unapproved the way kvkk.retention_sweep
// does: inside a transaction that is always rolled back. The transaction
// is back on the session role after each sweep.
func (w retentionWorld) dryRunInKvkkTx(t *testing.T, appRole string) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, code := range sweepOrder {
		err := w.tx.WithTx(t.Context(), func(ctx context.Context) error {
			res, err := w.mod.Sweeper.Sweep(ctx, retentionPolicy(code, w.now, false), w.now)
			if err != nil {
				return err
			}
			out[code] = res.Count
			if got := w.role(t, ctx); got != appRole {
				t.Errorf("%s: the dry run left the transaction on %q, want %q", code, got, appRole)
			}
			return errRollback
		})
		if !errors.Is(err, errRollback) {
			t.Fatalf("%s: dry run: %v", code, err)
		}
	}
	return out
}

// checkRetention runs the scenario of the acceptance criteria on w: the
// dry run counts and destroys nothing; the approved sweep deletes exactly
// the rows past their period (with collection off); a second sweep
// changes nothing.
func checkRetention(t *testing.T, w retentionWorld, appRole string) {
	t.Helper()
	f := seedRetention(t, w)
	rows := func() map[string]int {
		out := map[string]int{}
		for table := range f.before {
			out[table] = w.rows(t, table)
		}
		return out
	}
	if got, want := w.dryRunInKvkkTx(t, appRole), map[string]int{talent.PolicyLedger: 2, talent.PolicyCounters: 1,
		talent.PolicyRuns: 1, talent.PolicyNeutralization: 1}; !mapsEqual(got, want) {
		t.Errorf("dry run counts = %v, want %v (config version still referenced)", got, want)
	}
	if got := rows(); !mapsEqual(got, f.before) {
		t.Fatalf("the dry run destroyed rows: %v, before %v", got, f.before)
	}

	if w.approve != nil {
		w.approve(t)
	}
	if got, want := w.sweepAll(t, true), map[string]int{talent.PolicyLedger: 3, talent.PolicyCounters: 1, talent.PolicyRuns: 1,
		talent.PolicyNeutralization: 1}; !mapsEqual(got, want) {
		t.Errorf("approved sweep counts = %v, want %v", got, want)
	}
	want := map[string]int{"point_events": f.before["point_events"] - 2, "config_versions": f.before["config_versions"] - 1,
		"daily_counters": f.before["daily_counters"] - 1, "score_runs": f.before["score_runs"] - 1,
		"neutralizations": f.before["neutralizations"] - 1}
	if got := rows(); !mapsEqual(got, want) {
		t.Fatalf("after the sweep rows = %v, want %v", got, want)
	}

	zero := map[string]int{talent.PolicyLedger: 0, talent.PolicyCounters: 0, talent.PolicyRuns: 0, talent.PolicyNeutralization: 0}
	if got := w.sweepAll(t, true); !mapsEqual(got, zero) {
		t.Errorf("second sweep counts = %v, want nothing", got)
	}
	if got := rows(); !mapsEqual(got, want) {
		t.Fatalf("the second sweep changed rows: %v, want %v", got, want)
	}
}

func mapsEqual[K comparable](a, b map[K]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// TestSweeper_PolicyCodes (BE-079): the sweeper claims the four talent
// policies of the seed and refuses any other code.
func TestSweeper_PolicyCodes(t *testing.T) {
	t.Parallel()
	w := memWorld(t)
	got := slices.Sorted(slices.Values(w.mod.Sweeper.PolicyCodes()))
	if want := []string{"talent_counters", "talent_ledger", "talent_neutralization", "talent_runs"}; !slices.Equal(got, want) {
		t.Fatalf("PolicyCodes = %v, want %v", got, want)
	}
	if _, err := w.mod.Sweeper.Sweep(t.Context(), privacy.RetentionPolicy{Code: "post_removed", LegalApproved: true}, w.now); err == nil {
		t.Fatal("a policy of another module was swept")
	}
}

// TestSweeper_TalentRetention_Mem (BE-079): the acceptance scenario on the
// memory store, collection off (talent.default_enabled = false).
func TestSweeper_TalentRetention_Mem(t *testing.T) {
	t.Parallel()
	checkRetention(t, memWorld(t), "")
}

// TestSweeper_ApprovedNeverJoinsATransaction: an approved sweep deletes in
// talent transactions of its own; called inside another module's
// transaction it refuses (app_talent would leak into it) and deletes
// nothing.
func TestSweeper_ApprovedNeverJoinsATransaction(t *testing.T) {
	t.Parallel()
	w := memWorld(t)
	f := seedRetention(t, w)
	err := db.NopTx{}.WithTx(t.Context(), func(ctx context.Context) error {
		_, err := w.mod.Sweeper.Sweep(ctx, retentionPolicy(talent.PolicyCounters, w.now, true), w.now)
		return err
	})
	if err == nil {
		t.Fatal("an approved sweep ran inside a foreign transaction")
	}
	if n := w.rows(t, "daily_counters"); n != f.before["daily_counters"] {
		t.Fatalf("daily counters = %d, want %d", n, f.before["daily_counters"])
	}
	// Outside a transaction the dry run opens its own.
	res, err := w.mod.Sweeper.Sweep(t.Context(), retentionPolicy(talent.PolicyCounters, w.now, false), w.now)
	if err != nil || res.Count != 1 {
		t.Fatalf("dry run outside a transaction = %+v, %v", res, err)
	}
}

// recordingStore records the size of every real purge batch.
type recordingStore struct {
	*talent.MemStore
	mu      sync.Mutex
	batches map[string][]int
}

func (r *recordingStore) record(op string, n int, dry bool) {
	if dry {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.batches[op] = append(r.batches[op], n)
}

func (r *recordingStore) PurgeLedger(ctx context.Context, cutoff time.Time, limit int, dry bool) (int, error) {
	n, err := r.MemStore.PurgeLedger(ctx, cutoff, limit, dry)
	r.record("ledger", n, dry)
	return n, err
}

func (r *recordingStore) PurgeCounters(ctx context.Context, before civil.Date, limit int, dry bool) (int, error) {
	n, err := r.MemStore.PurgeCounters(ctx, before, limit, dry)
	r.record("counters", n, dry)
	return n, err
}

// countTx counts the outermost transactions.
type countTx struct {
	db.NopTx
	mu sync.Mutex
	n  int
}

func (c *countTx) WithTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if !db.InTx(ctx) {
		c.mu.Lock()
		c.n++
		c.mu.Unlock()
	}
	return c.NopTx.WithTx(ctx, fn)
}

// TestSweeper_BatchesOf1000 (01 §7: ≤ 1000 rows per transaction): an
// approved sweep deletes in batches of at most 1000 rows, one transaction
// each. The ledger goes in phases (old reversals, then their originals), so
// a short batch does not end it: 1100 reversals and 1200 originals take
// 1000 + 100 + 1000 + 200 and a last empty batch; 2500 counters take
// 1000 + 1000 + 500.
func TestSweeper_BatchesOf1000(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(t0.AddDate(-3, 0, 0))
	rec := &recordingStore{MemStore: talent.NewMemStore(clk.Now), batches: map[string][]int{}}
	tx := &countTx{}
	mod, err := talent.New(talent.Deps{Store: rec, Tx: tx, Audit: &fakeAudit{}, Clock: clk, Log: obs.Discard(),
		Students:    &fakeStudents{cohorts: map[uuid.UUID]students.Cohort{}, choices: map[uuid.UUID]students.RankingChoice{}},
		Directory:   fakeDirectory{sensitive: map[uuid.UUID]bool{}},
		Accounts:    &fakeAccounts{created: map[uuid.UUID]time.Time{}},
		Events:      &fakeEvents{of: map[uuid.UUID]uuid.UUID{}, facts: map[uuid.UUID]talent.EventFacts{}},
		Memberships: &fakeMemberships{m: map[uuid.UUID][]authz.Membership{}},
		Likes:       &fakeLikes{withdrawn: map[talent.LikeRef]bool{}}, Destructions: &fakeDestructions{}})
	if err != nil {
		t.Fatal(err)
	}
	user, cfg := uuid.NewV7(), uuid.NewV7()
	err = db.NopTx{}.WithTx(t.Context(), func(ctx context.Context) error {
		ctx, err := rec.Enter(ctx)
		if err != nil {
			return err
		}
		day := civil.Today(t0).AddDays(-30)
		for i := range 2500 {
			if _, err := rec.LockCounter(ctx, uuid.NewV7(), day.AddDays(-i%10), talent.SignalFeedLike); err != nil {
				return err
			}
		}
		for i := range 1200 {
			p := talent.PointEvent{ID: uuid.NewV7(), UserID: user, Signal: talent.SignalFeedLike, SourceType: talent.SourcePost,
				SourceID: uuid.NewV7(), OccurredAt: clk.Now(), RawPointsCP: 10, PointsCP: 10, ReasonCode: talent.SignalFeedLike,
				ConfigVersionID: cfg, SourceEventID: uuid.NewV7()}
			if _, _, err := rec.InsertPoint(ctx, p); err != nil {
				return err
			}
			if i < 1100 {
				r := p
				r.ID, r.SourceType, r.SourceID, r.ReversalOf, r.PointsCP, r.RawPointsCP = uuid.NewV7(), talent.SourceReversal, p.ID, &p.ID, -10, -10
				if _, _, err := rec.InsertPoint(ctx, r); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	clk.Set(t0)
	for _, code := range []string{talent.PolicyLedger, talent.PolicyCounters} {
		before := tx.n
		res, err := mod.Sweeper.Sweep(t.Context(), retentionPolicy(code, t0, true), t0)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]int{talent.PolicyLedger: 2300, talent.PolicyCounters: 2500}[code]
		if res.Count != want {
			t.Errorf("%s: count %d, want %d", code, res.Count, want)
		}
		// The ledger's five batches and the config version purge's one.
		calls := map[string]int{talent.PolicyLedger: 6, talent.PolicyCounters: 3}[code]
		if got := tx.n - before; got != calls {
			t.Errorf("%s: %d transactions, want %d (one per batch)", code, got, calls)
		}
	}
	if got := rec.batches["ledger"]; !slices.Equal(got, []int{1000, 100, 1000, 200, 0}) {
		t.Errorf("ledger batches = %v", got)
	}
	if got := rec.batches["counters"]; !slices.Equal(got, []int{1000, 1000, 500}) {
		t.Errorf("counter batches = %v", got)
	}
	if n := len(rec.AllPoints()); n != 0 {
		t.Errorf("%d ledger rows left", n)
	}
}
