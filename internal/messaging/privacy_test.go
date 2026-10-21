package messaging_test

import (
	"testing"
	"time"

	"koro/internal/authz/authztest"
	"koro/internal/messaging"
	"koro/internal/platform/privacy"
)

// TestPrivacy_ExportAndErase (01 §3.3, 02 §14): the student's export holds
// the received requests (with the company) and the blocks; the sender's
// export holds the sent requests without the student; erasure deletes the
// received requests and the blocks, keeps the sent ones (the company's
// record, with a reason) and is idempotent.
func TestPrivacy_ExportAndErase(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	v := e.send(t, "student")
	if err := e.svc.Block(ctx, actor(t, "student"), e.companyB); err != nil {
		t.Fatal(err)
	}
	student, sender := authztest.ID("student"), authztest.ID("company_member")

	sec, err := e.mod.Privacy.Export(ctx, student)
	if err != nil || sec.Module != "messaging" || len(sec.Items) != 2 {
		t.Fatalf("student export = %+v, %v", sec, err)
	}
	records := map[string]map[string]any{}
	for _, it := range sec.Items {
		records[it["record"].(string)] = it
	}
	if r := records["contact_request_received"]; r == nil || r["company_org_id"] != e.companyA.String() ||
		r["contact_request_id"] != v.Request.ID.String() || r["message"] != v.Request.Message {
		t.Fatalf("received item = %+v", r)
	}
	if b := records["company_block"]; b == nil || b["company_org_id"] != e.companyB.String() {
		t.Fatalf("block item = %+v", b)
	}

	sent, err := e.mod.Privacy.Export(ctx, sender)
	if err != nil || len(sent.Items) != 1 || sent.Items[0]["record"] != "contact_request_sent" {
		t.Fatalf("sender export = %+v, %v", sent, err)
	}
	for k, val := range sent.Items[0] {
		if val == student.String() {
			t.Fatalf("the sender's export names the student under %q", k)
		}
	}

	res, err := e.mod.Privacy.Erase(ctx, student)
	if err != nil || res.Deleted != 2 || res.Retained != 0 {
		t.Fatalf("student erase = %+v, %v", res, err)
	}
	if again, err := e.mod.Privacy.Erase(ctx, student); err != nil || again.Deleted != 0 {
		t.Fatalf("erase again = %+v, %v", again, err)
	}
	if left, _ := e.store.ReceivedBy(ctx, student); len(left) != 0 {
		t.Fatalf("received left: %v", left)
	}

	w := e.send(t, "student_granted")
	res, err = e.mod.Privacy.Erase(ctx, sender)
	if err != nil || res.Deleted != 0 || res.Retained != 1 || res.RetainedReason == "" {
		t.Fatalf("sender erase = %+v, %v", res, err)
	}
	if _, err := e.store.Get(ctx, e.companyA, w.Request.ID); err != nil {
		t.Fatalf("the company's record was deleted: %v", err)
	}
}

// TestSweep_ClosedAfter12Months (02 §14.1 contact_request_closed): an
// unapproved policy counts only; an approved one deletes the requests
// closed more than the retention ago (updated_at of the closed row) and
// never a pending one.
func TestSweep_ClosedAfter12Months(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	old := e.send(t, "student")
	if _, err := e.svc.Respond(ctx, actor(t, "student"), old.Request.ID, messaging.DecisionDeclined); err != nil {
		t.Fatal(err)
	}
	pending := e.send(t, "student_granted")
	e.store.Age(old.Request.ID, 400*24*time.Hour)
	e.store.Age(pending.Request.ID, 400*24*time.Hour)
	p := privacy.RetentionPolicy{Code: messaging.PolicyContactRequestClosed, Retention: 365 * 24 * time.Hour}
	now := e.clk.Now()

	r, err := e.mod.Sweeper.Sweep(ctx, p, now)
	if err != nil || r.Count != 1 || r.Table != "contact_requests" || r.Method != privacy.MethodHardDelete {
		t.Fatalf("dry run = %+v, %v", r, err)
	}
	if _, err := e.store.GetForStudent(ctx, authztest.ID("student"), old.Request.ID); err != nil {
		t.Fatal("a dry run deleted")
	}
	p.LegalApproved = true
	if r, err = e.mod.Sweeper.Sweep(ctx, p, now); err != nil || r.Count != 1 {
		t.Fatalf("sweep = %+v, %v", r, err)
	}
	if _, err := e.store.GetForStudent(ctx, authztest.ID("student_granted"), pending.Request.ID); err != nil {
		t.Fatal("a pending request was swept")
	}
	if _, err := e.mod.Sweeper.Sweep(ctx, privacy.RetentionPolicy{Code: "other"}, now); err == nil {
		t.Fatal("an unknown policy was swept")
	}
	if got := e.mod.Sweeper.PolicyCodes(); len(got) != 1 || got[0] != messaging.PolicyContactRequestClosed {
		t.Fatalf("policy codes = %v", got)
	}
}
