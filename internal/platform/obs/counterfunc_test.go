package obs_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"koro/internal/platform/obs"
)

// TestObs_CounterFunc_ScrapesCounterType (BE-047): a CounterFunc series is
// exposed with the counter type and its current value at every scrape; a
// nil registry ignores the call; a second registration of the name fails.
func TestObs_CounterFunc_ScrapesCounterType(t *testing.T) {
	t.Parallel()
	var nilMetrics *obs.Metrics
	if err := nilMetrics.CounterFunc("koro_x_total", "x", nil, func() []obs.Sample { return nil }); err != nil {
		t.Fatalf("nil metrics: %v", err)
	}
	m := obs.NewMetrics()
	n := 1.0
	if err := m.CounterFunc("koro_test_events_total", "Test events by outcome.", []string{"outcome"}, func() []obs.Sample {
		return []obs.Sample{{Labels: []string{"written"}, Value: n}}
	}); err != nil {
		t.Fatal(err)
	}
	scrape := func() string {
		rec := httptest.NewRecorder()
		m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, obs.MetricsPath, nil))
		b, _ := io.ReadAll(rec.Body)
		return string(b)
	}
	body := scrape()
	for _, want := range []string{"# TYPE koro_test_events_total counter", `koro_test_events_total{outcome="written"} 1`} {
		if !strings.Contains(body, want) {
			t.Fatalf("scrape lacks %q:\n%s", want, body)
		}
	}
	n = 3
	if body := scrape(); !strings.Contains(body, `koro_test_events_total{outcome="written"} 3`) {
		t.Fatalf("scrape does not read the source again:\n%s", body)
	}
	if err := m.CounterFunc("koro_test_events_total", "again", []string{"outcome"}, func() []obs.Sample { return nil }); err == nil {
		t.Fatal("a second registration of the same name succeeded")
	}
}
