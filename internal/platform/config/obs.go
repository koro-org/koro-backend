package config

// Obs configures telemetry (internal/platform/obs; 05 §11.3–11.4).
// Empty = disabled. Owner: BE-001 (hooks only; exporters are a future ticket).
type Obs struct {
	OTLPEndpoint string `env:"OTEL_EXPORTER_OTLP_ENDPOINT"`
	MetricsAddr  string `env:"METRICS_ADDR"`
}
