package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
)

// Obs configures telemetry (internal/platform/obs; 05 §11.3–11.4).
// Empty = disabled. Owner: BE-001 (hooks), BE-058 (exporters).
type Obs struct {
	// OTLPEndpoint is the OTLP/HTTP collector base URL
	// (http://otel-collector:4318); spans go to <base>/v1/traces.
	OTLPEndpoint string `env:"OTEL_EXPORTER_OTLP_ENDPOINT"`
	// SampleRatio is the share of root traces sampled (ParentBased).
	SampleRatio float64 `env:"OTEL_TRACES_SAMPLER_ARG" envDefault:"0.1"`
	// MetricsAddr serves GET /metrics on a separate internal listener
	// (host:port). It is never routed through Traefik/Cloudflare.
	MetricsAddr string `env:"METRICS_ADDR"`
}

func (o *Obs) validate(Common) error {
	var errs []error
	if o.OTLPEndpoint != "" {
		u, err := url.Parse(o.OTLPEndpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" {
			errs = append(errs, errors.New("OTEL_EXPORTER_OTLP_ENDPOINT must be an http(s)://host:port base URL or empty"))
		}
	}
	if o.SampleRatio <= 0 || o.SampleRatio > 1 {
		errs = append(errs, fmt.Errorf("OTEL_TRACES_SAMPLER_ARG must be in (0, 1] (got %v)", o.SampleRatio))
	}
	if o.MetricsAddr != "" {
		if _, _, err := net.SplitHostPort(o.MetricsAddr); err != nil {
			errs = append(errs, fmt.Errorf("METRICS_ADDR must be host:port or empty: %w", err))
		}
	}
	return errors.Join(errs...)
}
