package talent_test

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"testing"
	"time"
	"uuid"

	"koro/internal/students"
	"koro/internal/talent"
)

// day is a day of the test clock's past.
func day(n float64) time.Time { return t0.Add(-time.Duration(n * 24 * float64(time.Hour))) }

// scoreEnv is an environment with collection on and a config version of
// params in force since the day before t0 (test values).
func scoreEnv(t *testing.T, p talent.ScoreParams) *env {
	t.Helper()
	e := buildEnvWith(t, envOptions{collection: true})
	e.cfg = e.version(t, t0.Add(-24*time.Hour), p)
	return e
}

// version inserts a config version of p effective from at.
func (e *env) version(t *testing.T, at time.Time, p talent.ScoreParams) talent.ConfigVersion {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var v talent.ConfigVersion
	e.inTalentTx(t, func(ctx context.Context) error {
		v, err = e.store.InsertConfigVersion(ctx, talent.ConfigVersion{ID: uuid.NewV7(), EffectiveFrom: at, Params: raw,
			CreatedBy: uuid.NewV7()})
		return err
	})
	return v
}

// studentAt adds an adult student of uni in class year (nil: unknown).
func (e *env) studentAt(uni uuid.UUID, year *int16) uuid.UUID {
	id := uuid.NewV7()
	e.students.mu.Lock()
	defer e.students.mu.Unlock()
	e.students.cohorts[id] = students.Cohort{UniversityID: uni, ClassYear: year}
	return id
}

// row appends a ledger row of user (signal, points, occurred at) as a
// consumer would have written it.
func (e *env) row(t *testing.T, user uuid.UUID, signal string, points int, at time.Time) talent.PointEvent {
	t.Helper()
	p := talent.PointEvent{ID: uuid.NewV7(), UserID: user, Signal: signal, SourceType: talent.SourceCheckin, SourceID: uuid.NewV7(),
		OccurredAt: at, RawPointsCP: points, PointsCP: points, ReasonCode: signal, ConfigVersionID: e.cfg.ID, SourceEventID: uuid.NewV7()}
	e.inTalentTx(t, func(ctx context.Context) error {
		var err error
		p, _, err = e.store.InsertPoint(ctx, p)
		return err
	})
	return p
}

// recompute runs the nightly score.
func (e *env) recompute(t *testing.T) {
	t.Helper()
	if err := e.mod.Recompute(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// score returns user's score row (fails without one).
func (e *env) score(t *testing.T, user uuid.UUID) talent.Score {
	t.Helper()
	s, ok := e.store.AllScores()[user]
	if !ok {
		t.Fatalf("no score of %s", user)
	}
	return s
}

// bigCaps are category caps no test reaches.
func bigCaps(p talent.ScoreParams) talent.ScoreParams {
	for _, c := range talent.Categories() {
		p.CategoryCapsCP[c] = 1000000
	}
	return p
}

// TestRecompute_HalfLifeDecay_Table (R-003 §3.4): every row decays as
// points × 0.5^(age / half-life) at the run's instant; a signal without a
// half-life (profile_section_complete) never decays; a negative row decays
// too and pulls its category down (never below 0); a row from the future
// counts in full; a row older than lookback_days is out of the window
// (its user has no score).
func TestRecompute_HalfLifeDecay_Table(t *testing.T) {
	t.Parallel()
	e := scoreEnv(t, bigCaps(testParams()))
	type row struct {
		signal string
		points int
		age    float64 // days before t0
	}
	cases := []struct {
		name string
		rows []row
		raw  int
	}{
		{"check-in today", []row{{talent.SignalEventCheckin, 500, 0}}, 500},
		{"check-in one half-life ago (120 d)", []row{{talent.SignalEventCheckin, 500, 120}}, 250},
		{"check-in two half-lives ago", []row{{talent.SignalEventCheckin, 500, 240}}, 125},
		{"check-in half a half-life ago", []row{{talent.SignalEventCheckin, 500, 60}}, 354}, // 500 / √2
		{"registration two half-lives ago (45 d)", []row{{talent.SignalRegistrationConfirmed, 100, 90}}, 25},
		{"like one half-life ago (30 d)", []row{{talent.SignalFeedLike, 10, 30}}, 5},
		{"profile section never decays", []row{{talent.SignalProfileSection, 200, 400}}, 200},
		{"no-show decays (60 d) and pulls participation down", []row{{talent.SignalEventCheckin, 500, 0}, {talent.SignalEventNoShow, -200, 60}}, 400},
		{"no-show alone is floored at 0", []row{{talent.SignalEventNoShow, -200, 1}}, 0},
		{"a row from the future counts in full", []row{{talent.SignalEventCheckin, 500, -1}}, 500},
		{"categories add up", []row{{talent.SignalEventCheckin, 500, 120}, {talent.SignalProfileSection, 200, 10}}, 450},
	}
	users := make([]uuid.UUID, len(cases))
	for i, c := range cases {
		users[i] = e.student()
		for _, r := range c.rows {
			e.row(t, users[i], r.signal, r.points, day(r.age))
		}
	}
	old := e.student()
	e.row(t, old, talent.SignalEventCheckin, 500, day(541)) // lookback_days 540
	e.recompute(t)
	for i, c := range cases {
		if got := e.score(t, users[i]).RawCP; got != c.raw {
			t.Errorf("%s: raw %d, want %d", c.name, got, c.raw)
		}
	}
	if _, ok := e.store.AllScores()[old]; ok {
		t.Error("a user with nothing in the window was scored")
	}
}

// TestRecompute_CategoryCap (R-003 §3.3-4): each category's decayed sum is
// capped by the version's category cap and floored at 0, independently of
// the other categories: likes cannot buy more than the engagement cap.
func TestRecompute_CategoryCap(t *testing.T) {
	t.Parallel()
	e := scoreEnv(t, testParams()) // participation 4000, engagement 300
	liker, attender, mixed, penalised := e.student(), e.student(), e.student(), e.student()
	for range 40 {
		e.row(t, liker, talent.SignalFeedLike, 10, t0)
		e.row(t, mixed, talent.SignalFeedLike, 10, t0)
	}
	for range 10 {
		e.row(t, attender, talent.SignalEventCheckin, 500, t0)
	}
	e.row(t, mixed, talent.SignalEventCheckin, 500, t0)
	e.row(t, penalised, talent.SignalEventNoShow, -200, t0)
	e.recompute(t)
	for _, c := range []struct {
		name string
		user uuid.UUID
		raw  int
	}{{"400 cp of likes", liker, 300}, {"5000 cp of check-ins", attender, 4000}, {"capped likes + a check-in", mixed, 800},
		{"a no-show alone", penalised, 0}} {
		if got := e.score(t, c.user).RawCP; got != c.raw {
			t.Errorf("%s: raw %d, want %d", c.name, got, c.raw)
		}
	}
	if s := e.score(t, penalised); s.Band != talent.BandNeutral || s.NSignals != 1 {
		t.Errorf("penalised = band %v n %d, want neutral", int(s.Band), s.NSignals)
	}
	top := e.score(t, mixed).TopContributors
	if len(top) != 2 || top[0].Signal != talent.SignalEventCheckin || top[0].ValueCP != 500 || top[1].ValueCP != 400 {
		t.Errorf("top contributors = %+v", top)
	}
}

// TestRecompute_CohortPercentile (02 §15.5, R-003 §3.4): the cohort is
// university + class year with at least cohort_min ranked users, else the
// university, else global; the percentile is the percent rank by raw score
// within that whole cohort (ties share it); with few signals it shrinks
// towards the median. Minors and users without a student profile get no
// score (a stale one is removed); the cohort rows follow.
func TestRecompute_CohortPercentile(t *testing.T) {
	t.Parallel()
	p := bigCaps(testParams())
	p.ShrinkK = 0
	e := scoreEnv(t, p)
	u1, u2 := uuid.NewV7(), uuid.NewV7()
	y2, y3 := int16(2), int16(3)
	raw := func(user uuid.UUID, cp int) { e.row(t, user, talent.SignalProfileSection, cp, t0) }
	var year2 []uuid.UUID
	for i := 1; i <= 10; i++ {
		u := e.studentAt(u1, &y2)
		raw(u, 100*i)
		year2 = append(year2, u)
	}
	low3, high3 := e.studentAt(u1, &y3), e.studentAt(u1, &y3)
	raw(low3, 150)
	raw(high3, 1050)
	small := []uuid.UUID{e.studentAt(u2, nil), e.studentAt(u2, nil), e.studentAt(u2, nil)}
	raw(small[0], 120)
	raw(small[1], 500)
	raw(small[2], 2000)
	minor := e.minor()
	raw(minor, 900)
	stranger := uuid.NewV7() // no student profile
	raw(stranger, 900)
	e.store.PutScore(stranger, 3, false) // stale
	e.recompute(t)

	for _, c := range []struct {
		name   string
		user   uuid.UUID
		cohort string
		pct    int
	}{
		{"lowest of year 2", year2[0], "uni:" + u1.String() + ":year:2", 0},
		{"second of year 2", year2[1], "uni:" + u1.String() + ":year:2", 1111},
		{"fifth of year 2", year2[4], "uni:" + u1.String() + ":year:2", 4444},
		{"top of year 2", year2[9], "uni:" + u1.String() + ":year:2", 10000},
		{"year 3 (2 < 10) falls back to the university, low", low3, "uni:" + u1.String(), 909},
		{"year 3, high", high3, "uni:" + u1.String(), 10000},
		{"small university falls back to global", small[0], "global", 714},
		{"global, tied with a year-2 student", small[1], "global", 4286},
		{"global, top", small[2], "global", 10000},
	} {
		s := e.score(t, c.user)
		if s.CohortKey != c.cohort || s.PctBP != c.pct {
			t.Errorf("%s: cohort %s pct %d, want %s %d", c.name, s.CohortKey, s.PctBP, c.cohort, c.pct)
		}
		if e.store.Cohorts()[c.user] != c.cohort {
			t.Errorf("%s: cohort row %q", c.name, e.store.Cohorts()[c.user])
		}
	}
	for name, u := range map[string]uuid.UUID{"minor": minor, "no student profile": stranger} {
		if _, ok := e.store.AllScores()[u]; ok {
			t.Errorf("%s was scored", name)
		}
		if _, ok := e.store.Cohorts()[u]; ok {
			t.Errorf("%s has a cohort row", name)
		}
	}

	// Shrinkage (w = n / (n + k)): with k = 5 one signal pulls the top of
	// year 2 from 1.0 to 1/6 · 1 + 5/6 · 0.5.
	p.ShrinkK = 5
	e.version(t, t0.Add(-time.Hour), p)
	e.recompute(t)
	if s := e.score(t, year2[9]); s.PctBP != 5833 || s.Band != 1 {
		t.Errorf("shrunk top of year 2: pct %d band %d, want 5833 band 1", s.PctBP, int(s.Band))
	}
	if s := e.score(t, year2[0]); s.PctBP != 4167 || s.Band != talent.BandNeutral {
		t.Errorf("shrunk bottom of year 2: pct %d band %d, want 4167 band 0", s.PctBP, int(s.Band))
	}
}

// TestRecompute_OnlyUpwardBands_LowIsNeutral (ADR-0015, DECISIONS-LOG
// 2026-10-02): the bands only lift — 1, 2, 3 from the version's
// thresholds — and everything below the first threshold, a cohort where
// everybody ties, a user whose points were all reversed and a user with 0
// points are band 0: neutral, the same as no score, never a penalty band.
func TestRecompute_OnlyUpwardBands_LowIsNeutral(t *testing.T) {
	t.Parallel()
	p := bigCaps(testParams())
	p.ShrinkK = 0
	e := scoreEnv(t, p)
	uni, y1 := uuid.NewV7(), int16(1)
	var users []uuid.UUID
	for i := 1; i <= 10; i++ {
		u := e.studentAt(uni, &y1)
		e.row(t, u, talent.SignalProfileSection, 100*i, t0)
		users = append(users, u)
	}
	reversed := e.studentAt(uni, &y1)
	orig := e.row(t, reversed, talent.SignalProfileSection, 5000, t0)
	e.inTalentTx(t, func(ctx context.Context) error {
		of := orig.ID
		_, _, err := e.store.InsertPoint(ctx, talent.PointEvent{ID: uuid.NewV7(), UserID: reversed, Signal: orig.Signal,
			SourceType: talent.SourceReversal, SourceID: orig.ID, OccurredAt: orig.OccurredAt, RawPointsCP: -5000, PointsCP: -5000,
			ReasonCode: "registration_cancelled", ConfigVersionID: e.cfg.ID, ReversalOf: &of, SourceEventID: uuid.NewV7()})
		return err
	})
	// Ten students of another university, all with the same score.
	other := uuid.NewV7()
	var tied []uuid.UUID
	for range 10 {
		u := e.studentAt(other, &y1)
		e.row(t, u, talent.SignalProfileSection, 300, t0)
		tied = append(tied, u)
	}
	e.recompute(t)
	// 11 ranked in the university (the reversed user ranks 0): pct = below/10.
	want := []talent.Band{0, 0, 0, 0, 1, 1, 1, 2, 3, 3}
	for i, u := range users {
		if s := e.score(t, u); s.Band != want[i] {
			t.Errorf("user %d (raw %d, pct %d): band %d, want %d", i+1, s.RawCP, s.PctBP, int(s.Band), int(want[i]))
		}
	}
	if s := e.score(t, reversed); s.RawCP != 0 || s.NSignals != 0 || s.Band != talent.BandNeutral || s.PctBP != 5000 {
		t.Errorf("all points reversed: %+v", s)
	}
	for _, u := range tied {
		if s := e.score(t, u); s.PctBP != 0 || s.Band != talent.BandNeutral {
			t.Fatalf("tied cohort: pct %d band %d, want neutral", s.PctBP, int(s.Band))
		}
	}
	bands, err := e.mod.Ranking.Bands(t.Context(), append(append([]uuid.UUID{}, users...), tied...))
	if err != nil || len(bands) != 6 {
		t.Fatalf("Bands = %d lifted, %v; want 6 (the low half is neutral, absent)", len(bands), err)
	}
	if _, ok := bands[users[0]]; ok {
		t.Fatal("the lowest student is lifted")
	}
}

// TestRecompute_NeutralizedUserBand0 (02 §15.6 CHECK, ADR-0015): a user
// with an active neutralisation (auditor, opt-out or objection) gets a
// neutralised score — band 0, no points, no contributors — and stays out
// of every cohort ranking, so the others rank among themselves; Bands
// keeps the user neutral.
func TestRecompute_NeutralizedUserBand0(t *testing.T) {
	t.Parallel()
	p := bigCaps(testParams())
	p.ShrinkK = 0
	e := scoreEnv(t, p)
	uni, y4 := uuid.NewV7(), int16(4)
	var users []uuid.UUID
	for i := 1; i <= 12; i++ {
		u := e.studentAt(uni, &y4)
		e.row(t, u, talent.SignalProfileSection, 100*i, t0)
		users = append(users, u)
	}
	top, second := users[11], users[10]
	if err := e.mod.Auditor.Neutralize(t.Context(), top, talent.NeutralizeAdmin, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.mod.Auditor.Neutralize(t.Context(), second, talent.NeutralizeObjection, ptr(uuid.NewV7())); err != nil {
		t.Fatal(err)
	}
	e.recompute(t)
	for _, u := range []uuid.UUID{top, second} {
		s := e.score(t, u)
		if !s.Neutralized || s.Band != talent.BandNeutral || s.RawCP != 0 || s.NSignals != 0 || len(s.TopContributors) != 0 {
			t.Fatalf("neutralised score = %+v", s)
		}
	}
	// Ten ranked users remain in the year cohort: the third best is the top.
	if s := e.score(t, users[9]); s.PctBP != 10000 || s.Band != 3 || s.CohortKey != "uni:"+uni.String()+":year:4" {
		t.Fatalf("best of the rest = %+v", s)
	}
	bands, err := e.mod.Ranking.Bands(t.Context(), []uuid.UUID{top, second, users[9]})
	if err != nil || len(bands) != 1 {
		t.Fatalf("Bands = %d lifted, %v; want only the non-neutralised", len(bands), err)
	}
}

// TestRecompute_Idempotent_RunRecorded (01 §7): every run is a score_runs
// row (running → succeeded with the users written, the version in force);
// a second run at the same instant writes the same scores, and so does a
// run over the same ledger written in another order (deterministic). With
// collection off or without a version nothing runs; a version whose params
// do not validate records a failed run without retry and leaves the scores.
func TestRecompute_Idempotent_RunRecorded(t *testing.T) {
	t.Parallel()
	p := testParams()
	type entry struct {
		user   int
		signal string
		points int
		age    float64
	}
	var entries []entry
	r := rand.New(rand.NewPCG(52, 47))
	signals := []string{talent.SignalEventCheckin, talent.SignalRegistrationConfirmed, talent.SignalFeedLike, talent.SignalProfileSection,
		talent.SignalEventNoShow}
	for i := range 120 {
		sig := signals[r.IntN(len(signals))]
		pts := map[string]int{talent.SignalEventCheckin: 500, talent.SignalRegistrationConfirmed: 100, talent.SignalFeedLike: 10,
			talent.SignalProfileSection: 200, talent.SignalEventNoShow: -200}[sig]
		entries = append(entries, entry{user: i % 30, signal: sig, points: pts, age: float64(r.IntN(400))})
	}
	uni, y2 := uuid.NewV7(), int16(2)
	build := func(order []int) (*env, []uuid.UUID) {
		e := scoreEnv(t, p)
		users := make([]uuid.UUID, 30)
		for i := range users {
			users[i] = uuid.NewV7()
			e.students.cohorts[users[i]] = students.Cohort{UniversityID: uni, ClassYear: &y2}
		}
		for _, i := range order {
			en := entries[i]
			e.row(t, users[en.user], en.signal, en.points, day(en.age))
		}
		return e, users
	}
	order := make([]int, len(entries))
	for i := range order {
		order[i] = i
	}
	e, users := build(order)
	e.recompute(t)
	first := e.store.AllScores()
	e.recompute(t)
	second := e.store.AllScores()
	runs := e.store.Runs()
	if len(runs) != 2 {
		t.Fatalf("runs = %+v", runs)
	}
	for _, run := range runs {
		if run.Status != talent.RunSucceeded || run.UsersProcessed != 30 || run.FinishedAt == nil || run.ConfigVersionID != e.cfg.ID ||
			!run.StartedAt.Equal(t0) {
			t.Fatalf("run = %+v", run)
		}
	}
	if runs[0].ID == runs[1].ID {
		t.Fatal("two runs share an ID")
	}
	for _, u := range users {
		a, b := first[u], second[u]
		if a.RunID != runs[0].ID || b.RunID != runs[1].ID {
			t.Fatalf("run IDs %s %s", a.RunID, b.RunID)
		}
		a.RunID, b.RunID = uuid.Nil(), uuid.Nil()
		if !sameScore(a, b) {
			t.Fatalf("second run changed %s: %+v → %+v", u, a, b)
		}
	}
	// The same ledger written in another order scores the same.
	r.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
	e2, users2 := build(order)
	e2.recompute(t)
	again := e2.store.AllScores()
	for i := range users {
		a, b := first[users[i]], again[users2[i]]
		if a.RawCP != b.RawCP || a.PctBP != b.PctBP || a.Band != b.Band || a.NSignals != b.NSignals || !sameTop(a, b) {
			t.Fatalf("user %d differs with another write order: %+v vs %+v", i, a, b)
		}
	}

	// Collection off: nothing runs.
	off := buildEnvWith(t, envOptions{})
	off.cfg = off.version(t, t0.Add(-24*time.Hour), p)
	if err := off.mod.Recompute(t.Context()); err != nil || len(off.store.Runs()) != 0 {
		t.Fatalf("collection off: %v, runs %d", err, len(off.store.Runs()))
	}
	// No version in force: nothing runs.
	none := buildEnvWith(t, envOptions{collection: true})
	if err := none.mod.Recompute(t.Context()); err != nil || len(none.store.Runs()) != 0 {
		t.Fatalf("no version: %v, runs %d", err, len(none.store.Runs()))
	}
	// A version that does not validate: a failed run, no retry, scores kept.
	before := e.store.AllScores()
	e.inTalentTx(t, func(ctx context.Context) error {
		_, err := e.store.InsertConfigVersion(ctx, talent.ConfigVersion{ID: uuid.NewV7(), EffectiveFrom: t0.Add(-time.Minute),
			Params: json.RawMessage(`{"shrink_k":5}`), CreatedBy: uuid.NewV7()})
		return err
	})
	err := e.mod.Recompute(t.Context())
	if err == nil || !isPermanent(err) {
		t.Fatalf("invalid params: %v, want a permanent error", err)
	}
	runs = e.store.Runs()
	if last := runs[len(runs)-1]; last.Status != talent.RunFailed || last.Error == "" || last.UsersProcessed != 0 {
		t.Fatalf("failed run = %+v", last)
	}
	after := e.store.AllScores()
	for u, s := range before {
		if !sameScore(s, after[u]) {
			t.Fatal("a failed run changed a score")
		}
	}
}

func sameScore(a, b talent.Score) bool {
	return a.UserID == b.UserID && a.RawCP == b.RawCP && a.PctBP == b.PctBP && a.Band == b.Band && a.NSignals == b.NSignals &&
		a.CohortKey == b.CohortKey && a.ConfigVersionID == b.ConfigVersionID && a.RunID == b.RunID && a.Neutralized == b.Neutralized &&
		a.CalculatedAt.Equal(b.CalculatedAt) && sameTop(a, b)
}

func sameTop(a, b talent.Score) bool {
	ja, _ := json.Marshal(a.TopContributors)
	jb, _ := json.Marshal(b.TopContributors)
	return string(ja) == string(jb)
}
