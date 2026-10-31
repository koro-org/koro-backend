//go:build integration

package kvkk_test

import (
	"testing"
	"time"

	"koro/internal/kvkk"
	"koro/internal/platform/civil"
	"koro/internal/platform/testutil"
)

// TestGormMetrics_MatchesCatalogue (BE-164): the SQL functions of 000172
// return exactly the metrics of kvkk.PeriodMetrics / CurrentMetrics, in
// order and with their units, and a snapshot writes one row per stock
// metric (a rerun of the day overwrites).
func TestGormMetrics_MatchesCatalogue(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	s := kvkk.NewGormMetrics(d.ORM)
	now := time.Now().UTC()
	check := func(name string, got []kvkk.Metric, err error, want []kvkk.MetricDef) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(got) != len(want) {
			t.Fatalf("%s: %d metrics, want %d", name, len(got), len(want))
		}
		for i, m := range got {
			if m.Code != want[i].Code || m.Unit != want[i].Unit || m.Suppressed {
				t.Errorf("%s[%d] = %+v, want %+v (empty world: nothing suppressed)", name, i, m, want[i])
			}
		}
	}
	got, err := s.Period(t.Context(), now.AddDate(0, 0, -30), now)
	check("Period", got, err, kvkk.PeriodMetrics)
	got, err = s.Current(t.Context(), now)
	check("Current", got, err, kvkk.CurrentMetrics)
	for range 2 {
		n, err := s.Snapshot(t.Context(), civil.Today(now), now)
		if err != nil || n != len(kvkk.CurrentMetrics) {
			t.Fatalf("Snapshot = %d, %v", n, err)
		}
	}
	var rows int
	if err := d.Pool.QueryRow(t.Context(), `SELECT count(*) FROM product_metrics_daily`).Scan(&rows); err != nil || rows != len(kvkk.CurrentMetrics) {
		t.Fatalf("product_metrics_daily rows = %d (%v)", rows, err)
	}
}
