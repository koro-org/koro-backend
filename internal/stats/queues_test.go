package stats_test

import (
	"errors"
	"net/http"
	"testing"

	"koro/internal/authz"
	"koro/internal/feed"
	"koro/internal/orgs"
	"koro/internal/stats"
)

// queueFigures fills the fake with one distinct size per queue.
func queueFigures(f *fake) {
	f.orgStats = orgs.OrgStats{Counts: []orgs.OrgCount{
		{Kind: orgs.KindClub, Status: orgs.StatusPendingReview, Count: 1},
		{Kind: orgs.KindCompany, Status: orgs.StatusActive, Count: 9},
		{Kind: orgs.KindCompany, Status: orgs.StatusPendingReview, Count: 2}}, TaxIDReviews: 6}
	f.content = feed.ContentStats{PostsByStatus: map[feed.Status]int{feed.StatusPublished: 40},
		ReportsByStatus: map[feed.ReportStatus]int{feed.ReportOpen: 4, feed.ReportInReview: 1, feed.ReportActioned: 6}}
	f.claims, f.dsrs, f.docs = 5, 7, 8
}

// counts reads a QueueStats as name → size (-1: null).
func counts(q stats.QueueStats) map[string]int {
	out := map[string]int{}
	for name, p := range map[string]*int{"org_reviews": q.OrgReviews, "tax_id_reviews": q.TaxIDReviews, "club_claims": q.ClubClaims,
		"content_reports": q.ContentReports, "kvkk_requests": q.KvkkRequests, "student_documents": q.StudentDocuments} {
		out[name] = -1
		if p != nil {
			out[name] = *p
		}
	}
	return out
}

// TestQueues_EachCounterOnlyForTheRolesThatWorkTheQueue (BE-196, 03 k37;
// BE-206 tax_id_reviews behind orgs.org.list_admin like org_reviews):
// every platform role reads the queue sizes; each counter is the queue's
// size for a role its own list action admits (orgs.org.list_admin,
// directory.club_claim.list, feed.report.list, kvkk.dsr.list,
// identity.edu_verification.list) and null for any other, and a refused
// queue's system read is not even made. Everyone without a platform role
// gets 403.
func TestQueues_EachCounterOnlyForTheRolesThatWorkTheQueue(t *testing.T) {
	t.Parallel()
	const null = -1
	for _, tc := range []struct {
		persona string
		want    map[string]int
	}{
		{"moderator", map[string]int{"org_reviews": 3, "tax_id_reviews": 6, "club_claims": null, "content_reports": 5, "kvkk_requests": null, "student_documents": null}},
		{"support", map[string]int{"org_reviews": 3, "tax_id_reviews": 6, "club_claims": 5, "content_reports": null, "kvkk_requests": null, "student_documents": 8}},
		{"dpo", map[string]int{"org_reviews": null, "tax_id_reviews": null, "club_claims": null, "content_reports": null, "kvkk_requests": 7, "student_documents": null}},
		{"platform_admin", map[string]int{"org_reviews": 3, "tax_id_reviews": 6, "club_claims": 5, "content_reports": 5, "kvkk_requests": 7, "student_documents": 8}},
		{"finance_admin", map[string]int{"org_reviews": null, "tax_id_reviews": null, "club_claims": null, "content_reports": null, "kvkk_requests": null, "student_documents": null}},
		{"talent_auditor", map[string]int{"org_reviews": null, "tax_id_reviews": null, "club_claims": null, "content_reports": null, "kvkk_requests": null, "student_documents": null}},
	} {
		f, svc := world(t)
		queueFigures(f)
		q, err := svc.Queues(t.Context(), persona(t, tc.persona))
		if err != nil {
			t.Fatalf("%s: %v", tc.persona, err)
		}
		got := counts(q)
		for name, want := range tc.want {
			if got[name] != want {
				t.Errorf("%s: %s = %d, want %d (-1: null)", tc.persona, name, got[name], want)
			}
		}
		for port, queue := range map[string]string{"OrgStats": "org_reviews", "PendingClaims": "club_claims", "ContentStats": "content_reports"} {
			if read := f.reads[port] > 0; read != (tc.want[queue] != null) {
				t.Errorf("%s: %s read = %v for %s = %d", tc.persona, port, read, queue, tc.want[queue])
			}
		}
	}
	_, svc := world(t)
	for _, p := range []string{"anonymous", "student", "club_admin", "university_rep", "company_admin", "other_company_member"} {
		_, err := svc.Queues(t.Context(), persona(t, p))
		wantStatus(t, err, http.StatusForbidden)
	}
}

// TestQueues_SameFiguresAsThePlatformPanel: for platform_admin the queue
// sizes are the queues of getAdminStats, computed by the same rules.
func TestQueues_SameFiguresAsThePlatformPanel(t *testing.T) {
	t.Parallel()
	pad := persona(t, "platform_admin")
	f, svc := world(t)
	queueFigures(f)
	q, err := svc.Queues(t.Context(), pad)
	if err != nil {
		t.Fatal(err)
	}
	st, err := svc.Platform(t.Context(), pad, stats.RangeQuery{})
	if err != nil {
		t.Fatal(err)
	}
	pq := st.Queues
	if pq.KvkkRequests == nil || pq.StudentDocuments == nil {
		t.Fatalf("platform queues = %+v", pq)
	}
	want := map[string]int{"org_reviews": pq.OrgReviews, "tax_id_reviews": pq.TaxIDReviews, "club_claims": pq.ClubClaims,
		"content_reports": pq.ContentReports,
		"kvkk_requests":   *pq.KvkkRequests, "student_documents": *pq.StudentDocuments}
	got := counts(q)
	for name, n := range want {
		if got[name] != n {
			t.Errorf("%s: queues %d, platform panel %d", name, got[name], n)
		}
	}
}

// TestQueues_RefusalIsNullFailureFails: a module refusing the caller
// (403/404) leaves its counter null, an MFA refusal or any other failure
// of a count fails the answer (no partial queue card).
func TestQueues_RefusalIsNullFailureFails(t *testing.T) {
	t.Parallel()
	pad := persona(t, "platform_admin")
	f, svc := world(t)
	queueFigures(f)
	f.dsrErr, f.docsErr = authz.ErrNotFound, authz.ErrForbidden
	q, err := svc.Queues(t.Context(), pad)
	if err != nil {
		t.Fatal(err)
	}
	if q.KvkkRequests != nil || q.StudentDocuments != nil || q.ClubClaims == nil {
		t.Fatalf("queues = %+v, want the refused two null", q)
	}
	f.dsrErr, f.docsErr = authz.ErrMFARequired, nil
	if _, err := svc.Queues(t.Context(), pad); !errors.Is(err, authz.ErrMFARequired) {
		t.Fatalf("MFA refusal: err = %v", err)
	}
	f.dsrErr, f.claimsErr = nil, errors.New("database down")
	if _, err := svc.Queues(t.Context(), pad); err == nil {
		t.Fatal("a failing queue count did not fail the answer")
	}
}
