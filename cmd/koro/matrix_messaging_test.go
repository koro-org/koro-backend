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
// members via discovery, expiring 14 days after the container clock; a
// "conversation" (BE-162) is the open conversation of such a request,
// accepted.
func (w *messagingWiring) matrixFixtures(fx *matrixFixtures) error {
	ctx := context.Background()
	for _, f := range fx.Of("messaging") {
		switch f.Kind {
		case "request":
			if err := w.matrixRequest(ctx, fx, f); err != nil {
				return fmt.Errorf("fixture %s: %w", f.Name, err)
			}
		case "conversation":
			if err := w.matrixConversation(ctx, fx, f); err != nil {
				return fmt.Errorf("fixture %s: %w", f.Name, err)
			}
		default:
			return fmt.Errorf("fixture %s: unknown kind %q", f.Name, f.Kind)
		}
	}
	return nil
}

func (w *messagingWiring) matrixRequest(ctx context.Context, fx *matrixFixtures, f fixture) error {
	id, err := fx.ID(f.Name)
	if err != nil {
		return err
	}
	_, err = w.insertMatrixRequest(ctx, fx, f, id, messaging.StatusPending)
	return err
}

// insertMatrixRequest writes a contact request of the fixture's company,
// sender and student in status st (an accepted one carries its answer
// time).
func (w *messagingWiring) insertMatrixRequest(ctx context.Context, fx *matrixFixtures, f fixture, id uuid.UUID,
	st messaging.Status,
) (messaging.ContactRequest, error) {
	if w.store == nil || w.late == nil {
		return messaging.ContactRequest{}, errors.New("messaging is not built")
	}
	company, err := fx.Ref(f, "company")
	if err != nil {
		return messaging.ContactRequest{}, err
	}
	sender, err := fx.Ref(f, "sender")
	if err != nil {
		return messaging.ContactRequest{}, err
	}
	student, err := fx.Ref(f, "student")
	if err != nil {
		return messaging.ContactRequest{}, err
	}
	now := w.late.infra.clock.Now().UTC()
	r := messaging.ContactRequest{
		ID: id, OrgID: company, SenderUserID: sender, StudentUserID: student, Via: messaging.ViaDiscovery,
		Subject: "Matris talebi", Message: "Profilinizi gördük, görüşmek isteriz. (Matris fikstürü)",
		Status: st, ExpiresAt: now.Add(messaging.RequestTTL), EntitlementUsageID: authztest.ID(f.Name + "_usage"),
	}
	if st == messaging.StatusAccepted {
		r.RespondedAt = &now
	}
	return w.store.Insert(ctx, r)
}

// matrixConversation writes an accepted request of the fixture (its ID
// authztest.ID(name+"_request")) and its open conversation (BE-162).
func (w *messagingWiring) matrixConversation(ctx context.Context, fx *matrixFixtures, f fixture) error {
	id, err := fx.ID(f.Name)
	if err != nil {
		return err
	}
	r, err := w.insertMatrixRequest(ctx, fx, f, authztest.ID(f.Name+"_request"), messaging.StatusAccepted)
	if err != nil {
		return err
	}
	_, err = w.store.InsertConversation(ctx, messaging.Conversation{
		ID: id, ContactRequestID: &r.ID, OrgID: r.OrgID, StudentUserID: r.StudentUserID, CompanyUserID: r.SenderUserID,
		Status: messaging.ConversationOpen, LastActivityAt: w.late.infra.clock.Now().UTC(),
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

// TestMatrixMessaging_AcceptOpensConversation (BE-162, Q-40): on the matrix
// application (real messaging, students, orgs and audit over the in-memory
// stores, the HTTP layer with its middlewares) the persona student accepts
// company_a's request; the acceptance's conversation is listed on both
// sides; the company's message reaches the student (unread, newest) and the
// student's reply the company; the company's view names who wrote, the
// student's does not.
func TestMatrixMessaging_AcceptOpensConversation(t *testing.T) {
	t.Parallel()
	env := &matrixEnv{opts: realMatrixOptions()}
	m, probs := loadMatrix(os.DirFS("testdata/authz"), env.opts.modules)
	if len(probs) > 0 {
		t.Fatal(strings.Join(probs, "\n"))
	}
	env.m = m
	a := env.newMatrixApp(t, m.config)
	call := func(persona, method, path string, params map[string]string, body string) (int, string) {
		return a.do(t, m, matrixCase{name: "messaging conversation " + method + " " + path, method: method, path: path,
			req: matrixRequest{PathParams: params, Body: body}, persona: persona})
	}
	req := authztest.ID("request_student_a").String()
	if code, body := call("student", "POST", "/api/v1/me/contact-requests/{request_id}/respond",
		map[string]string{"request_id": "request_student_a"}, "bodies/messaging/respond_accept.json"); code != 200 {
		t.Fatalf("accept = %d %s", code, body)
	}
	code, body := call("student", "GET", "/api/v1/me/conversations", nil, "")
	var list struct {
		Items []struct {
			ConversationID   string `json:"conversation_id"`
			ContactRequestID string `json:"contact_request_id"`
			Status           string `json:"status"`
			CanSend          bool   `json:"can_send"`
			Company          struct {
				OrgID string `json:"org_id"`
			} `json:"company"`
		} `json:"items"`
	}
	if code != 200 || json.Unmarshal([]byte(body), &list) != nil {
		t.Fatalf("my conversations = %d %s", code, body)
	}
	conv := ""
	for _, c := range list.Items {
		if c.ContactRequestID == req {
			if c.Status != "open" || !c.CanSend || c.Company.OrgID != authztest.ID("company_a").String() {
				t.Fatalf("conversation = %+v", c)
			}
			conv = c.ConversationID
		}
	}
	if conv == "" {
		t.Fatalf("the acceptance opened no conversation: %s", body)
	}
	org := map[string]string{"org_id": "company_a", "conversation_id": literalPrefix + conv}
	me := map[string]string{"conversation_id": literalPrefix + conv}
	if code, body := call("company_member", "POST", "/api/v1/orgs/{org_id}/conversations/{conversation_id}/messages", org,
		"bodies/messaging/message.json"); code != 201 || !strings.Contains(body, `"sender_side":"company"`) ||
		!strings.Contains(body, `"mine":true`) {
		t.Fatalf("company message = %d %s", code, body)
	}
	code, body = call("student", "GET", "/api/v1/me/conversations/{conversation_id}", me, "")
	if code != 200 || !strings.Contains(body, `"unread_count":1`) || !strings.Contains(body, `"last_message":{`) ||
		strings.Contains(body, authztest.ID("company_member").String()) {
		t.Fatalf("student view = %d %s", code, body)
	}
	if code, body := call("student", "POST", "/api/v1/me/conversations/{conversation_id}/messages", me,
		"bodies/messaging/message.json"); code != 201 {
		t.Fatalf("student reply = %d %s", code, body)
	}
	code, body = call("company_admin", "GET", "/api/v1/orgs/{org_id}/conversations/{conversation_id}/messages", org, "")
	if code != 200 || !strings.Contains(body, `"sender_user_id":"`+authztest.ID("company_member").String()+`"`) ||
		!strings.Contains(body, `"sender_side":"student"`) {
		t.Fatalf("company messages = %d %s", code, body)
	}
}
