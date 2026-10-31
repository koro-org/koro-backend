package kvkk

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"

	"koro/internal/platform/civil"
	"koro/internal/platform/db"
)

// NewGormMetrics returns the PostgreSQL MetricsStore over the 000172
// functions (koro_metrics_period, koro_metrics_current) and
// product_metrics_daily.
func NewGormMetrics(orm *gorm.DB) MetricsStore { return gormMetrics{orm: orm} }

type gormMetrics struct{ orm *gorm.DB }

const metricCols = `metric, unit, numerator, denominator, value::float8 AS value, suppressed`

type metricRow struct {
	Metric      string
	Unit        string
	Numerator   *int64
	Denominator *int64
	Value       *float64
	Suppressed  bool
}

func (g gormMetrics) read(ctx context.Context, what, sql string, args ...any) ([]Metric, error) {
	var rows []metricRow
	if err := db.Conn(ctx, g.orm).Raw(sql, args...).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("kvkk: read %s metrics: %w", what, err)
	}
	out := make([]Metric, len(rows))
	for i, r := range rows {
		out[i] = Metric{Code: r.Metric, Unit: MetricUnit(r.Unit), Numerator: r.Numerator, Denominator: r.Denominator,
			Value: r.Value, Suppressed: r.Suppressed}
	}
	return out, nil
}

// Period implements MetricsStore.
func (g gormMetrics) Period(ctx context.Context, from, to time.Time) ([]Metric, error) {
	return g.read(ctx, "period", `SELECT `+metricCols+` FROM public.koro_metrics_period( ?, ?)`, from, to)
}

// Current implements MetricsStore.
func (g gormMetrics) Current(ctx context.Context, at time.Time) ([]Metric, error) {
	return g.read(ctx, "current", `SELECT `+metricCols+` FROM public.koro_metrics_current( ?)`, at)
}

// Snapshot implements MetricsStore.
func (g gormMetrics) Snapshot(ctx context.Context, day civil.Date, at time.Time) (int, error) {
	res := db.Conn(ctx, g.orm).Exec(`INSERT INTO product_metrics_daily
		(day, metric, unit, numerator, denominator, value, suppressed, taken_at)
		SELECT ?, m.metric, m.unit, m.numerator, m.denominator, m.value, m.suppressed, ? FROM public.koro_metrics_current( ?) m
		ON CONFLICT (day, metric) DO UPDATE SET unit = EXCLUDED.unit, numerator = EXCLUDED.numerator,
		  denominator = EXCLUDED.denominator, value = EXCLUDED.value, suppressed = EXCLUDED.suppressed,
		  taken_at = EXCLUDED.taken_at`, day, at, at)
	if res.Error != nil {
		return 0, fmt.Errorf("kvkk: store metrics snapshot: %w", res.Error)
	}
	return int(res.RowsAffected), nil
}
