package config_test

import (
	"strings"
	"testing"

	"koro/internal/platform/config"
)

// TestConfig_ObsSection: telemetry is off by default; a configured endpoint,
// ratio and metrics address are validated (BE-058).
func TestConfig_ObsSection(t *testing.T) {
	t.Parallel()
	cfg, err := config.LoadAPI(validAPIEnv())
	if err != nil {
		t.Fatalf("LoadAPI() error = %v", err)
	}
	if o := cfg.Obs; o.OTLPEndpoint != "" || o.MetricsAddr != "" || o.SampleRatio != 0.1 {
		t.Fatalf("obs defaults: %+v", o)
	}
	ok, err := config.LoadWorker(validAPIEnv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel-collector:4318",
		"OTEL_TRACES_SAMPLER_ARG", "1", "METRICS_ADDR", ":9090"))
	if err != nil || ok.Obs.MetricsAddr != ":9090" || ok.Obs.SampleRatio != 1 {
		t.Fatalf("valid obs: %+v, %v", ok.Obs, err)
	}
	for _, kv := range [][2]string{
		{"OTEL_EXPORTER_OTLP_ENDPOINT", "otel-collector:4318"},
		{"OTEL_EXPORTER_OTLP_ENDPOINT", "ftp://otel:21"},
		{"OTEL_TRACES_SAMPLER_ARG", "0"},
		{"OTEL_TRACES_SAMPLER_ARG", "1.5"},
		{"METRICS_ADDR", "9090"},
	} {
		if _, err := config.LoadAPI(validAPIEnv(kv[0], kv[1])); err == nil || !strings.Contains(err.Error(), kv[0]) {
			t.Errorf("%s=%q accepted (err %v)", kv[0], kv[1], err)
		}
	}
}
