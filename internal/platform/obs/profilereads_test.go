package obs_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"uuid"

	"koro/internal/platform/obs"
)

// TestObs_ProfileReads_BoundedLabels (BE-167, uyum #39): the reader is
// only a bucket (one hexadecimal digit, the same for the same reader, the
// 16 buckets all reachable), an unknown kind is "other", n <= 0 and a nil
// registry count nothing, and the scrape never names the reader.
func TestObs_ProfileReads_BoundedLabels(t *testing.T) {
	t.Parallel()
	var off *obs.Metrics
	off.ProfileReads(obs.ReadCompanyCV, uuid.NewV7(), 1) // must not panic

	seen := map[string]bool{}
	for range 2000 {
		b := obs.ReadBucket(uuid.NewV7())
		if len(b) != 1 || !strings.Contains("0123456789abcdef", b) {
			t.Fatalf("bucket %q", b)
		}
		seen[b] = true
	}
	if len(seen) != obs.ReadBuckets {
		t.Fatalf("%d of %d buckets reached", len(seen), obs.ReadBuckets)
	}

	m := obs.NewMetrics()
	org, staff := uuid.NewV7(), uuid.NewV7()
	same := org // a copy: the bucket depends on the bytes only
	if obs.ReadBucket(org) != obs.ReadBucket(same) {
		t.Fatal("bucket is not stable")
	}
	m.ProfileReads(obs.ReadDiscoverySearch, org, 25)
	m.ProfileReads(obs.ReadDiscoverySearch, org, 5)
	m.ProfileReads(obs.ReadCompanyCV, org, 1)
	m.ProfileReads(obs.ReadAdminProfile, staff, 1)
	m.ProfileReads("students.profile.read_company", org, 1) // not a Read* kind
	m.ProfileReads(obs.ReadCompanyProfile, org, 0)
	m.ProfileReads(obs.ReadCompanyProfile, org, -3)

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, obs.MetricsPath, nil))
	b, _ := io.ReadAll(rec.Body)
	body := string(b)
	ob, sb := obs.ReadBucket(org), obs.ReadBucket(staff)
	for _, want := range []string{
		"# TYPE koro_profile_reads_total counter",
		`koro_profile_reads_total{bucket="` + ob + `",kind="discovery_search"} 30`,
		`koro_profile_reads_total{bucket="` + ob + `",kind="company_cv"} 1`,
		`koro_profile_reads_total{bucket="` + sb + `",kind="admin_profile"} 1`,
		`koro_profile_reads_total{bucket="` + ob + `",kind="other"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("scrape lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `kind="company_profile"`) {
		t.Fatal("n <= 0 created a series")
	}
	if strings.Contains(body, org.String()) || strings.Contains(body, staff.String()) {
		t.Fatal("the scrape names a reader")
	}
}
