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
	"time"
	"uuid"

	"koro/internal/authz/authztest"
	"koro/internal/consent"
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

// TestMatrixMessaging_AcceptSharesContact (T-335/T-345; BE-089): on the
// matrix application (real messaging, students, consent and identity over
// the in-memory stores) the persona student accepts company_a's request
// with share_contact and the current contact_info_share text: company_a's
// level 3 view carries the e-mail, company_b's does not (its
// grant_student_b: sharing is per company); the privacy setting mirrors
// the consent. unshareMyAccessGrantContact on company_a's grant takes the
// e-mail back while its level 3 access stays. (The rollback of a refused
// sharing needs a real transaction: TestService_Postgres_AcceptShareContactSameTx,
// TestShareContactWithCompany_ConsentFailureRollsBack_Postgres.)
func TestMatrixMessaging_AcceptSharesContact(t *testing.T) {
	t.Parallel()
	env := &matrixEnv{opts: realMatrixOptions()}
	m, probs := loadMatrix(os.DirFS("testdata/authz"), env.opts.modules)
	if len(probs) > 0 {
		t.Fatal(strings.Join(probs, "\n"))
	}
	env.m = m
	a := env.newMatrixApp(t, m.config)
	ctx := t.Context()
	// The current contact_info_share text (its body is matrixDiscoveryText,
	// whose SHA-256 the body holds).
	id, purpose, base := uuid.NewV7(), consent.PurposeContactInfoShare, time.Now().UTC().Truncate(time.Microsecond)
	if err := a.c.consent.store.InsertNotice(ctx, consent.Notice{ID: id, Kind: consent.KindConsentText, PurposeCode: &purpose,
		Audience: consent.Audience("student"), Locale: consent.LocaleTR, Version: "2026.10.23", Title: "Matris iletişim paylaşımı",
		BodyMD: matrixDiscoveryText, BodySHA256: consent.BodyDigest(matrixDiscoveryText), CreatedAt: base.Add(-48 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.c.consent.store.Publish(ctx, id, base.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	a.fx.mu.Lock()
	a.fx.saved["notice_contact_share"] = id
	a.fx.mu.Unlock()
	call := func(persona, method, path string, params map[string]string, body string) (int, string) {
		return a.do(t, m, matrixCase{name: "messaging share " + method + " " + path, method: method, path: path,
			req: matrixRequest{PathParams: params, Body: body}, persona: persona})
	}
	l3 := func(persona, org string) (int, string) {
		return call(persona, "GET", "/api/v1/orgs/{org_id}/students/{user_id}", map[string]string{"org_id": org, "user_id": "student"}, "")
	}
	email := `"contact":{"email":"` + matrixEmail("student") + `"}`

	code, body := call("student", "POST", "/api/v1/me/contact-requests/{request_id}/respond",
		map[string]string{"request_id": "request_student_a"}, "bodies/messaging/respond_accept_share.json")
	if code != 200 || !strings.Contains(body, `"status":"accepted"`) {
		t.Fatalf("accept with share_contact = %d %s", code, body)
	}
	if code, body := l3("company_admin", "company_a"); code != 200 || !strings.Contains(body, email) {
		t.Fatalf("company_a's level 3 after sharing = %d %s", code, body)
	}
	if code, body := l3("other_company_member", "company_b"); code != 200 || strings.Contains(body, `"contact"`) {
		t.Fatalf("company_b's level 3 (not shared with it) = %d %s", code, body)
	}
	if code, body := call("student", "GET", "/api/v1/me/privacy", nil, ""); code != 200 ||
		!strings.Contains(body, `"show_contact_on_grant":true`) {
		t.Fatalf("privacy after sharing = %d %s", code, body)
	}

	code, body = call("student", "GET", "/api/v1/me/access-grants", nil, "")
	var grants struct {
		Items []struct {
			GrantID string `json:"grant_id"`
			Company struct {
				OrgID string `json:"org_id"`
			} `json:"company"`
			ContactSharedAt *time.Time `json:"contact_shared_at"`
		} `json:"items"`
	}
	if code != 200 || json.Unmarshal([]byte(body), &grants) != nil {
		t.Fatalf("my grants = %d %s", code, body)
	}
	grantA := ""
	for _, g := range grants.Items {
		switch g.Company.OrgID {
		case authztest.ID("company_a").String():
			if g.ContactSharedAt == nil {
				t.Fatalf("company_a's grant does not show the share: %s", body)
			}
			grantA = g.GrantID
		case authztest.ID("company_b").String():
			if g.ContactSharedAt != nil {
				t.Fatalf("company_b's grant shows a share: %s", body)
			}
		}
	}
	if grantA == "" {
		t.Fatalf("no grant of company_a: %s", body)
	}
	if code, body := call("student", "DELETE", "/api/v1/me/access-grants/{grant_id}/contact",
		map[string]string{"grant_id": literalPrefix + grantA}, ""); code != 204 {
		t.Fatalf("unshare = %d %s", code, body)
	}
	if code, body := l3("company_admin", "company_a"); code != 200 || strings.Contains(body, `"contact"`) {
		t.Fatalf("company_a's level 3 after unsharing = %d %s", code, body)
	}
}
