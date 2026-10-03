package obs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// DefaultSampleRatio is the share of root traces sampled (05 §11.3).
const DefaultSampleRatio = 0.1

const (
	metricsReadHeaderTimeout = 5 * time.Second
	metricsWriteTimeout      = 30 * time.Second
)

// TelemetryOptions configures OpenTelemetry tracing and the Prometheus
// endpoint (05 §11.3–11.4). Empty values mean disabled.
type TelemetryOptions struct {
	OTLPEndpoint string  // OTEL_EXPORTER_OTLP_ENDPOINT (base URL; /v1/traces is appended)
	MetricsAddr  string  // METRICS_ADDR (internal listener, never published)
	SampleRatio  float64 // OTEL_TRACES_SAMPLER_ARG; 0 means DefaultSampleRatio
	Service      string
	Version      string
	Env          string
	Log          *slog.Logger // exporter and listener errors; nil discards
}

// Telemetry is the process telemetry. With empty options nothing is
// installed: Metrics is nil, Tracing is false, TracerProvider is a no-op and
// callers add no middleware or plugin (no overhead, unchanged behaviour).
type Telemetry struct {
	// Metrics is the Prometheus registry; nil when METRICS_ADDR is empty.
	Metrics *Metrics
	tp      *sdktrace.TracerProvider // nil when tracing is off
	srv     *http.Server             // metrics listener; nil when off
	addr    string                   // bound metrics address
	log     *slog.Logger
}

// SetupTelemetry installs the exporters. Tracing (OTLP/HTTP, batch span
// processor, ParentBased(TraceIDRatioBased) sampler, TraceContext+Baggage
// propagator) becomes the global OpenTelemetry provider so that module code
// can start spans with otel.Tracer. Every exported span passes through the
// PII filter (filterExporter). The metrics listener serves only GET
// /metrics on MetricsAddr; it is never part of the public router.
func SetupTelemetry(ctx context.Context, o TelemetryOptions) (*Telemetry, error) {
	t := &Telemetry{log: o.Log}
	if t.log == nil {
		t.log = Discard()
	}
	if o.OTLPEndpoint != "" {
		exp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(tracesURL(o.OTLPEndpoint)))
		if err != nil {
			return nil, fmt.Errorf("obs: otlp exporter: %w", err)
		}
		t.tp = newTracerProvider(o, exp)
		otel.SetTracerProvider(t.tp)
		otel.SetTextMapPropagator(Propagator())
		otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
			t.log.LogAttrs(ctx, slog.LevelWarn, "otel export failed", slog.String("err", Scrub(err.Error())))
		}))
	}
	if o.MetricsAddr != "" {
		t.Metrics = NewMetrics()
		if err := t.serveMetrics(ctx, o.MetricsAddr); err != nil {
			return nil, errors.Join(err, t.shutdownTracing(ctx))
		}
	}
	return t, nil
}

// Tracing reports whether spans are exported.
func (t *Telemetry) Tracing() bool { return t != nil && t.tp != nil }

// TracerProvider returns the exporting provider, or a no-op one when
// tracing is off.
func (t *Telemetry) TracerProvider() trace.TracerProvider {
	if !t.Tracing() {
		return noop.NewTracerProvider()
	}
	return t.tp
}

// MetricsAddr returns the bound address of the metrics listener ("" when
// off); with METRICS_ADDR=host:0 it reveals the chosen port.
func (t *Telemetry) MetricsAddr() string {
	if t == nil {
		return ""
	}
	return t.addr
}

// Shutdown stops the metrics listener and flushes pending spans. It is
// called after HTTP has drained (05 §10.9).
func (t *Telemetry) Shutdown(ctx context.Context) error {
	if t == nil {
		return nil
	}
	var errs []error
	if t.srv != nil {
		if err := t.srv.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("obs: metrics listener: %w", err))
		}
	}
	return errors.Join(append(errs, t.shutdownTracing(ctx))...)
}

func (t *Telemetry) shutdownTracing(ctx context.Context) error {
	if t.tp == nil {
		return nil
	}
	if err := t.tp.Shutdown(ctx); err != nil {
		return fmt.Errorf("obs: flush spans: %w", err)
	}
	return nil
}

// serveMetrics listens on addr and serves GET /metrics only.
func (t *Telemetry) serveMetrics(ctx context.Context, addr string) error {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("obs: metrics listen %s: %w", addr, err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET "+MetricsPath, t.Metrics.Handler())
	t.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: metricsReadHeaderTimeout,
		WriteTimeout:      metricsWriteTimeout,
		BaseContext:       func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}
	t.addr = ln.Addr().String()
	go func() {
		if err := t.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.log.LogAttrs(ctx, slog.LevelError, "metrics listener stopped", slog.String("err", err.Error()))
		}
	}()
	t.log.LogAttrs(ctx, slog.LevelInfo, "metrics listening", slog.String("addr", t.addr))
	return nil
}

// Propagator is the W3C TraceContext + Baggage propagator (05 §11.3).
func Propagator() propagation.TextMapPropagator {
	return propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})
}

func newTracerProvider(o TelemetryOptions, exp sdktrace.SpanExporter) *sdktrace.TracerProvider {
	ratio := o.SampleRatio
	if ratio <= 0 {
		ratio = DefaultSampleRatio
	}
	attrs := []attribute.KeyValue{attribute.String("service.name", o.Service)}
	if o.Version != "" {
		attrs = append(attrs, attribute.String("service.version", o.Version))
	}
	if o.Env != "" {
		attrs = append(attrs, attribute.String("deployment.environment.name", o.Env))
	}
	return sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(filterExporter{exp}),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))),
		sdktrace.WithResource(resource.NewSchemaless(attrs...)),
	)
}

// tracesURL turns the OTLP base endpoint into the traces URL, as the SDK
// does for OTEL_EXPORTER_OTLP_ENDPOINT.
func tracesURL(base string) string { return strings.TrimSuffix(base, "/") + "/v1/traces" }

// droppedAttrs are span attributes that carry personal data or raw request
// data (KVKK; 05 §11.3). otelgin records them; http.route (the template)
// stays.
var droppedAttrs = map[attribute.Key]bool{
	"url.path": true, "url.full": true, "url.query": true, "http.target": true, "http.url": true,
	"client.address": true, "http.client_ip": true, "net.peer.ip": true, "net.sock.peer.addr": true,
	"network.peer.address": true, "network.peer.port": true, "user_agent.original": true, "http.user_agent": true,
	"enduser.name": true, "user.email": true, "user.name": true, "user.full_name": true,
}

// filterExporter removes droppedAttrs and scrubs every remaining string
// (name, attributes, events, status) before export: the last line of
// defence, whatever an instrumentation library records.
type filterExporter struct{ sdktrace.SpanExporter }

func (e filterExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	out := make([]sdktrace.ReadOnlySpan, len(spans))
	for i, s := range spans {
		out[i] = filteredSpan{s}
	}
	if err := e.SpanExporter.ExportSpans(ctx, out); err != nil {
		return fmt.Errorf("obs: export spans: %w", err)
	}
	return nil
}

type filteredSpan struct{ sdktrace.ReadOnlySpan }

func (s filteredSpan) Name() string { return Scrub(s.ReadOnlySpan.Name()) }

func (s filteredSpan) Attributes() []attribute.KeyValue {
	return filterAttrs(s.ReadOnlySpan.Attributes())
}

func (s filteredSpan) Events() []sdktrace.Event {
	ev := s.ReadOnlySpan.Events()
	out := make([]sdktrace.Event, len(ev))
	for i, e := range ev {
		e.Attributes = filterAttrs(e.Attributes)
		e.Name = Scrub(e.Name)
		out[i] = e
	}
	return out
}

func (s filteredSpan) Status() sdktrace.Status {
	st := s.ReadOnlySpan.Status()
	st.Description = Scrub(st.Description)
	return st
}

func filterAttrs(in []attribute.KeyValue) []attribute.KeyValue {
	out := make([]attribute.KeyValue, 0, len(in))
	for _, kv := range in {
		if droppedAttrs[kv.Key] {
			continue
		}
		if kv.Value.Type() == attribute.STRING {
			kv = kv.Key.String(Scrub(kv.Value.AsString()))
		}
		out = append(out, kv)
	}
	return out
}
