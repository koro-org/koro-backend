package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/platform/clock"
	"koro/internal/platform/httpx"
	"koro/internal/platform/obs"
)

// Student verification wiring (BE-018).

// eduUniversity is the university of the test resolvers.
var eduUniversity = uuid.MustParse("01900000-0000-7000-8000-0000000000ed")

func (e *actorEnv) fake() *clock.Fake { return e.c.infra.clock.(*clock.Fake) }

// verified inserts a verified .edu.tr row of u ending at expires.
func (e *actorEnv) verified(t *testing.T, u identity.User, address string, expires time.Time) identity.EduRecord {
	t.Helper()
	now, uni := e.clk.Now().UTC().Truncate(time.Microsecond), eduUniversity
	_, domain, _ := strings.Cut(address, "@")
	v := identity.EduRecord{ID: uuid.NewV7(), UserID: u.ID, Method: identity.EduMethodEmail, EduEmail: address,
		EduEmailNormalized: strings.ToLower(address), EmailDomain: strings.ToLower(domain), UniversityID: &uni,
		Status: identity.EduVerified, VerifiedAt: &now, ExpiresAt: &expires}
	if err := e.store.InsertEduVerification(t.Context(), v); err != nil {
		t.Fatal(err)
	}
	return v
}

// http serves the registered routes with the real ActorResolver.
func (e *actorEnv) http(t *testing.T) func(method, path, tok, body string) (int, string) {
	t.Helper()
	app, err := httpx.New(httpx.Deps{Log: obs.Discard(), Actors: e.c.actors})
	if err != nil {
		t.Fatal(err)
	}
	e.c.registerRoutes(app.V1)
	return func(method, path, tok, body string) (int, string) {
		req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
		req.Header.Set("X-Koro-Client", "mobile")
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
}

// TestActor_StudentStatusLapsedAfterExpiry: the Actor's StudentStatus is
// read on every request (03 §2.1): verified with its university while the
// year runs, lapsed from the instant it ends (before identity.edu_expire
// marks the row), none for a user never verified.
func TestActor_StudentStatusLapsedAfterExpiry(t *testing.T) {
	t.Parallel()
	e := newActorEnv(t)
	u := e.user(t, nil)
	raw := e.session(t, u, identity.ClientMobile)
	expires := e.clk.Now().UTC().Truncate(time.Microsecond).Add(time.Hour)
	e.verified(t, u, "ada@ogr.uni.edu.tr", expires)
	a, err := e.resolve(t.Context(), httpx.TransportBearer, raw)
	if err != nil {
		t.Fatal(err)
	}
	if a.StudentStatus != authz.StudentVerified || a.UniversityID == nil || *a.UniversityID != eduUniversity || !a.IsVerifiedStudent() {
		t.Fatalf("actor before the expiry: %s %v", a.StudentStatus, a.UniversityID)
	}
	e.fake().Advance(time.Hour) // exactly at expires_at: no longer in effect
	a, err = e.resolve(t.Context(), httpx.TransportBearer, raw)
	if err != nil {
		t.Fatal(err)
	}
	if a.StudentStatus != authz.StudentLapsed || a.UniversityID != nil || a.IsVerifiedStudent() {
		t.Fatalf("actor after the expiry: %s %v", a.StudentStatus, a.UniversityID)
	}
	// The nightly job changes the row, not the answer.
	for _, j := range e.c.infra.jobs.Jobs() {
		if j.Name == identity.EduExpireJobName {
			if err := j.Handle(obs.WithTaskID(t.Context(), "task-1")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if a, _ = e.resolve(t.Context(), httpx.TransportBearer, raw); a.StudentStatus != authz.StudentLapsed {
		t.Fatalf("actor after edu_expire: %s", a.StudentStatus)
	}
	// Never verified: none.
	v := e.user(t, nil)
	if a, _ = e.resolve(t.Context(), httpx.TransportBearer, e.session(t, v, identity.ClientMobile)); a.StudentStatus != authz.StudentNone {
		t.Fatalf("never verified: %s", a.StudentStatus)
	}
}

// TestEduEmail_MaskedInResponse: getMyEduVerification never returns the
// .edu.tr address in full (04 §2.1: a***@x.edu.tr), neither does /me.
func TestEduEmail_MaskedInResponse(t *testing.T) {
	t.Parallel()
	e := newActorEnv(t)
	u := e.user(t, nil)
	tok := e.session(t, u, identity.ClientMobile)
	const address = "Ada.Yilmaz@ogr.uni.edu.tr"
	e.verified(t, u, address, e.clk.Now().UTC().Add(365*24*time.Hour))
	do := e.http(t)
	code, body := do(http.MethodGet, "/api/v1/me/edu-verification", tok, "")
	if code != http.StatusOK {
		t.Fatalf("getMyEduVerification = %d %s", code, body)
	}
	var got struct {
		Status     string `json:"status"`
		EduEmail   string `json:"edu_email"`
		University struct {
			ID string `json:"id"`
		} `json:"university"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "verified" || got.EduEmail != "A***@ogr.uni.edu.tr" || got.University.ID != eduUniversity.String() {
		t.Fatalf("answer %s", body)
	}
	for _, path := range []string{"/api/v1/me/edu-verification", "/api/v1/me"} {
		_, b := do(http.MethodGet, path, tok, "")
		if strings.Contains(strings.ToLower(b), "ada.yilmaz") {
			t.Fatalf("%s shows the address: %s", path, b)
		}
	}
}

// TestWire_IdentityReadsDomainResolverHook: identity reads
// hooks.domainResolver at call time (directory, BE-020, fills it after
// identity is built); while it is nil every domain is unknown (422) and
// the document path is the way (BE-018 safe default).
func TestWire_IdentityReadsDomainResolverHook(t *testing.T) {
	t.Parallel()
	e := newActorEnv(t)
	u := e.user(t, nil)
	tok := e.session(t, u, identity.ClientMobile)
	do := e.http(t)
	start := `{"edu_email":"ada@ogr.uni.edu.tr"}`
	if code, body := do(http.MethodPost, "/api/v1/me/edu-verifications", tok, start); code != http.StatusUnprocessableEntity ||
		!strings.Contains(body, "identity.edu_domain_unknown") || strings.Contains(body, "ada@") {
		t.Fatalf("nil hook = %d %s", code, body)
	}
	var asked []string
	e.c.hooks.domainResolver = func(_ context.Context, domain string) (uuid.UUID, bool, error) {
		asked = append(asked, domain)
		return eduUniversity, strings.HasSuffix(domain, "uni.edu.tr"), nil
	}
	if code, body := do(http.MethodPost, "/api/v1/me/edu-verifications", tok, start); code != http.StatusAccepted {
		t.Fatalf("with the hook = %d %s", code, body)
	}
	if len(asked) != 1 || asked[0] != "ogr.uni.edu.tr" {
		t.Fatalf("resolver asked %v (the domain only, never the address)", asked)
	}
	if code, _ := do(http.MethodPost, "/api/v1/me/edu-verifications", "", start); code != http.StatusUnauthorized {
		t.Fatalf("anonymous start = %d", code)
	}
}

// TestIdentityTemplates_EduRender: the student verification e-mails render
// in both locales; every notice kind has its own sentence (no fallback) and
// the reminder carries the days left.
func TestIdentityTemplates_EduRender(t *testing.T) {
	t.Parallel()
	tpl, err := notify.EmbeddedTemplates()
	if err != nil {
		t.Fatal(err)
	}
	for _, loc := range []string{"tr", "en"} {
		r, err := tpl.Render("identity.edu_verify", loc, notify.RenderInput{Data: map[string]any{"name": "Ada",
			"link": "https://x.test/auth/verify-edu#token=T", "token": "TOKEN123", "valid_minutes": "1440"}})
		if err != nil || !strings.Contains(r.Text, "TOKEN123") || !strings.Contains(r.Text, "1440") {
			t.Fatalf("edu_verify %s: %v %q", loc, err, r.Text)
		}
		fallback, err := tpl.Render("identity.edu_notice", loc, notify.RenderInput{Data: map[string]any{"name": "Ada", "kind": "?", "link": "https://x.test"}})
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, k := range []string{"renewal_reminder", "document_approved", "document_rejected", "lapsed"} {
			r, err := tpl.Render("identity.edu_notice", loc, notify.RenderInput{Data: map[string]any{"name": "Ada", "kind": k,
				"days_left": "7", "link": "https://x.test"}})
			if err != nil {
				t.Fatalf("%s %s: %v", loc, k, err)
			}
			if r.Text == fallback.Text || seen[r.Text] || !strings.Contains(r.Text, "Ada") {
				t.Fatalf("%s %s: no own text: %q", loc, k, r.Text)
			}
			if k == "renewal_reminder" && !strings.Contains(r.Text, "7") {
				t.Fatalf("%s reminder without the days: %q", loc, r.Text)
			}
			seen[r.Text] = true
		}
	}
}
