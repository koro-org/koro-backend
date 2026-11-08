package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"koro/internal/outreach"
	"koro/internal/platform/config"
	"koro/internal/platform/obs"
)

// testTokenKeys is a CRYPTO_TOKEN_KEYS keyring of tests (one 32-byte key).
const testTokenKeys = "dev1:dG9rdG9rdG9rdG9rdG9rdG9rdG9rdG9rdG9rdG9rdG8="

// TestWire_OutreachNoiseFromTokenKeys (BE-203): without a preset, the
// offset of fresh audience estimates is keyed by CRYPTO_TOKEN_KEYS; without
// those keys outreach does not build (fail closed: never a count without
// its offset).
func TestWire_OutreachNoiseFromTokenKeys(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	c.outreach.noise = nil
	if err := c.build(); err == nil || !strings.Contains(err.Error(), "CRYPTO_TOKEN_KEYS") {
		t.Fatalf("build without token keys = %v, want a CRYPTO_TOKEN_KEYS error", err)
	}
	c = newTestContainer(t)
	c.outreach.noise = nil
	c.infra.platform.Crypto.TokenKeys = config.NewSecret(testTokenKeys)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	if c.outreach.svc == nil {
		t.Fatal("outreach service not built")
	}
}

// TestWire_OutreachCountAlarmsMetric (BE-203): the API registers
// koro_outreach_count_alarms_total{reason} with a series per reason; nil
// metrics are off.
func TestWire_OutreachCountAlarmsMetric(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	if err := c.outreach.metrics(nil); err != nil {
		t.Fatal(err)
	}
	m := obs.NewMetrics()
	if err := c.outreach.metrics(m); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, obs.MetricsPath, nil))
	for _, reason := range []string{outreach.AlarmFamily, outreach.AlarmValueFloor, outreach.AlarmBudget} {
		if want := `koro_outreach_count_alarms_total{reason="` + reason + `"} 0`; !strings.Contains(rec.Body.String(), want) {
			t.Errorf("metrics lack %s", want)
		}
	}
}
