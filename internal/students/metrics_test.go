package students_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"koro/internal/platform/obs"
)

// TestHTTP_ProfileReadsCounted (BE-167, uyum #39): the company's profile
// read, CV download and search rows count in koro_profile_reads_total
// under the company's bucket; a refused read (404) counts nothing; the
// scrape names neither the company nor the student.
func TestHTTP_ProfileReadsCounted(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	m := obs.NewMetrics()
	app := newHTTPMetrics(t, e, m)
	org, rec, _ := e.company("Acme")
	s, _ := e.granted(t, org)
	e.withCV(t, s)

	base := "/api/v1/orgs/" + org.String() + "/students/"
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, base + s.UserID.String(), ""},
		{http.MethodGet, base + s.UserID.String(), ""},
		{http.MethodGet, base + s.UserID.String() + "/cv", ""},
		{http.MethodPost, base + "search", `{}`},
		{http.MethodPost, base + "search", `{"q":"zeynep"}`}, // no row: nothing counted
	} {
		if r := e.send(t, app, rec, c.method, c.path, c.body); r.Code != http.StatusOK {
			t.Fatalf("%s %s = %d %s", c.method, c.path, r.Code, r.Body)
		}
	}
	if r := e.send(t, app, rec, http.MethodGet, base+e.profiled(t).UserID.String(), ""); r.Code != http.StatusNotFound {
		t.Fatalf("a student without a grant = %d", r.Code)
	}

	scrape := httptest.NewRecorder()
	m.Handler().ServeHTTP(scrape, httptest.NewRequestWithContext(t.Context(), http.MethodGet, obs.MetricsPath, nil))
	b, _ := io.ReadAll(scrape.Body)
	body := string(b)
	bucket := obs.ReadBucket(org)
	for _, want := range []string{
		`koro_profile_reads_total{bucket="` + bucket + `",kind="company_profile"} 2`,
		`koro_profile_reads_total{bucket="` + bucket + `",kind="company_cv"} 1`,
		`koro_profile_reads_total{bucket="` + bucket + `",kind="company_search"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("scrape lacks %q:\n%s", want, body)
		}
	}
	if n := strings.Count(body, "koro_profile_reads_total{"); n != 3 {
		t.Fatalf("%d series, want 3", n)
	}
	if strings.Contains(body, org.String()) || strings.Contains(body, s.UserID.String()) {
		t.Fatal("the scrape names the company or the student")
	}
}
