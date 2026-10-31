package kvkk

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/platform/civil"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/problem"
)

// registerMetrics adds adminGetProductMetrics (BE-164; SR §6).
func registerMetrics(api huma.API, h handler) {
	op.Register(api, "kvkk", huma.Operation{
		OperationID: "adminGetProductMetrics", Method: http.MethodGet, Path: "/admin/metrics",
		Summary: "Read the product KPIs (north star and SR §6 metrics)",
		Description: "Flow metrics of the period [from, to] (Europe/Istanbul days, both inclusive; default the last 30 days " +
			"up to today; at most 366 days, not after today) and stock metrics as of the read. Aggregates only: a count of " +
			"people between 1 and 9 is null with suppressed = true (k ≥ 10), and a value built from it is null. The same SQL " +
			"definitions feed the Metabase views analytics.product_metrics_monthly, _current and _daily. Operational counts " +
			"(users, organisations, content) are not here.",
	}, op.Meta{Action: string(authz.KvkkMetricsRead), Surface: op.SurfaceAdmin}, h.productMetrics)
}

type metricsInput struct {
	From string `query:"from" format:"date" doc:"First day (Europe/Istanbul); default: to minus 29 days"`
	To   string `query:"to" format:"date" doc:"Last day, inclusive (Europe/Istanbul); default: today"`
}

// MetricDTO is one product metric.
type MetricDTO struct {
	Metric      string   `json:"metric" doc:"Metric code; see the description of adminGetProductMetrics"`
	Unit        string   `json:"unit" enum:"count,ratio,average,kurus" doc:"count: value = numerator; ratio: 0..1; average: numerator ÷ denominator; kurus: numerator ÷ denominator in kuruş"`
	Numerator   *int64   `json:"numerator" doc:"null when suppressed (1..9 people)"`
	Denominator *int64   `json:"denominator" doc:"null when not applicable or suppressed"`
	Value       *float64 `json:"value" doc:"null when an operand is null or the denominator is 0"`
	Suppressed  bool     `json:"suppressed" doc:"A count of people was below 10 and is not shown (k ≥ 10)"`
}

// ProductMetricsDTO is the body of adminGetProductMetrics.
type ProductMetricsDTO struct {
	From    string      `json:"from" format:"date"`
	To      string      `json:"to" format:"date" doc:"Inclusive"`
	Period  []MetricDTO `json:"period" doc:"Flow metrics of [from, to]: engaged_verified_students (north star), event_attendance_rate, contact_request_acceptance_rate, targeted_post_engagement_rate, paying_companies, revenue_per_paying_company, package_renewal_rate"`
	AsOf    time.Time   `json:"as_of"`
	Current []MetricDTO `json:"current" doc:"Stock metrics as of as_of: verified_active_students (30 days), data_freshness_rate (12 months), verified_clubs, active_members_per_verified_club, discoverability_rate"`
}

type metricsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         ProductMetricsDTO
}

func toMetricDTOs(ms []Metric) []MetricDTO {
	out := make([]MetricDTO, len(ms))
	for i, m := range ms {
		out[i] = MetricDTO{Metric: m.Code, Unit: string(m.Unit), Numerator: m.Numerator, Denominator: m.Denominator,
			Value: m.Value, Suppressed: m.Suppressed}
	}
	return out
}

// civilDate parses an optional YYYY-MM-DD query value.
func civilDate(raw, location string) (civil.Date, error) {
	if raw == "" {
		return civil.Date{}, nil
	}
	d, err := civil.Parse(raw)
	if err != nil {
		return civil.Date{}, problem.ErrValidation.WithFields(problem.FieldError{Location: location, Message: "must be a date (YYYY-MM-DD)"})
	}
	return d, nil
}

func (h handler) productMetrics(ctx context.Context, in *metricsInput) (*metricsOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	var q MetricsQuery
	if q.From, err = civilDate(in.From, "query.from"); err != nil {
		return nil, err
	}
	if q.To, err = civilDate(in.To, "query.to"); err != nil {
		return nil, err
	}
	r, err := h.svc.ProductMetrics(ctx, a, q)
	if err != nil {
		return nil, err
	}
	return &metricsOutput{CacheControl: "no-store", Body: ProductMetricsDTO{From: r.From.String(), To: r.To.String(),
		Period: toMetricDTOs(r.Period), AsOf: r.AsOf, Current: toMetricDTOs(r.Current)}}, nil
}
