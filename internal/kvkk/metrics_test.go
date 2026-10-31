package kvkk_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"koro/internal/authz"
	"koro/internal/kvkk"
	"koro/internal/platform/civil"
	"koro/internal/platform/clock"
)

func date(t *testing.T, s string) civil.Date {
	t.Helper()
	d, err := civil.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestProductMetrics_PeriodDefaultsAndBounds (BE-164): the default period
// is the last 30 Europe/Istanbul days up to today, both inclusive; the
// store is asked for [from 00:00, to+1 00:00) in Istanbul and the stock
// metrics at the read; invalid periods are kvkk.invalid_period.
func TestProductMetrics_PeriodDefaultsAndBounds(t *testing.T) {
	t.Parallel()
	e := newEnv(t) // clock: 2026-10-03 09:00 UTC = 12:00 Istanbul
	admin := actor(t, "platform_admin")
	r, err := e.svc.ProductMetrics(t.Context(), admin, kvkk.MetricsQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if r.From.String() != "2026-09-04" || r.To.String() != "2026-10-03" || !r.AsOf.Equal(e.clk.Now()) {
		t.Fatalf("default period %s..%s as of %s", r.From, r.To, r.AsOf)
	}
	calls := e.metrics.Calls()
	ist := clock.Istanbul()
	want := []kvkk.MetricsCall{
		{Kind: "period", From: time.Date(2026, 9, 4, 0, 0, 0, 0, ist), To: time.Date(2026, 10, 4, 0, 0, 0, 0, ist)},
		{Kind: "current", From: e.clk.Now()},
	}
	if len(calls) != 2 || calls[0].Kind != want[0].Kind || !calls[0].From.Equal(want[0].From) || !calls[0].To.Equal(want[0].To) ||
		calls[1].Kind != "current" || !calls[1].From.Equal(want[1].From) {
		t.Fatalf("store calls = %+v, want %+v", calls, want)
	}
	codes := func(ms []kvkk.Metric) []string {
		out := make([]string, len(ms))
		for i, m := range ms {
			out[i] = m.Code
		}
		return out
	}
	defCodes := func(ds []kvkk.MetricDef) []string {
		out := make([]string, len(ds))
		for i, d := range ds {
			out[i] = d.Code
		}
		return out
	}
	if !slices.Equal(codes(r.Period), defCodes(kvkk.PeriodMetrics)) || !slices.Equal(codes(r.Current), defCodes(kvkk.CurrentMetrics)) {
		t.Fatalf("metrics %v / %v", codes(r.Period), codes(r.Current))
	}

	// Only to: from follows it; a single day is a valid period.
	r, err = e.svc.ProductMetrics(t.Context(), admin, kvkk.MetricsQuery{To: date(t, "2026-06-30")})
	if err != nil || r.From.String() != "2026-06-01" {
		t.Fatalf("to only: %s (%v)", r.From, err)
	}
	if _, err := e.svc.ProductMetrics(t.Context(), admin, kvkk.MetricsQuery{From: date(t, "2026-10-03"), To: date(t, "2026-10-03")}); err != nil {
		t.Fatalf("one day: %v", err)
	}
	// 366 days is the longest period.
	if _, err := e.svc.ProductMetrics(t.Context(), admin, kvkk.MetricsQuery{From: date(t, "2025-10-03"), To: date(t, "2026-10-03")}); err != nil {
		t.Fatalf("366 days: %v", err)
	}
	for name, q := range map[string]kvkk.MetricsQuery{
		"from after to":  {From: date(t, "2026-10-02"), To: date(t, "2026-10-01")},
		"367 days":       {From: date(t, "2025-10-02"), To: date(t, "2026-10-03")},
		"to after today": {To: date(t, "2026-10-04")},
		"from in future": {From: date(t, "2026-10-05")},
	} {
		if _, err := e.svc.ProductMetrics(t.Context(), admin, q); !errors.Is(err, kvkk.ErrInvalidPeriod) {
			t.Errorf("%s: err = %v, want kvkk.invalid_period", name, err)
		}
	}
}

// TestProductMetrics_OnlyPlatformAdmin (BE-164; 03 §5.22 kvkk.metrics.read):
// the DPO and other staff are refused before the store is read.
func TestProductMetrics_OnlyPlatformAdmin(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, name := range []string{"dpo", "finance_admin", "moderator", "support", "student", "company_admin"} {
		if _, err := e.svc.ProductMetrics(t.Context(), actor(t, name), kvkk.MetricsQuery{}); !errors.Is(err, authz.ErrForbidden) {
			t.Errorf("%s: err = %v, want forbidden", name, err)
		}
	}
	if n := len(e.metrics.Calls()); n != 0 {
		t.Fatalf("store read %d times for refused callers", n)
	}
}

// TestMetricsSnapshot_StoresTodaysStockMetrics (BE-164): the nightly
// kvkk.metrics_snapshot stores the stock metrics under the Istanbul day.
func TestMetricsSnapshot_StoresTodaysStockMetrics(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	n := int64(42)
	e.metrics.SetCurrent([]kvkk.Metric{{Code: kvkk.MetricVerifiedClubs, Unit: kvkk.UnitCount, Numerator: &n}})
	var job func() error
	for _, j := range e.mod.Jobs() {
		if j.Name == kvkk.MetricsSnapshotJobName {
			job = func() error { return j.Handle(workerCtx(t)) }
		}
	}
	if job == nil {
		t.Fatal("no kvkk.metrics_snapshot job")
	}
	if err := job(); err != nil {
		t.Fatal(err)
	}
	got := e.metrics.SnapshotOf(date(t, "2026-10-03"))
	if len(got) != 1 || got[0].Code != kvkk.MetricVerifiedClubs || *got[0].Numerator != 42 {
		t.Fatalf("snapshot = %+v", got)
	}
}
