package talent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/queue"
	"koro/internal/students"
)

// RecomputeJobName is the nightly score (01 §7): 03:00 Europe/Istanbul on
// the low queue, system action talent.score.compute (03 §3.7).
const RecomputeJobName = "talent.recompute"

// recomputeBatch is the users per transaction and per students read
// (01 §7: ≤ 1000 rows per transaction).
const recomputeBatch = 1000

// maxRunError is talent.score_runs.error's limit.
const maxRunError = 2000

// Jobs returns the scheduled jobs of talent (01 §7). The retention purge
// is no job of its own: kvkk.retention_sweep runs Module.Sweeper (BE-079).
func (m Module) Jobs() []queue.Job {
	return []queue.Job{{Name: RecomputeJobName, Cron: "0 3 * * *", Queue: queue.Low, Unique: 23 * time.Hour,
		Timeout: 30 * time.Minute, Handle: m.svc.recompute}}
}

// Recompute runs the nightly score once (tests, operators).
func (m Module) Recompute(ctx context.Context) error { return m.svc.recompute(ctx) }

// recompute is talent.recompute. With collection off (T-32) or without a
// config version in force it does nothing and records no run. Otherwise it
// records a run, scores every user in batches and records the outcome:
// succeeded with the number of scores written, or failed with the error
// (an invalid config version fails without retry; a new version fixes it).
// It is idempotent: a second run over the same ledger at the same instant
// writes the same scores.
func (s *service) recompute(ctx context.Context) error {
	if err := systemAction(ctx, RecomputeJobName, authz.TalentScoreCompute); err != nil {
		return queue.Permanent(err)
	}
	if !s.d.CollectionEnabled {
		s.d.Log.LogAttrs(ctx, slog.LevelInfo, "talent recompute skipped: collection is off (talent.default_enabled, T-32)")
		return nil
	}
	now := s.now()
	var cfg ConfigVersion
	err := s.inTx(ctx, func(ctx context.Context) error {
		var err error
		cfg, err = s.d.Store.CurrentConfig(ctx, now)
		return err
	})
	switch {
	case errors.Is(err, ErrNotFound):
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "talent recompute skipped: no config version in force (createTalentConfigVersion)")
		return nil
	case err != nil:
		return fmt.Errorf("%s: %w", RecomputeJobName, err)
	}
	run := ScoreRun{ID: uuid.NewV7(), ConfigVersionID: cfg.ID, StartedAt: now, Status: RunRunning}
	if err := s.inTx(ctx, func(ctx context.Context) error { return s.d.Store.InsertRun(ctx, run) }); err != nil {
		return fmt.Errorf("%s: %w", RecomputeJobName, err)
	}
	n, scoreErr := s.score(ctx, cfg, run.ID, now)
	finished := s.now()
	run.FinishedAt, run.UsersProcessed, run.Status = &finished, n, RunSucceeded
	if scoreErr != nil {
		run.Status, run.Error = RunFailed, cut(scoreErr.Error(), maxRunError)
	}
	// The outcome is recorded even when the job's context ended.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := s.inTx(fctx, func(ctx context.Context) error { return s.d.Store.FinishRun(ctx, run) }); err != nil {
		return fmt.Errorf("%s: finish run: %w", RecomputeJobName, errors.Join(scoreErr, err))
	}
	if scoreErr != nil {
		if errors.Is(scoreErr, errInvalidParams) {
			scoreErr = queue.Permanent(scoreErr)
		}
		return fmt.Errorf("%s: %w", RecomputeJobName, scoreErr)
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "talent scores recomputed", slog.String("run_id", run.ID.String()),
		slog.Int("users", n))
	return nil
}

// scorer is the state of one run.
type scorer struct {
	p    ScoreParams
	defs map[string]SignalDef
	now  time.Time
	cfg  uuid.UUID
	run  uuid.UUID
}

// subjectFacts is one user of the run.
type subjectFacts struct {
	id         uuid.UUID
	university uuid.UUID
	year       int // 1..8, 0 = unknown
	neutral    bool
	raw, n     int
	top        []Contribution
}

// score computes and writes the scores of the run and returns how many it
// wrote.
func (s *service) score(ctx context.Context, cfg ConfigVersion, runID uuid.UUID, now time.Time) (int, error) {
	p, err := parseParams(cfg.Params)
	if err != nil {
		return 0, err
	}
	sc := scorer{p: p, now: now, cfg: cfg.ID, run: runID, defs: map[string]SignalDef{}}
	since := now.Add(-time.Duration(p.LookbackDays) * 24 * time.Hour)
	var users []uuid.UUID
	err = s.inTx(ctx, func(ctx context.Context) error {
		var err error
		if users, err = s.d.Store.ScoringUsers(ctx, since); err != nil {
			return err
		}
		defs, err := s.d.Store.SignalDefs(ctx)
		for _, d := range defs {
			sc.defs[d.Code] = d
		}
		return err
	})
	if err != nil {
		return 0, err
	}
	slices.SortFunc(users, compareUUID)

	var kept []subjectFacts
	var drop []uuid.UUID
	figures := eventFigures{}
	for batch := range slices.Chunk(users, recomputeBatch) {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		// The students read runs on app_rw, before the talent transaction.
		cohorts, err := s.d.Students.Cohorts(ctx, batch)
		if err != nil {
			return 0, fmt.Errorf("talent: cohorts: %w", err)
		}
		var rows []PointEvent
		var neutral map[uuid.UUID]bool
		err = s.inTx(ctx, func(ctx context.Context) error {
			var err error
			if rows, err = s.d.Store.PointsSince(ctx, batch, since); err != nil {
				return err
			}
			neutral, err = s.d.Store.ActiveNeutralized(ctx, batch)
			return err
		})
		if err != nil {
			return 0, err
		}
		// The gaming thresholds read identity and events on app_rw, after
		// the talent transaction.
		if rows, err = s.screenRows(ctx, p, figures, rows, neutral); err != nil {
			return 0, err
		}
		byUser := map[uuid.UUID][]PointEvent{}
		for _, r := range rows {
			byUser[r.UserID] = append(byUser[r.UserID], r)
		}
		for _, id := range batch {
			co, student := cohorts[id]
			ledger := byUser[id]
			// No student profile, a minor (T-23) or nothing in the window
			// (every row dropped by a gaming threshold included): no score
			// at all (band 0 by absence).
			if !student || co.IsMinor || len(ledger) == 0 {
				drop = append(drop, id)
				continue
			}
			f := subjectFacts{id: id, university: co.UniversityID, year: classYear(co), neutral: neutral[id]}
			if !f.neutral {
				f.raw, f.n, f.top = sc.aggregate(ledger)
			}
			kept = append(kept, f)
		}
	}

	scores := sc.rank(kept)
	for batch := range slices.Chunk(scores, recomputeBatch) {
		if err := s.inTx(ctx, func(ctx context.Context) error { return s.d.Store.PutScores(ctx, batch) }); err != nil {
			return 0, err
		}
	}
	for batch := range slices.Chunk(drop, recomputeBatch) {
		if err := s.inTx(ctx, func(ctx context.Context) error {
			_, err := s.d.Store.DeleteScores(ctx, batch)
			return err
		}); err != nil {
			return 0, err
		}
	}
	return len(scores), nil
}

// classYear is the cohort's class year when the cohort key can carry it
// (1..8, 02 §15.5), else 0.
func classYear(c students.Cohort) int {
	if c.ClassYear == nil || *c.ClassYear < 1 || *c.ClassYear > 8 {
		return 0
	}
	return int(*c.ClassYear)
}

// aggregate is one user's raw score (R-003 §3.4): every row decays with
// its signal's half-life (value = points × 0.5^(age / half-life); no
// half-life: no decay; negative rows decay too), the decayed values are
// summed by category in occurred_at order, each category is floored at 0
// and capped by the version's category cap, and rounded to cp; raw is the
// sum of the categories. n counts the signals that carry points and were
// not reversed (a reversal row cancels its original with the same decay).
// top are the five largest positive decayed contributions by signal.
func (sc scorer) aggregate(ledger []PointEvent) (raw, n int, top []Contribution) {
	byCat, bySignal := map[string]float64{}, map[string]float64{}
	reversed := map[uuid.UUID]bool{}
	for _, r := range ledger {
		if r.ReversalOf != nil {
			reversed[*r.ReversalOf] = true
		}
	}
	for _, r := range ledger {
		d, ok := sc.defs[r.Signal]
		if !ok {
			continue // talent.point_events.signal_code is a FK: unreachable
		}
		v := float64(float64(r.PointsCP) * sc.decay(d, r.OccurredAt))
		byCat[d.Category] = float64(byCat[d.Category] + v)
		bySignal[r.Signal] = float64(bySignal[r.Signal] + v)
		if r.ReversalOf == nil && r.PointsCP != 0 && !reversed[r.ID] {
			n++
		}
	}
	for _, c := range Categories() {
		v := min(max(byCat[c], 0), float64(sc.p.CategoryCapsCP[c]))
		raw += int(math.Round(v))
	}
	for sig, v := range bySignal {
		if cp := int(math.Round(v)); cp > 0 {
			top = append(top, Contribution{Signal: sig, ValueCP: cp})
		}
	}
	slices.SortFunc(top, func(a, b Contribution) int {
		return cmp.Or(cmp.Compare(b.ValueCP, a.ValueCP), strings.Compare(a.Signal, b.Signal))
	})
	if len(top) > 5 {
		top = top[:5]
	}
	return raw, n, top
}

// decay is the factor of a row of d that occurred at at, at the run's
// instant: 1 without a half-life or for a row from the future.
func (sc scorer) decay(d SignalDef, at time.Time) float64 {
	if d.HalfLifeDays == nil || *d.HalfLifeDays <= 0 {
		return 1
	}
	age := sc.now.Sub(at).Hours() / 24
	if age <= 0 {
		return 1
	}
	return math.Pow(0.5, age/float64(*d.HalfLifeDays))
}

// cohort keys of 02 §15.5.
func yearKey(uni uuid.UUID, year int) string { return fmt.Sprintf("uni:%s:year:%d", uni, year) }
func uniKey(uni uuid.UUID) string            { return "uni:" + uni.String() }

const globalKey = "global"

// rank turns the users of the run into scores. The ranked population is
// every user that is not neutralised. A user's cohort is university +
// class year when that group has at least cohort_min users, else the
// university when it has, else global; the percentile is the user's
// percent rank by raw score within the whole population of that cohort
// (the share of the cohort with a lower raw score; ties share a rank, a
// cohort of one is 0). It shrinks towards the median with few signals
// (w = n / (n + shrink_k), pct_final = w·pct + (1 − w)·0.5) and lifts only
// upwards: the band is the highest rule whose min_pct_bp pct_final
// reaches, and 0 (neutral, never a penalty) for a neutralised user, a user
// without points or signals and a percentile below the first rule.
func (sc scorer) rank(users []subjectFacts) []Score {
	years, unis := map[string][]int{}, map[string][]int{}
	var all []int
	for _, u := range users {
		if u.neutral {
			continue
		}
		all = append(all, u.raw)
		unis[uniKey(u.university)] = append(unis[uniKey(u.university)], u.raw)
		if u.year > 0 {
			k := yearKey(u.university, u.year)
			years[k] = append(years[k], u.raw)
		}
	}
	for _, m := range []map[string][]int{years, unis} {
		for _, v := range m {
			slices.Sort(v)
		}
	}
	slices.Sort(all)

	out := make([]Score, 0, len(users))
	for _, u := range users {
		key, pop := globalKey, all
		switch yk := yearKey(u.university, u.year); {
		case u.year > 0 && len(years[yk]) >= sc.p.CohortMin:
			key, pop = yk, years[yk]
		case len(unis[uniKey(u.university)]) >= sc.p.CohortMin:
			key, pop = uniKey(u.university), unis[uniKey(u.university)]
		}
		s := Score{UserID: u.id, CohortKey: key, ConfigVersionID: sc.cfg, RunID: sc.run, CalculatedAt: sc.now,
			PctBP: 5000, TopContributors: []Contribution{}, Neutralized: u.neutral}
		if !u.neutral {
			s.RawCP, s.NSignals, s.TopContributors = u.raw, u.n, u.top
			if s.TopContributors == nil {
				s.TopContributors = []Contribution{}
			}
			s.PctBP = sc.pctFinalBP(percentRank(pop, u.raw), u.n)
			s.Band = sc.band(s)
		}
		out = append(out, s)
	}
	return out
}

// percentRank is (number of values below v) / (len − 1) over the sorted
// population; 0 for a population of one.
func percentRank(sorted []int, v int) float64 {
	if len(sorted) < 2 {
		return 0
	}
	below, _ := slices.BinarySearch(sorted, v)
	return float64(below) / float64(len(sorted)-1)
}

// pctFinalBP shrinks pct towards 0.5 with few signals, in basis points.
func (sc scorer) pctFinalBP(pct float64, n int) int {
	w := 0.0
	if n > 0 {
		w = float64(n) / float64(n+sc.p.ShrinkK)
	}
	v := float64(float64(w*pct) + float64((1-w)*0.5))
	return min(max(int(math.Round(v*10000)), 0), 10000)
}

// band applies the upward-only rules to a non-neutralised score.
func (sc scorer) band(s Score) Band {
	if s.Neutralized || s.RawCP <= 0 || s.NSignals == 0 {
		return BandNeutral
	}
	b := BandNeutral
	for _, r := range sc.p.Bands {
		if s.PctBP >= r.MinPctBP {
			b = bandOfInt(r.Band)
		}
	}
	return b
}

func compareUUID(a, b uuid.UUID) int { return strings.Compare(string(a[:]), string(b[:])) }
