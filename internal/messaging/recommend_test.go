package messaging_test

import (
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz/authztest"
	"koro/internal/feed"
	"koro/internal/messaging"
)

// TestContactedOpportunityPostIDs_OwnRequestsOnly (BE-198): the
// recommendations read the opportunity calls named by the requests
// addressed to one student, whatever their status (a declined one
// included), since a time, bounded by the limit; a request via discovery
// names none and another student's requests are not theirs.
func TestContactedOpportunityPostIDs_OwnRequestsOnly(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	cmm, ocm := actor(t, "company_member"), actor(t, "other_company_member")
	callA, callB := uuid.NewV7(), uuid.NewV7()
	e.posts.posts[callA] = feed.PostSummary{ID: callA, OrgID: e.companyA, Kind: feed.KindOpportunityCall, Status: feed.StatusPublished}
	e.posts.posts[callB] = feed.PostSummary{ID: callB, OrgID: e.companyB, Kind: feed.KindOpportunityCall, Status: feed.StatusPublished}
	since := e.clk.Now()
	about := func(company uuid.UUID, student string, call uuid.UUID) messaging.CreateInput {
		in := request(company, student)
		in.OpportunityPostID = &call
		return in
	}
	declined, err := e.svc.Create(ctx, cmm, e.companyA, about(e.companyA, "student", callA))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Respond(ctx, actor(t, "student"), declined.Request.ID, decline); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(time.Minute)
	if _, err := e.svc.Create(ctx, ocm, e.companyB, about(e.companyB, "student", callB)); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(time.Minute)
	if _, err := e.svc.Create(ctx, ocm, e.companyB, about(e.companyB, "student_granted", callB)); err != nil {
		t.Fatal(err)
	}
	e.send(t, "student_granted") // via discovery: no call

	r, ok := e.svc.(messaging.ContactedOpportunities)
	if !ok {
		t.Fatal("the messaging service does not implement ContactedOpportunities")
	}
	student, granted := authztest.ID("student"), authztest.ID("student_granted")
	got, err := r.ContactedOpportunityPostIDs(ctx, student, since, 10)
	if err != nil || !slices.Equal(got, []uuid.UUID{callB, callA}) {
		t.Fatalf("student = %v, %v; want callB then callA (the declined request counts)", got, err)
	}
	if got, err := r.ContactedOpportunityPostIDs(ctx, granted, since, 10); err != nil || !slices.Equal(got, []uuid.UUID{callB}) {
		t.Fatalf("student_granted = %v, %v; want callB only", got, err)
	}
	if got, err := r.ContactedOpportunityPostIDs(ctx, student, since, 1); err != nil || !slices.Equal(got, []uuid.UUID{callB}) {
		t.Fatalf("limit 1 = %v, %v", got, err)
	}
	if got, err := r.ContactedOpportunityPostIDs(ctx, student, e.clk.Now().Add(time.Second), 10); err != nil || len(got) != 0 {
		t.Fatalf("since later = %v, %v", got, err)
	}
	for name, user := range map[string]uuid.UUID{"no user": uuid.Nil(), "a company member": cmm.UserID} {
		if got, err := r.ContactedOpportunityPostIDs(ctx, user, since, 10); err != nil || len(got) != 0 {
			t.Fatalf("%s = %v, %v", name, got, err)
		}
	}
	if got, err := r.ContactedOpportunityPostIDs(ctx, student, since, 0); err != nil || len(got) != 0 {
		t.Fatalf("limit 0 = %v, %v", got, err)
	}
}
