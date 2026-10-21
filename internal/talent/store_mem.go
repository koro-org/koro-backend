package talent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"uuid"

	"koro/internal/platform/civil"
	"koro/internal/platform/db"
	"koro/internal/platform/problem"
)

// MemStore is the in-memory Store of service, wiring and authz matrix tests
// (05 §13.2). It keeps the PostgreSQL contract that matters to the service:
// the talent transaction (Enter) is required exactly as in PostgreSQL, the
// dedupe key and the one-reversal rule, the active neutralisation index,
// the objection-needs-a-request CHECK, and created_at from its own clock
// (the database stamps it in PostgreSQL). A rollback is not emulated
// (db.NopTx).
type MemStore struct {
	mu      sync.Mutex
	now     func() time.Time
	defs    map[string]SignalDef
	configs []ConfigVersion
	points  []PointEvent
	counter map[counterKey]int
	neutral []Neutralization
	scores  map[uuid.UUID]Score
	cohorts map[uuid.UUID]string
	runs    []ScoreRun
	// ErasedUsers lists the users whose erasure request is completed: the
	// ledger may be erased only for them (talent.point_events_erase_user).
	// nil means every user (wiring tests have no kvkk state to consult).
	ErasedUsers map[uuid.UUID]bool
}

type counterKey struct {
	user   uuid.UUID
	day    civil.Date
	signal string
}

var _ Store = (*MemStore)(nil)

// NewMemStore returns an empty store seeded with the signal catalogue of
// 000081 (the starting weights; a product decision, data not code).
func NewMemStore(now func() time.Time) *MemStore {
	m := &MemStore{now: now, defs: map[string]SignalDef{}, counter: map[counterKey]int{},
		scores: map[uuid.UUID]Score{}, cohorts: map[uuid.UUID]string{}}
	for _, d := range seedSignals() {
		m.defs[d.Code] = d
	}
	return m
}

func ip(v int) *int { return &v }

// seedSignals mirrors 000081.
func seedSignals() []SignalDef {
	return []SignalDef{
		{Code: SignalEventCheckin, Category: CategoryParticipation, BasePointsCP: 500, DailyCapCP: ip(1000), HalfLifeDays: ip(120), ExcludedWhenSensitive: true, Enabled: true},
		{Code: SignalRegistrationConfirmed, Category: CategoryParticipation, BasePointsCP: 100, DailyCapCP: ip(300), WeeklyCapCP: ip(500), HalfLifeDays: ip(45), ExcludedWhenSensitive: true, Enabled: true},
		{Code: SignalEventNoShow, Category: CategoryParticipation, BasePointsCP: -200, WeeklyCapCP: ip(400), HalfLifeDays: ip(60), ExcludedWhenSensitive: true, Enabled: true},
		{Code: "opportunity_application", Category: CategoryIntent, BasePointsCP: 300, DailyCapCP: ip(900), HalfLifeDays: ip(90), ExcludedWhenSensitive: true, Enabled: false},
		{Code: SignalProfileSection, Category: CategoryProfile, BasePointsCP: 200, ExcludedWhenSensitive: true, Enabled: true},
		{Code: "profile_refresh", Category: CategoryProfile, BasePointsCP: 200, HalfLifeDays: ip(365), ExcludedWhenSensitive: true, Enabled: true},
		{Code: SignalClubMembership, Category: CategoryCommunity, BasePointsCP: 200, ExcludedWhenSensitive: true, Enabled: true},
		{Code: SignalFeedLike, Category: CategoryEngagement, BasePointsCP: 10, DailyCapCP: ip(50), HalfLifeDays: ip(30), ExcludedWhenSensitive: true, Enabled: true},
		{Code: SignalFeedSave, Category: CategoryEngagement, BasePointsCP: 10, DailyCapCP: ip(50), HalfLifeDays: ip(30), ExcludedWhenSensitive: true, Enabled: true},
		{Code: SignalActiveDay, Category: CategoryEngagement, BasePointsCP: 20, DailyCapCP: ip(20), WeeklyCapCP: ip(100), HalfLifeDays: ip(30), ExcludedWhenSensitive: true, Enabled: true},
	}
}

// SetSignalEnabled turns a definition on or off (tests).
func (m *MemStore) SetSignalEnabled(code string, on bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.defs[code]
	d.Enabled = on
	m.defs[code] = d
}

// PutScore stores a minimal score row in the global cohort (tests and
// matrix fixtures; the nightly run writes full rows with PutScores).
func (m *MemStore) PutScore(userID uuid.UUID, b Band, neutralized bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scores[userID] = Score{UserID: userID, Band: b, Neutralized: neutralized, CohortKey: "global", PctBP: 5000,
		TopContributors: []Contribution{}, CalculatedAt: m.stamp()}
	m.cohorts[userID] = "global"
}

// AllScores returns every score row by user (tests).
func (m *MemStore) AllScores() map[uuid.UUID]Score {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[uuid.UUID]Score, len(m.scores))
	for k, v := range m.scores {
		v.TopContributors = slices.Clone(v.TopContributors)
		out[k] = v
	}
	return out
}

// Cohorts returns every cohort row by user (tests).
func (m *MemStore) Cohorts() map[uuid.UUID]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[uuid.UUID]string, len(m.cohorts))
	maps.Copy(out, m.cohorts)
	return out
}

// Runs returns the score runs, oldest first (tests).
func (m *MemStore) Runs() []ScoreRun {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.runs)
}

// AllPoints returns every ledger row (tests).
func (m *MemStore) AllPoints() []PointEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.points)
}

// AllNeutralizations returns every neutralisation (tests).
func (m *MemStore) AllNeutralizations() []Neutralization {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.neutral)
}

// Counters returns the number of daily counter rows (tests).
func (m *MemStore) Counters() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.counter)
}

func (m *MemStore) check(ctx context.Context) error {
	if !db.InTx(ctx) || !inRole(ctx) {
		return errNoTalentTx
	}
	return nil
}

// Enter implements Store.
func (m *MemStore) Enter(ctx context.Context) (context.Context, error) {
	if !db.InTx(ctx) {
		return ctx, errNoTalentTx
	}
	return withRole(ctx, true), nil
}

// AsApp implements Store.
func (m *MemStore) AsApp(ctx context.Context, fn func(context.Context) error) error {
	if err := m.check(ctx); err != nil {
		return err
	}
	return fn(withRole(ctx, false))
}

// SignalDef implements Store.
func (m *MemStore) SignalDef(ctx context.Context, code string) (SignalDef, error) {
	if err := m.check(ctx); err != nil {
		return SignalDef{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.defs[code]
	if !ok {
		return SignalDef{}, ErrNotFound
	}
	return d, nil
}

// SignalDefs implements Store.
func (m *MemStore) SignalDefs(ctx context.Context) ([]SignalDef, error) {
	if err := m.check(ctx); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SignalDef, 0, len(m.defs))
	for _, d := range m.defs {
		out = append(out, d)
	}
	slices.SortFunc(out, func(a, b SignalDef) int { return strings.Compare(a.Code, b.Code) })
	return out, nil
}

// CurrentConfig implements Store.
func (m *MemStore) CurrentConfig(ctx context.Context, at time.Time) (ConfigVersion, error) {
	if err := m.check(ctx); err != nil {
		return ConfigVersion{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var best *ConfigVersion
	for i := range m.configs {
		v := &m.configs[i]
		if !v.EffectiveFrom.After(at) && (best == nil || v.EffectiveFrom.After(best.EffectiveFrom)) {
			best = v
		}
	}
	if best == nil {
		return ConfigVersion{}, ErrNotFound
	}
	return *best, nil
}

// InsertConfigVersion implements Store.
func (m *MemStore) InsertConfigVersion(ctx context.Context, v ConfigVersion) (ConfigVersion, error) {
	if err := m.check(ctx); err != nil {
		return ConfigVersion{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, o := range m.configs {
		if o.EffectiveFrom.Equal(v.EffectiveFrom) {
			return ConfigVersion{}, errMemUnique
		}
	}
	if len(v.Params) == 0 {
		v.Params = json.RawMessage(`{}`)
	}
	v.CreatedAt = m.stamp()
	m.configs = append(m.configs, v)
	return v, nil
}

// UpdateSignalDef implements Store.
func (m *MemStore) UpdateSignalDef(ctx context.Context, d SignalDef, _ uuid.UUID) (SignalDef, error) {
	if err := m.check(ctx); err != nil {
		return SignalDef{}, err
	}
	switch {
	case d.DailyCapCP != nil && *d.DailyCapCP <= 0, d.WeeklyCapCP != nil && *d.WeeklyCapCP <= 0,
		d.HalfLifeDays != nil && *d.HalfLifeDays <= 0:
		return SignalDef{}, errMemCheck
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.defs[d.Code]
	if !ok {
		return SignalDef{}, ErrNotFound
	}
	cur.BasePointsCP, cur.DailyCapCP, cur.WeeklyCapCP, cur.HalfLifeDays, cur.Enabled =
		d.BasePointsCP, d.DailyCapCP, d.WeeklyCapCP, d.HalfLifeDays, d.Enabled
	m.defs[d.Code] = cur
	return cur, nil
}

// LockCounter implements Store.
func (m *MemStore) LockCounter(ctx context.Context, userID uuid.UUID, day civil.Date, signal string) (int, error) {
	if err := m.check(ctx); err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	k := counterKey{userID, day, signal}
	v, ok := m.counter[k]
	if !ok {
		m.counter[k] = 0
	}
	return v, nil
}

// AppliedBetween implements Store.
func (m *MemStore) AppliedBetween(ctx context.Context, userID uuid.UUID, signal string, from, to civil.Date) (int, error) {
	if err := m.check(ctx); err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for k, v := range m.counter {
		if k.user == userID && k.signal == signal && !k.day.Before(from) && !k.day.After(to) {
			n += v
		}
	}
	return n, nil
}

// AddCounter implements Store.
func (m *MemStore) AddCounter(ctx context.Context, userID uuid.UUID, day civil.Date, signal string, delta int) error {
	if err := m.check(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	k := counterKey{userID, day, signal}
	if _, ok := m.counter[k]; ok {
		m.counter[k] += delta
	}
	return nil
}

// InsertPoint implements Store.
func (m *MemStore) InsertPoint(ctx context.Context, p PointEvent) (PointEvent, bool, error) {
	if err := m.check(ctx); err != nil {
		return PointEvent{}, false, err
	}
	switch {
	case (p.ReversalOf == nil) != (p.SourceType != SourceReversal):
		return PointEvent{}, false, errMemCheck
	case p.Capped != (p.CapReason != ""):
		return PointEvent{}, false, errMemCheck
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, o := range m.points {
		if o.Key() == p.Key() || (p.ReversalOf != nil && o.ReversalOf != nil && *o.ReversalOf == *p.ReversalOf) {
			return PointEvent{}, false, nil
		}
	}
	p.CreatedAt = m.stamp()
	m.points = append(m.points, p)
	return p, true, nil
}

// FindPoint implements Store.
func (m *MemStore) FindPoint(ctx context.Context, k SourceKey) (PointEvent, error) {
	if err := m.check(ctx); err != nil {
		return PointEvent{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.points {
		if p.Key() == k {
			return p, nil
		}
	}
	return PointEvent{}, ErrNotFound
}

// CountCredited implements Store.
func (m *MemStore) CountCredited(ctx context.Context, userID uuid.UUID, signal string) (int, error) {
	if err := m.check(ctx); err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	reversed := map[uuid.UUID]bool{}
	for _, p := range m.points {
		if p.ReversalOf != nil {
			reversed[*p.ReversalOf] = true
		}
	}
	n := 0
	for _, p := range m.points {
		if p.UserID == userID && p.Signal == signal && p.PointsCP > 0 && p.ReversalOf == nil && !reversed[p.ID] {
			n++
		}
	}
	return n, nil
}

// Points implements Store.
func (m *MemStore) Points(ctx context.Context, userID uuid.UUID) ([]PointEvent, error) {
	if err := m.check(ctx); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []PointEvent
	for _, p := range m.points {
		if p.UserID == userID {
			out = append(out, p)
		}
	}
	slices.SortStableFunc(out, func(a, b PointEvent) int { return a.OccurredAt.Compare(b.OccurredAt) })
	return out, nil
}

// GetPoint implements Store.
func (m *MemStore) GetPoint(ctx context.Context, id uuid.UUID) (PointEvent, error) {
	if err := m.check(ctx); err != nil {
		return PointEvent{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.points {
		if p.ID == id {
			return p, nil
		}
	}
	return PointEvent{}, ErrNotFound
}

// PointsSince implements Store.
func (m *MemStore) PointsSince(ctx context.Context, userIDs []uuid.UUID, since time.Time) ([]PointEvent, error) {
	if len(userIDs) == 0 {
		return nil, nil
	}
	if err := m.check(ctx); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []PointEvent
	for _, p := range m.points {
		if slices.Contains(userIDs, p.UserID) && p.OccurredAt.After(since) {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b PointEvent) int {
		if c := bytes.Compare(a.UserID[:], b.UserID[:]); c != 0 {
			return c
		}
		if c := a.OccurredAt.Compare(b.OccurredAt); c != 0 {
			return c
		}
		return bytes.Compare(a.ID[:], b.ID[:])
	})
	return out, nil
}

// Neutralizations implements Store.
func (m *MemStore) Neutralizations(ctx context.Context, userID uuid.UUID) ([]Neutralization, error) {
	if err := m.check(ctx); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Neutralization
	for _, n := range m.neutral {
		if n.UserID == userID {
			out = append(out, n)
		}
	}
	return out, nil
}

// GetNeutralization implements Store.
func (m *MemStore) GetNeutralization(ctx context.Context, id uuid.UUID) (Neutralization, error) {
	if err := m.check(ctx); err != nil {
		return Neutralization{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, n := range m.neutral {
		if n.ID == id {
			return n, nil
		}
	}
	return Neutralization{}, ErrNotFound
}

// InsertNeutralization implements Store.
func (m *MemStore) InsertNeutralization(ctx context.Context, n Neutralization) (Neutralization, bool, error) {
	if err := m.check(ctx); err != nil {
		return Neutralization{}, false, err
	}
	if !n.Reason.Valid() || (n.Reason == NeutralizeObjection && n.DSRID == nil) {
		return Neutralization{}, false, errMemCheck
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, o := range m.neutral {
		if o.UserID == n.UserID && o.Reason == n.Reason && o.Active() {
			return o, false, nil
		}
	}
	n.CreatedAt, n.LiftedAt, n.LiftedBy = m.stamp(), nil, nil
	m.neutral = append(m.neutral, n)
	return n, true, nil
}

// LiftNeutralization implements Store.
func (m *MemStore) LiftNeutralization(ctx context.Context, id uuid.UUID, by *uuid.UUID, at time.Time) (Neutralization, bool, error) {
	if err := m.check(ctx); err != nil {
		return Neutralization{}, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, n := range m.neutral {
		if n.ID != id {
			continue
		}
		if !n.Active() {
			return n, false, nil
		}
		t := at
		m.neutral[i].LiftedAt, m.neutral[i].LiftedBy = &t, by
		return m.neutral[i], true, nil
	}
	return Neutralization{}, false, ErrNotFound
}

// Bands implements Store.
func (m *MemStore) Bands(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]Band, error) {
	out := map[uuid.UUID]Band{}
	if len(userIDs) == 0 {
		return out, nil
	}
	if err := m.check(ctx); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range userIDs {
		sc, ok := m.scores[id]
		if !ok || sc.Band == BandNeutral || sc.Neutralized || m.activeLocked(id) {
			continue
		}
		out[id] = sc.Band
	}
	return out, nil
}

func (m *MemStore) activeLocked(userID uuid.UUID) bool {
	for _, n := range m.neutral {
		if n.UserID == userID && n.Active() {
			return true
		}
	}
	return false
}

// StoredBand implements Store.
func (m *MemStore) StoredBand(ctx context.Context, userID uuid.UUID) (Band, bool, error) {
	if err := m.check(ctx); err != nil {
		return 0, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	sc, ok := m.scores[userID]
	return sc.Band, ok, nil
}

// Score implements Store.
func (m *MemStore) Score(ctx context.Context, userID uuid.UUID) (Score, bool, error) {
	if err := m.check(ctx); err != nil {
		return Score{}, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	sc, ok := m.scores[userID]
	sc.TopContributors = slices.Clone(sc.TopContributors)
	return sc, ok, nil
}

// BandCounts implements Store.
func (m *MemStore) BandCounts(ctx context.Context, cohortPrefix string) (map[int]int, error) {
	if err := m.check(ctx); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[int]int{}
	for id, sc := range m.scores {
		if sc.Neutralized || m.activeLocked(id) {
			continue
		}
		if cohortPrefix != "" && sc.CohortKey != cohortPrefix && !strings.HasPrefix(sc.CohortKey, cohortPrefix+":") {
			continue
		}
		out[int(sc.Band)]++
	}
	return out, nil
}

// ScoringUsers implements Store.
func (m *MemStore) ScoringUsers(ctx context.Context, since time.Time) ([]uuid.UUID, error) {
	if err := m.check(ctx); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	set := map[uuid.UUID]bool{}
	for _, p := range m.points {
		if p.OccurredAt.After(since) {
			set[p.UserID] = true
		}
	}
	for id := range m.scores {
		set[id] = true
	}
	for id := range m.cohorts {
		set[id] = true
	}
	out := slices.Collect(maps.Keys(set))
	slices.SortFunc(out, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	return out, nil
}

// ActiveNeutralized implements Store.
func (m *MemStore) ActiveNeutralized(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]bool, error) {
	out := map[uuid.UUID]bool{}
	if len(userIDs) == 0 {
		return out, nil
	}
	if err := m.check(ctx); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range userIDs {
		if m.activeLocked(id) {
			out[id] = true
		}
	}
	return out, nil
}

// InsertRun implements Store.
func (m *MemStore) InsertRun(ctx context.Context, r ScoreRun) error {
	if err := m.check(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r.Status, r.FinishedAt, r.UsersProcessed, r.Error = RunRunning, nil, 0, ""
	m.runs = append(m.runs, r)
	return nil
}

// FinishRun implements Store.
func (m *MemStore) FinishRun(ctx context.Context, r ScoreRun) error {
	if err := m.check(ctx); err != nil {
		return err
	}
	if r.Status != RunRunning && r.Status != RunSucceeded && r.Status != RunFailed || len([]rune(r.Error)) > 2000 {
		return errMemCheck
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.runs {
		if m.runs[i].ID == r.ID {
			m.runs[i].FinishedAt, m.runs[i].UsersProcessed, m.runs[i].Status, m.runs[i].Error = r.FinishedAt, r.UsersProcessed, r.Status, r.Error
			return nil
		}
	}
	return errors.New("talent: finish run: no such run (memory store)")
}

// PutScores implements Store.
func (m *MemStore) PutScores(ctx context.Context, scores []Score) error {
	if err := m.check(ctx); err != nil {
		return err
	}
	for _, sc := range scores {
		switch {
		case sc.RawCP < 0, sc.PctBP < 0, sc.PctBP > 10000, sc.Band > BandMax, sc.NSignals < 0,
			sc.Neutralized && sc.Band != BandNeutral, !cohortKeyRe.MatchString(sc.CohortKey):
			return errMemCheck
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, sc := range scores {
		if sc.TopContributors == nil {
			sc.TopContributors = []Contribution{}
		}
		sc.TopContributors = slices.Clone(sc.TopContributors)
		m.scores[sc.UserID] = sc
		m.cohorts[sc.UserID] = sc.CohortKey
	}
	return nil
}

// DeleteScores implements Store.
func (m *MemStore) DeleteScores(ctx context.Context, userIDs []uuid.UUID) (int, error) {
	if len(userIDs) == 0 {
		return 0, nil
	}
	if err := m.check(ctx); err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, id := range userIDs {
		if _, ok := m.scores[id]; ok {
			delete(m.scores, id)
			n++
		}
		if _, ok := m.cohorts[id]; ok {
			delete(m.cohorts, id)
			n++
		}
	}
	return n, nil
}

// HasData implements Store.
func (m *MemStore) HasData(ctx context.Context, userID uuid.UUID) (bool, error) {
	if err := m.check(ctx); err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, scored := m.scores[userID]
	if _, ok := m.cohorts[userID]; ok || scored {
		return true, nil
	}
	for _, p := range m.points {
		if p.UserID == userID {
			return true, nil
		}
	}
	for k := range m.counter {
		if k.user == userID {
			return true, nil
		}
	}
	for _, n := range m.neutral {
		if n.UserID == userID {
			return true, nil
		}
	}
	return false, nil
}

// EraseUser implements Store.
func (m *MemStore) EraseUser(ctx context.Context, userID uuid.UUID) (int, error) {
	if err := m.check(ctx); err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ledger := slices.ContainsFunc(m.points, func(p PointEvent) bool { return p.UserID == userID })
	if ledger && m.ErasedUsers != nil && !m.ErasedUsers[userID] {
		return 0, errNoErasure // 55000 in PostgreSQL: the transaction rolls back
	}
	n := 0
	for k := range m.counter {
		if k.user == userID {
			delete(m.counter, k)
			n++
		}
	}
	if _, ok := m.scores[userID]; ok {
		delete(m.scores, userID)
		n++
	}
	if _, ok := m.cohorts[userID]; ok {
		delete(m.cohorts, userID)
		n++
	}
	before := len(m.neutral)
	m.neutral = slices.DeleteFunc(m.neutral, func(x Neutralization) bool { return x.UserID == userID })
	n += before - len(m.neutral)
	before = len(m.points)
	m.points = slices.DeleteFunc(m.points, func(p PointEvent) bool { return p.UserID == userID })
	return n + before - len(m.points), nil
}

// stamp is created_at: the store's clock in the database's precision.
func (m *MemStore) stamp() time.Time { return m.now().UTC().Truncate(time.Microsecond) }

// cohortKeyRe is the cohorts_cohort_key_chk CHECK (000084).
var cohortKeyRe = regexp.MustCompile(`^(uni:[0-9a-f-]{36}(:year:[1-8])?|global)$`)

// Errors the memory store reports where PostgreSQL raises (db.Translate
// maps a unique violation to 409, a check violation to 422).
var (
	errMemUnique = problem.ErrConflict.Wrap(errors.New("talent: unique violation (memory store)"))
	errMemCheck  = problem.ErrValidation.Wrap(errors.New("talent: check violation (memory store)"))
	errNoErasure = errors.New("talent: point_events_erase_user: no completed erasure request for the user")
)
