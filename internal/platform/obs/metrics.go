package obs

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// MetricsPath is the only path of the internal metrics listener.
const MetricsPath = "/metrics"

// gaugeTimeout bounds one scrape-time gauge read (DB, Redis).
const gaugeTimeout = 2 * time.Second

// Metrics is the Prometheus registry of one process (05 §11.4): its own
// registry (the global one stays clean), Go and process collectors and the
// koro_* metrics. Label values are low-cardinality by construction: route
// templates, never raw paths; no user, org or request IDs; no e-mails.
//
// Every method is safe on a nil *Metrics (metrics off: METRICS_ADDR empty),
// so callers never branch.
type Metrics struct {
	reg          *prometheus.Registry
	httpRequests *prometheus.CounterVec
	httpDuration *prometheus.HistogramVec
	httpInFlight prometheus.Gauge
	tasks        *prometheus.CounterVec
	taskDuration *prometheus.HistogramVec
	authzDenied  *prometheus.CounterVec
	rlRejected   *prometheus.CounterVec
}

// NewMetrics returns a registry with the koro_* metrics registered.
func NewMetrics() *Metrics {
	m := &Metrics{
		reg: prometheus.NewRegistry(),
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "koro_http_requests_total", Help: "HTTP requests by method, route template and status.",
		}, []string{"method", "route", "status"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "koro_http_request_duration_seconds", Help: "HTTP request latency by method and route template.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "route"}),
		httpInFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "koro_http_in_flight_requests", Help: "HTTP requests being served.",
		}),
		tasks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "koro_queue_tasks_processed_total", Help: "Processed background tasks by queue, type and result (ok|retry|failed).",
		}, []string{"queue", "type", "result"}),
		taskDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "koro_queue_task_duration_seconds", Help: "Background task duration by queue and type.",
			Buckets: []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 300},
		}, []string{"queue", "type"}),
		authzDenied: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "koro_authz_denied_total", Help: "Authorization denials by module, action and reason (06 H-28).",
		}, []string{"module", "action", "reason"}),
		rlRejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "koro_ratelimit_rejected_total", Help: "Requests refused with 429 by their operation's rate limit rule, by rule and action (05 §11.4, BE-078).",
		}, []string{"rule", "action"}),
	}
	m.reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.httpRequests, m.httpDuration, m.httpInFlight, m.tasks, m.taskDuration, m.authzDenied, m.rlRejected,
	)
	return m
}

// Handler serves the registry in the Prometheus text format. A failing
// gauge source drops only its own samples (ContinueOnError).
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{ErrorHandling: promhttp.ContinueOnError})
}

// HTTPStarted counts an in-flight request; call the returned func when it
// is done.
func (m *Metrics) HTTPStarted() func() {
	if m == nil {
		return func() {}
	}
	m.httpInFlight.Inc()
	return m.httpInFlight.Dec
}

// ObserveHTTP records one served request. route must be a template
// (c.FullPath()) or "unmatched".
func (m *Metrics) ObserveHTTP(method, route string, status int, d time.Duration) {
	if m == nil {
		return
	}
	method = httpMethod(method)
	m.httpRequests.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
	m.httpDuration.WithLabelValues(method, route).Observe(d.Seconds())
}

// ObserveTask records one processed task; result is ok, retry or failed.
func (m *Metrics) ObserveTask(queue, typ, result string, d time.Duration) {
	if m == nil {
		return
	}
	m.tasks.WithLabelValues(queue, typ, result).Inc()
	m.taskDuration.WithLabelValues(queue, typ).Observe(d.Seconds())
}

// AuthzDenied counts one authorization denial. module and action must be
// the authz constants (bounded set) and reason one of the fixed reason
// names; the caller maps anything else to "unknown".
func (m *Metrics) AuthzDenied(module, action, reason string) {
	if m == nil {
		return
	}
	m.authzDenied.WithLabelValues(module, action, reason).Inc()
}

// RateLimitRejected counts one request refused by the rate limit rule of
// its operation. rule must be a 04 §1.9 class name and action the
// operation's authz action (bounded sets); never an actor, IP or org.
func (m *Metrics) RateLimitRejected(rule, action string) {
	if m == nil {
		return
	}
	m.rlRejected.WithLabelValues(rule, action).Inc()
}

// Sample is one gauge value with its label values (in label order).
type Sample struct {
	Labels []string
	Value  float64
}

// GaugeFunc registers a gauge read at scrape time (DB pool, queue length,
// outbox lag, backup age). fn gets a context bounded by a short timeout; an
// error drops the gauge from that scrape and is reported by the handler.
// Label values must come from a fixed set.
func (m *Metrics) GaugeFunc(name, help string, labels []string, fn func(ctx context.Context) ([]Sample, error)) error {
	if m == nil {
		return nil
	}
	c := &gaugeFunc{desc: prometheus.NewDesc(name, help, labels, nil), fn: fn}
	if err := m.reg.Register(c); err != nil {
		return fmt.Errorf("obs: register %s: %w", name, err)
	}
	return nil
}

// CounterFunc registers a counter read at scrape time from an in-process
// monotonic source (BE-047: koro_talent_ingest_total, whose totals live in
// the talent module). fn must never decrease a series during the life of
// the process; label values must come from a fixed set.
func (m *Metrics) CounterFunc(name, help string, labels []string, fn func() []Sample) error {
	if m == nil {
		return nil
	}
	c := &gaugeFunc{desc: prometheus.NewDesc(name, help, labels, nil), kind: prometheus.CounterValue,
		fn: func(context.Context) ([]Sample, error) { return fn(), nil }}
	if err := m.reg.Register(c); err != nil {
		return fmt.Errorf("obs: register %s: %w", name, err)
	}
	return nil
}

// gaugeFunc is a collector whose samples come from fn at scrape time; kind
// is GaugeValue (zero value: GaugeFunc) or CounterValue (CounterFunc).
type gaugeFunc struct {
	desc *prometheus.Desc
	kind prometheus.ValueType
	fn   func(ctx context.Context) ([]Sample, error)
}

func (g *gaugeFunc) Describe(ch chan<- *prometheus.Desc) { ch <- g.desc }

func (g *gaugeFunc) Collect(ch chan<- prometheus.Metric) {
	// prometheus.Collector has no context: the scrape is bounded here.
	ctx, cancel := context.WithTimeout(context.Background(), gaugeTimeout)
	defer cancel()
	samples, err := g.fn(ctx)
	if err != nil {
		ch <- prometheus.NewInvalidMetric(g.desc, err)
		return
	}
	for _, s := range samples {
		kind := g.kind
		if kind == 0 {
			kind = prometheus.GaugeValue
		}
		m, err := prometheus.NewConstMetric(g.desc, kind, s.Value, s.Labels...)
		if err != nil {
			m = prometheus.NewInvalidMetric(g.desc, err)
		}
		ch <- m
	}
}

// httpMethod keeps the method label bounded: unknown verbs become OTHER.
func httpMethod(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodOptions, http.MethodConnect, http.MethodTrace:
		return m
	}
	return "OTHER"
}
