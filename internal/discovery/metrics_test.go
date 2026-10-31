package discovery_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"koro/internal/discovery"
	"koro/internal/platform/obs"
)

// TestHTTP_ProfileReadsCounted (BE-167, uyum #39): a search page counts
// its rows and a profile read counts one in koro_profile_reads_total,
// under the reading company's bucket; a refused call counts nothing; the
// scrape names neither the company nor a student.
func TestHTTP_ProfileReadsCounted(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ids := e.add(12, e.uniA, nil)
	m := obs.NewMetrics()
	app := newHTTPMetrics(t, e, m)
	base := "/api/v1/orgs/" + e.companyA.String() + "/discovery/profiles"

	rec := get(t, app, "company_member", base+"?limit=5")
	var list discovery.DiscoveryProfileList
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil || len(list.Items) != 5 {
		t.Fatalf("search = %d %s", rec.Code, rec.Body)
	}
	if rec := get(t, app, "company_member", base+"/"+list.Items[0].AnonID); rec.Code != http.StatusOK {
		t.Fatalf("read = %d %s", rec.Code, rec.Body)
	}
	if rec := get(t, app, "student", base); rec.Code != http.StatusNotFound {
		t.Fatalf("outsider search = %d", rec.Code)
	}
	if rec := get(t, app, "company_member", base+"/unknown"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown pseudonym = %d", rec.Code)
	}

	scrape := httptest.NewRecorder()
	m.Handler().ServeHTTP(scrape, httptest.NewRequestWithContext(t.Context(), http.MethodGet, obs.MetricsPath, nil))
	b, _ := io.ReadAll(scrape.Body)
	body := string(b)
	bucket := obs.ReadBucket(e.companyA)
	for _, want := range []string{
		`koro_profile_reads_total{bucket="` + bucket + `",kind="discovery_search"} 5`,
		`koro_profile_reads_total{bucket="` + bucket + `",kind="discovery_profile"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("scrape lacks %q:\n%s", want, grepLines(body, "koro_profile_reads_total"))
		}
	}
	if n := strings.Count(body, "koro_profile_reads_total{"); n != 2 {
		t.Fatalf("%d series, want 2:\n%s", n, grepLines(body, "koro_profile_reads_total"))
	}
	for _, id := range append(ids, e.companyA) {
		if strings.Contains(body, id.String()) {
			t.Fatalf("the scrape names %s", id)
		}
	}
}

func grepLines(s, sub string) string {
	var out []string
	for l := range strings.Lines(s) {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "")
}
