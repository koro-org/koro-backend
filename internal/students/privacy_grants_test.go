package students_test

import (
	"net/http"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/students"
)

// TestExport_RevokeReasonsAndResume (BE-088; Q-08/T-338, KVKK md. 11): the
// student's KVKK export carries the company's revocation reason, the
// system's expiry reason (suspension_expired) and the resume time of a
// resumed grant; no answer of the app carries a reason (the student's grant
// list and the company's list stay without one).
func TestExport_RevokeReasonsAndResume(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	app := newHTTP(t, e)
	ctx := t.Context()
	orgA, recA, _ := e.company("Acme")
	orgB, recB, _ := e.company("Beta")
	orgC, _, _ := e.company("Gamma")
	s, _ := e.granted(t, orgA)
	for _, org := range []uuid.UUID{orgB, orgC} {
		if err := e.svc.Grant(ctx, s.UserID, org, students.SourceContactRequest, uuid.NewV7()); err != nil {
			t.Fatal(err)
		}
	}
	const note = "Pozisyon kapandı, iç not"
	if err := e.svc.RevokeCompanyAccessGrant(ctx, recA, orgA, s.UserID, note); err != nil {
		t.Fatal(err)
	}
	e.lapse(t, s)
	e.clk.Advance(time.Hour)
	e.reverify(t, s, e.uniA, false)
	if _, err := e.svc.ResumeMyAccessGrant(ctx, s, e.suspendedGrant(t, s.UserID, orgB).ID); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(window)
	if err := e.runJob(t, students.ExpireSuspendedGrantsJobName); err != nil {
		t.Fatal(err)
	}

	sec, err := e.mod.Privacy.Export(ctx, s.UserID)
	if err != nil {
		t.Fatal(err)
	}
	byOrg := map[string]map[string]any{}
	for _, it := range sec.Items {
		if it["record"] == "access_grant" {
			byOrg[it["company_org_id"].(string)] = it
		}
	}
	if a := byOrg[orgA.String()]; a["revoked_by"] != "company" || a["revoke_reason"] != note {
		t.Fatalf("the company's revocation in the export = %v", a)
	}
	if b := byOrg[orgB.String()]; b["status"] != "active" || b["resumed_at"] == nil || b["suspended_at"] != nil ||
		b["revoke_reason"] != nil {
		t.Fatalf("the resumed grant in the export = %v", b)
	}
	if c := byOrg[orgC.String()]; c["revoked_by"] != "system" || c["revoke_reason"] != students.ReasonSuspensionExpired ||
		c["suspended_at"] == nil || c["revoked_at"] == nil {
		t.Fatalf("the expired grant in the export = %v", c)
	}

	// No answer of the app carries a reason.
	for _, r := range []struct {
		name string
		body string
	}{
		{"the student's grant list", e.send(t, app, s, http.MethodGet, "/api/v1/me/access-grants", "").Body.String()},
		{"company B's list", e.send(t, app, recB, http.MethodGet, "/api/v1/orgs/"+orgB.String()+"/students", "").Body.String()},
		{"company B's profile view", e.send(t, app, recB, http.MethodGet,
			"/api/v1/orgs/"+orgB.String()+"/students/"+s.UserID.String(), "").Body.String()},
	} {
		if strings.Contains(r.body, "reason") || strings.Contains(r.body, note) || !strings.Contains(r.body, "{") {
			t.Fatalf("%s carries a reason (or failed): %s", r.name, r.body)
		}
	}
}
