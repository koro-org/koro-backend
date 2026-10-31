package kvkk

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"koro/internal/authz"
	"koro/internal/platform/civil"
	"koro/internal/platform/clock"
	"koro/internal/platform/problem"
	"koro/internal/platform/queue"
)

// Product metrics (BE-164; SR §6, uyum denetimi #75). The definitions live
// once, in SQL (migration 000172: public.koro_metrics_period and
// public.koro_metrics_current): the admin API and the Metabase views
// (analytics.product_metrics_*) read the same functions, so a number never
// differs between the two. The output is aggregate only and a count of
// people between 1 and 9 never leaves the database (k ≥ 10, 01 §1.4).

// MetricUnit is how a metric's value reads.
type MetricUnit string

// Units of the metrics.
const (
	UnitCount   MetricUnit = "count"   // value = numerator
	UnitRatio   MetricUnit = "ratio"   // numerator ÷ denominator, 0..1, 4 decimals
	UnitAverage MetricUnit = "average" // numerator ÷ denominator, 2 decimals
	UnitKurus   MetricUnit = "kurus"   // numerator ÷ denominator in kuruş (TRY/100), whole
)

// Metric codes of SR §6. Flow metrics are measured over a period, stock
// metrics at an instant.
const (
	// MetricEngagedVerifiedStudents is the north star.
	MetricEngagedVerifiedStudents      = "engaged_verified_students"
	MetricEventAttendanceRate          = "event_attendance_rate"
	MetricContactRequestAcceptanceRate = "contact_request_acceptance_rate"
	MetricTargetedPostEngagementRate   = "targeted_post_engagement_rate"
	MetricPayingCompanies              = "paying_companies"
	MetricRevenuePerPayingCompany      = "revenue_per_paying_company"
	MetricPackageRenewalRate           = "package_renewal_rate"

	MetricVerifiedActiveStudents       = "verified_active_students"
	MetricDataFreshnessRate            = "data_freshness_rate"
	MetricVerifiedClubs                = "verified_clubs"
	MetricActiveMembersPerVerifiedClub = "active_members_per_verified_club"
	MetricDiscoverabilityRate          = "discoverability_rate"
)

// PeriodMetrics are the flow metrics in the order the store returns them.
var PeriodMetrics = []MetricDef{
	{MetricEngagedVerifiedStudents, UnitCount}, {MetricEventAttendanceRate, UnitRatio},
	{MetricContactRequestAcceptanceRate, UnitRatio}, {MetricTargetedPostEngagementRate, UnitRatio},
	{MetricPayingCompanies, UnitCount}, {MetricRevenuePerPayingCompany, UnitKurus}, {MetricPackageRenewalRate, UnitRatio},
}

// CurrentMetrics are the stock metrics in the order the store returns them.
var CurrentMetrics = []MetricDef{
	{MetricVerifiedActiveStudents, UnitCount}, {MetricDataFreshnessRate, UnitRatio}, {MetricVerifiedClubs, UnitCount},
	{MetricActiveMembersPerVerifiedClub, UnitAverage}, {MetricDiscoverabilityRate, UnitRatio},
}

// MetricDef names a metric and its unit.
type MetricDef struct {
	Code string
	Unit MetricUnit
}

// Metric is one computed metric. Numerator, Denominator and Value are nil
// when unknown, not applicable or suppressed (Suppressed: a count of
// people below 10).
type Metric struct {
	Code        string
	Unit        MetricUnit
	Numerator   *int64
	Denominator *int64
	Value       *float64
	Suppressed  bool
}

// MetricsQuery is the period of adminGetProductMetrics: Europe/Istanbul
// days, both inclusive. Zero values take the defaults (the last 30 days up
// to today).
type MetricsQuery struct {
	From civil.Date
	To   civil.Date
}

// MetricsReport is the answer of adminGetProductMetrics.
type MetricsReport struct {
	From, To civil.Date
	Period   []Metric // flow metrics of [From, To]
	AsOf     time.Time
	Current  []Metric // stock metrics at AsOf
}

// Period rules of adminGetProductMetrics.
const (
	DefaultMetricsDays = 30
	MaxMetricsDays     = 366
)

// ProductMetrics implements Service (adminGetProductMetrics,
// kvkk.metrics.read).
func (s *service) ProductMetrics(ctx context.Context, a authz.Actor, q MetricsQuery) (MetricsReport, error) {
	if err := authz.Can(ctx, a, authz.KvkkMetricsRead, adminSurface); err != nil {
		return MetricsReport{}, err
	}
	now := s.now()
	from, to, err := metricsPeriod(q, civil.Today(now))
	if err != nil {
		return MetricsReport{}, err
	}
	period, err := s.d.Metrics.Period(ctx, from.In(clock.Istanbul()), to.AddDays(1).In(clock.Istanbul()))
	if err != nil {
		return MetricsReport{}, err
	}
	current, err := s.d.Metrics.Current(ctx, now)
	if err != nil {
		return MetricsReport{}, err
	}
	return MetricsReport{From: from, To: to, Period: period, AsOf: now, Current: current}, nil
}

// metricsPeriod applies the defaults and the period rules.
func metricsPeriod(q MetricsQuery, today civil.Date) (from, to civil.Date, err error) {
	from, to = q.From, q.To
	if to.IsZero() {
		to = today
	}
	if from.IsZero() {
		from = to.AddDays(-(DefaultMetricsDays - 1))
	}
	switch {
	case to.After(today):
		return from, to, ErrInvalidPeriod.WithFields(problem.FieldError{Location: "query.to", Message: "must not be after today"})
	case from.After(to):
		return from, to, ErrInvalidPeriod.WithFields(problem.FieldError{Location: "query.from", Message: "must not be after to"})
	case from.AddDays(MaxMetricsDays - 1).Before(to):
		return from, to, ErrInvalidPeriod.WithFields(problem.FieldError{Location: "query.from",
			Message: fmt.Sprintf("the period is at most %d days", MaxMetricsDays)})
	}
	return from, to, nil
}

// MetricsSnapshotJobName stores the day's stock metrics in
// product_metrics_daily (daily 03:40, low), the trend Metabase reads
// through analytics.product_metrics_daily.
const MetricsSnapshotJobName = "kvkk.metrics_snapshot"

const (
	metricsSnapshotCron   = "40 3 * * *"
	metricsSnapshotUnique = 23 * time.Hour
)

func (s *service) metricsSnapshotJob() queue.Job {
	return queue.Job{Name: MetricsSnapshotJobName, Cron: metricsSnapshotCron, Queue: queue.Low, Unique: metricsSnapshotUnique,
		Handle: func(ctx context.Context) error { _, err := s.metricsSnapshot(ctx); return err }}
}

// metricsSnapshot writes the stock metrics of today (a rerun overwrites
// the day) and returns the rows written. Aggregates only: nothing to audit.
func (s *service) metricsSnapshot(ctx context.Context) (int, error) {
	now := s.now()
	n, err := s.d.Metrics.Snapshot(ctx, civil.Today(now), now)
	if err != nil {
		return 0, err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "product metrics snapshot stored", slog.Int("metrics", n))
	return n, nil
}
