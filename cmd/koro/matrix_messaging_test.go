package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"testing"

	"koro/internal/authz/authztest"
	"koro/internal/discovery"
	"koro/internal/messaging"
)

// matrixFixtures seeds fixtures/messaging.yaml (BE-056): a "request" is a
// pending contact request of a company to a student, sent by one of its
// members via discovery, expiring 14 days after the container clock.
func (w *messagingWiring) matrixFixtures(fx *matrixFixtures) error {
	ctx := context.Background()
	for _, f := range fx.Of("messaging") {
		switch f.Kind {
		case "request":
			if err := w.matrixRequest(ctx, fx, f); err != nil {
				return fmt.Errorf("fixture %s: %w", f.Name, err)
			}
		default:
			return fmt.Errorf("fixture %s: unknown kind %q", f.Name, f.Kind)
		}
	}
	return nil
}

func (w *messagingWiring) matrixRequest(ctx context.Context, fx *matrixFixtures, f fixture) error {
	if w.store == nil || w.late == nil {
		return errors.New("messaging is not built")
	}
	id, err := fx.ID(f.Name)
	if err != nil {
		return err
	}
	company, err := fx.Ref(f, "company")
	if err != nil {
		return err
	}
	sender, err := fx.Ref(f, "sender")
	if err != nil {
		return err
	}
	student, err := fx.Ref(f, "student")
	if err != nil {
		return err
	}
	_, err = w.store.Insert(ctx, messaging.ContactRequest{
		ID: id, OrgID: company, SenderUserID: sender, StudentUserID: student, Via: messaging.ViaDiscovery,
		Subject: "Matris talebi", Message: "Profilinizi gördük, görüşmek isteriz. (Matris fikstürü)",
		Status: messaging.StatusPending, ExpiresAt: w.late.infra.clock.Now().UTC().Add(messaging.RequestTTL),
		EntitlementUsageID: authztest.ID(f.Name + "_usage"),
	})
	return err
}

// TestMatrixMessaging_PseudonymLiterals: every anon_id of the messaging
// request bodies is a pseudonym of matrixPseudonyms (the discovery test
// deriver) or a deliberately malformed one.
func TestMatrixMessaging_PseudonymLiterals(t *testing.T) {
	t.Parallel()
	known := map[string]bool{}
	for _, v := range matrixPseudonyms {
		known[v] = true
	}
	files, err := fs.Glob(os.DirFS("testdata/authz"), "bodies/messaging/create_*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("bodies: %v, %v", files, err)
	}
	for _, f := range files {
		raw, err := fs.ReadFile(os.DirFS("testdata/authz"), f)
		if err != nil {
			t.Fatal(err)
		}
		var b struct {
			AnonID string `json:"anon_id"`
		}
		if err := json.Unmarshal(raw, &b); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if !known[b.AnonID] && discovery.ValidAnonID(b.AnonID) {
			t.Errorf("%s: anon_id %q is a well-formed pseudonym that is not in matrixPseudonyms", f, b.AnonID)
		}
	}
}

// TestMatrixMessaging_AcceptOpensLevel3 (BE-056): on the matrix application
// (real messaging, students, discovery, entitlements, orgs and audit over
// the in-memory stores) company_a cannot read the persona student's level 3
// profile until the student accepts its contact request; the acceptance
// (AccessGrants.Grant in the same transaction) opens it and the company's
// view of the request then carries the student's user ID.
func TestMatrixMessaging_AcceptOpensLevel3(t *testing.T) {
	t.Parallel()
	env := &matrixEnv{opts: realMatrixOptions()}
	m, probs := loadMatrix(os.DirFS("testdata/authz"), env.opts.modules)
	if len(probs) > 0 {
		t.Fatal(strings.Join(probs, "\n"))
	}
	env.m = m
	a := env.newMatrixApp(t, m.config)
	student := authztest.ID("student")
	call := func(persona, method, path string, params map[string]string, body string) (int, string) {
		return a.do(t, m, matrixCase{name: "messaging " + method + " " + path, method: method, path: path,
			req: matrixRequest{PathParams: params, Body: body}, persona: persona})
	}
	profile := map[string]string{"org_id": "company_a", "user_id": "student"}
	if code, body := call("company_admin", "GET", "/api/v1/orgs/{org_id}/students/{user_id}", profile, ""); code != 404 {
		t.Fatalf("level 3 before acceptance = %d %s", code, body)
	}
	req := map[string]string{"org_id": "company_a", "request_id": "request_student_a"}
	code, body := call("company_admin", "GET", "/api/v1/orgs/{org_id}/contact-requests/{request_id}", req, "")
	if code != 200 || strings.Contains(body, student.String()) || !strings.Contains(body, `"anon_id":"mjh6rq23ztj3hxgo"`) {
		t.Fatalf("pending request (company view) = %d %s", code, body)
	}
	code, body = call("student", "POST", "/api/v1/me/contact-requests/{request_id}/respond", req,
		"bodies/messaging/respond_accept.json")
	if code != 200 || !strings.Contains(body, `"status":"accepted"`) || !strings.Contains(body, `"name":`) {
		t.Fatalf("accept = %d %s", code, body)
	}
	if code, body := call("company_admin", "GET", "/api/v1/orgs/{org_id}/students/{user_id}", profile, ""); code != 200 {
		t.Fatalf("level 3 after acceptance = %d %s", code, body)
	}
	code, body = call("company_admin", "GET", "/api/v1/orgs/{org_id}/contact-requests/{request_id}", req, "")
	if code != 200 || !strings.Contains(body, `"student_user_id":"`+student.String()+`"`) {
		t.Fatalf("accepted request (company view) = %d %s", code, body)
	}
}
