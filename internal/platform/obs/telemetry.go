package obs

import (
	"context"
	"errors"
)

// TelemetryOptions configures OpenTelemetry tracing and the Prometheus
// endpoint (05 §11.3–11.4). Empty values mean disabled.
type TelemetryOptions struct {
	OTLPEndpoint string // OTEL_EXPORTER_OTLP_ENDPOINT
	MetricsAddr  string // METRICS_ADDR (internal port, never published)
	Service      string
	Version      string
	Env          string
}

// ShutdownFunc flushes telemetry; it is called after HTTP draining (05 §10.9).
type ShutdownFunc func(ctx context.Context) error

// ErrTelemetryNotImplemented is returned when an exporter is configured but
// not built in yet: BE-001 ships the hook only, and a configured-but-ignored
// exporter would be a silent fallback (05 §12.2).
var ErrTelemetryNotImplemented = errors.New("obs: OTLP/Prometheus exporters are not implemented yet; leave OTEL_EXPORTER_OTLP_ENDPOINT and METRICS_ADDR empty")

// SetupTelemetry installs the telemetry exporters. With empty options it is
// a no-op and returns a no-op shutdown function.
func SetupTelemetry(_ context.Context, o TelemetryOptions) (ShutdownFunc, error) {
	if o.OTLPEndpoint != "" || o.MetricsAddr != "" {
		return nil, ErrTelemetryNotImplemented
	}
	return func(context.Context) error { return nil }, nil
}
