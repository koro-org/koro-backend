package kvkk

import (
	"context"
	"slices"
	"sync"
	"time"

	"koro/internal/platform/civil"
)

// MemMetrics is the in-memory MetricsStore of service, wiring and matrix
// tests (05 §13.2). It computes nothing: it returns every metric of the
// catalogue with unknown values unless a test set them (SetPeriod,
// SetCurrent), and records the calls and snapshots.
type MemMetrics struct {
	mu        sync.Mutex
	period    []Metric
	current   []Metric
	calls     []MetricsCall
	snapshots map[civil.Date][]Metric
}

// MetricsCall is one Period or Current call seen by MemMetrics (Current:
// From is the instant, To is zero).
type MetricsCall struct {
	Kind     string // "period" | "current"
	From, To time.Time
}

// NewMemMetrics returns a store answering the catalogue with unknown values.
func NewMemMetrics() *MemMetrics {
	return &MemMetrics{period: unknownMetrics(PeriodMetrics), current: unknownMetrics(CurrentMetrics),
		snapshots: map[civil.Date][]Metric{}}
}

func unknownMetrics(defs []MetricDef) []Metric {
	out := make([]Metric, len(defs))
	for i, d := range defs {
		out[i] = Metric{Code: d.Code, Unit: d.Unit}
	}
	return out
}

// SetPeriod replaces the flow metrics answer.
func (m *MemMetrics) SetPeriod(ms []Metric) {
	m.mu.Lock()
	m.period = slices.Clone(ms)
	m.mu.Unlock()
}

// SetCurrent replaces the stock metrics answer.
func (m *MemMetrics) SetCurrent(ms []Metric) {
	m.mu.Lock()
	m.current = slices.Clone(ms)
	m.mu.Unlock()
}

// Calls returns the Period/Current calls so far.
func (m *MemMetrics) Calls() []MetricsCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.calls)
}

// SnapshotOf returns the stored snapshot of day.
func (m *MemMetrics) SnapshotOf(day civil.Date) []Metric {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.snapshots[day])
}

// Period implements MetricsStore.
func (m *MemMetrics) Period(_ context.Context, from, to time.Time) ([]Metric, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, MetricsCall{Kind: "period", From: from, To: to})
	return slices.Clone(m.period), nil
}

// Current implements MetricsStore.
func (m *MemMetrics) Current(_ context.Context, at time.Time) ([]Metric, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, MetricsCall{Kind: "current", From: at})
	return slices.Clone(m.current), nil
}

// Snapshot implements MetricsStore.
func (m *MemMetrics) Snapshot(_ context.Context, day civil.Date, _ time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snapshots[day] = slices.Clone(m.current)
	return len(m.current), nil
}
