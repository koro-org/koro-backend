package main

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/platform/civil"
	"koro/internal/platform/config"
	"koro/internal/platform/crypto"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/httpx"
	"koro/internal/platform/obs"
	"koro/internal/platform/passwd"
	"koro/internal/platform/privacy"
	"koro/internal/platform/problem"
	"koro/internal/platform/token"
)

// testPepper is the pepper of the shared test hasher (≥ 32 bytes).
const testPepper = "identity-test-pepper-identity-test-pepper"

// testHasher is one argon2id hasher shared by every test container: its
// constructor hashes a dummy password, so one per container would slow the
// matrix down.
var testHasher = sync.OnceValue(func() *passwd.Hasher {
	h, err := passwd.New([]byte(testPepper), 8)
	if err != nil {
		panic(err)
	}
	return h
})

// testPasswordHash is the stored hash of matrixReauthPassword: every
// matrix user signs in and re-authenticates with it (computed once).
var testPasswordHash = sync.OnceValue(func() string {
	h, err := testHasher().Hash(context.Background(), matrixReauthPassword)
	if err != nil {
		panic(err)
	}
	return h
})

// testDeriver derives the e-mail tokens of tests (fixed key).
var testDeriver = sync.OnceValue(func() *crypto.Deriver {
	d, err := crypto.NewDeriver([]crypto.Key{{ID: "test1", Secret: []byte("identity-test-token-key-identity-test-token-key")}})
	if err != nil {
		panic(err)
	}
	return d
})

// useMemStore installs the in-memory identity store used by service,
// wiring and authz matrix tests (05 §13.2), the shared test hasher and
// token deriver. First owner: BE-014. Only tickets of the identity module
// edit this file (BE-001 R4).
func (w *identityWiring) useMemStore() {
	w.store = identity.NewMemStore()
	w.tx = db.NopTx{}
	w.deriver = testDeriver()
	w.hasher = testHasher()
	w.cfg = &config.Identity{LinkBaseURL: "https://app.koro.test"}
}

// TestWire_IdentityTasksAndPrivacy pins what identity registers (01 §6.2,
// §7): the e-mail token subscription, the security notice task, the two
// cleanup jobs and the KVKK provider.
func TestWire_IdentityTasksAndPrivacy(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	var jobs []string
	for _, j := range c.infra.jobs.Jobs() {
		if strings.HasPrefix(j.Name, "identity.") {
			jobs = append(jobs, j.Name+" "+j.Cron+" "+j.Queue)
		}
	}
	want := []string{"identity.sessions_cleanup 30 3 * * * low", "identity.email_tokens_cleanup 35 3 * * * low"}
	if !slices.Equal(jobs, want) {
		t.Fatalf("identity jobs %q, want %q", jobs, want)
	}
	subs := c.infra.subs.For(domainevent.TopicIdentityEmailTokenIssued)
	if len(subs) != 1 || subs[0].Name != identity.SendEmailTokenTaskName {
		t.Fatalf("identity.email_token_issued subscriptions %+v", subs)
	}
	mux, err := c.workerMux(obs.Discard())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{identity.SendEmailTokenTaskName, identity.SendSecurityNoticeTaskName,
		identity.SessionsCleanupJobName, identity.EmailTokensCleanupJobName} {
		if !slices.Contains(mux.Types(), name) {
			t.Errorf("worker does not route %s", name)
		}
	}
	if !slices.ContainsFunc(c.privacy, func(p privacy.Provider) bool { return p.Name() == "identity" }) {
		t.Fatal("identity privacy provider not registered")
	}
}

// TestWire_IdentityBindsActorsAndReauth: build sets the real resolver and
// consent's verifier only when the slots are empty (a preset matrix
// resolver or test verifier stays).
func TestWire_IdentityBindsActorsAndReauth(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	c.consent.reauth = nil
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.actors.(actorResolver); !ok {
		t.Fatalf("c.actors = %T, want the identity actorResolver", c.actors)
	}
	if c.consent.reauth == nil {
		t.Fatal("consent's re-authentication verifier is not bound")
	}
	if c.notify.platformAdmins == nil {
		t.Fatal("notify's platform admin resolver is not bound")
	}
	if admins, err := c.notify.platformAdmins(t.Context()); err != nil || len(admins) != 0 {
		t.Fatalf("platformAdmins = %v, %v", admins, err)
	}
	// The bound verifier is identity's: a user without a password fails it.
	if _, err := c.consent.reauth(t.Context(), uuid.NewV7(), "nope", ""); !errors.Is(err, identity.ErrReauthFailed) {
		t.Fatalf("reauth of an unknown user = %v, want identity.reauth_failed", err)
	}
	if _, err := c.consent.reauth(t.Context(), uuid.NewV7(), "", ""); !errors.Is(err, identity.ErrReauthFailed) {
		t.Fatalf("reauth without proof = %v", err)
	}
}

// actorEnv is a built test container with one active user and helpers to
// open sessions and resolve credentials through the real ActorResolver.
type actorEnv struct {
	c     *container
	store *identity.MemStore
	clk   interface{ Now() time.Time }
}

func newActorEnv(t *testing.T) *actorEnv {
	t.Helper()
	c := newTestContainer(t)
	store := identity.NewMemStore()
	c.identity.store = store
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	return &actorEnv{c: c, store: store, clk: c.infra.clock}
}

func (e *actorEnv) user(t *testing.T, birth *civil.Date) identity.User {
	t.Helper()
	now := e.clk.Now().UTC().Truncate(time.Microsecond)
	id := uuid.NewV7()
	u := identity.User{
		ID: id, Status: identity.StatusActive, Email: id.String() + "@actor.koro.test", EmailNormalized: id.String() + "@actor.koro.test",
		EmailVerifiedAt: &now, PasswordHash: testPasswordHash(), FirstName: "Ada", LastName: "Yılmaz", BirthDate: birth,
		Persona: identity.PersonaStudent, Locale: identity.LocaleTR, CreatedAt: now, UpdatedAt: now,
	}
	if birth == nil {
		u.Persona = identity.PersonaCompany
	}
	if err := e.store.InsertUser(t.Context(), u); err != nil {
		t.Fatal(err)
	}
	return u
}

func (e *actorEnv) session(t *testing.T, u identity.User, kind identity.ClientKind) string {
	t.Helper()
	raw, hash, err := token.New()
	if err != nil {
		t.Fatal(err)
	}
	now := e.clk.Now().UTC().Truncate(time.Microsecond)
	s := identity.Session{ID: uuid.NewV7(), UserID: u.ID, TokenHash: hash, ClientKind: kind, CreatedAt: now, LastSeenAt: now,
		IdleExpiresAt: now.Add(48 * time.Hour), AbsoluteExpiresAt: now.Add(72 * time.Hour)}
	if err := e.store.InsertSession(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	return raw
}

func (e *actorEnv) resolve(ctx context.Context, tr httpx.Transport, raw string) (authz.Actor, error) {
	rctx, err := e.c.actors.Resolve(ctx, httpx.Credential{Transport: tr, TokenHash: sha256.Sum256([]byte(raw))})
	if err != nil {
		return authz.Actor{}, err
	}
	return authz.ActorFrom(rctx)
}

// TestActorResolver_MinorComputedEachRequest: IsMinor is computed from the
// birth date at every request (02 §5.1, never stored): the same session
// turns adult on the 18th birthday without any write.
func TestActorResolver_MinorComputedEachRequest(t *testing.T) {
	t.Parallel()
	e := newActorEnv(t)
	now := e.clk.Now()
	istanbulToday := civil.Today(now)
	// 18 years old tomorrow (Europe/Istanbul).
	birth, err := civil.New(istanbulToday.Year-18, istanbulToday.Month, istanbulToday.Day)
	if err != nil {
		t.Fatal(err)
	}
	birth = birth.AddDays(1)
	u := e.user(t, &birth)
	raw := e.session(t, u, identity.ClientMobile)

	a, err := e.resolve(t.Context(), httpx.TransportBearer, raw)
	if err != nil {
		t.Fatal(err)
	}
	if !a.IsMinor || a.UserID != u.ID || a.Kind != authz.KindUser || a.StudentStatus != authz.StudentNone {
		t.Fatalf("actor before the birthday = %+v", a)
	}
	e.c.infra.clock.(interface{ Advance(time.Duration) }).Advance(24 * time.Hour)
	a, err = e.resolve(t.Context(), httpx.TransportBearer, raw)
	if err != nil {
		t.Fatal(err)
	}
	if a.IsMinor {
		t.Fatal("IsMinor still true on the 18th birthday: it must be computed per request")
	}
	// A non-student without a birth date is never a minor.
	adult := e.user(t, nil)
	a, err = e.resolve(t.Context(), httpx.TransportBearer, e.session(t, adult, identity.ClientMobile))
	if err != nil || a.IsMinor {
		t.Fatalf("actor without birth date = %+v, %v", a, err)
	}
}

// TestSession_CookieTokenAsBearer_401 (03 §2.2-1): a web session's token
// sent as a bearer token (and a mobile token sent as the cookie) is
// rejected; over HTTP, a protected operation answers 401 and a public one
// runs anonymously (06 H-25).
func TestSession_CookieTokenAsBearer_401(t *testing.T) {
	t.Parallel()
	e := newActorEnv(t)
	u := e.user(t, nil)
	web := e.session(t, u, identity.ClientWeb)
	mobile := e.session(t, u, identity.ClientMobile)

	if _, err := e.resolve(t.Context(), httpx.TransportCookie, web); err != nil {
		t.Fatalf("web session as cookie: %v", err)
	}
	if _, err := e.resolve(t.Context(), httpx.TransportBearer, web); !errors.Is(err, problem.ErrUnauthenticated) {
		t.Fatalf("web session as bearer = %v, want unauthenticated", err)
	}
	if _, err := e.resolve(t.Context(), httpx.TransportCookie, mobile); !errors.Is(err, problem.ErrUnauthenticated) {
		t.Fatalf("mobile session as cookie = %v, want unauthenticated", err)
	}

	app, err := httpx.New(httpx.Deps{Log: obs.Discard(), Actors: e.c.actors})
	if err != nil {
		t.Fatal(err)
	}
	e.c.registerRoutes(app.V1)
	do := func(method, path string, hdr map[string]string, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		return rec
	}
	if rec := do(http.MethodGet, "/api/v1/me", map[string]string{"Authorization": "Bearer " + web}, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /me with the cookie token as bearer = %d %s, want 401", rec.Code, rec.Body)
	}
	if rec := do(http.MethodGet, "/api/v1/me", map[string]string{"Cookie": httpx.SessionCookieName + "=" + web}, ""); rec.Code != http.StatusOK {
		t.Fatalf("GET /me with the cookie = %d %s, want 200", rec.Code, rec.Body)
	}
	// 06 H-25: a rejected credential on a public operation is anonymous.
	rec := do(http.MethodPost, "/api/v1/auth/password-resets", map[string]string{"Authorization": "Bearer " + web},
		`{"email":"nobody@actor.koro.test"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("public operation with a rejected credential = %d %s, want 202 (anonymous)", rec.Code, rec.Body)
	}
}

// TestWire_IdentityEmailGoesThroughNotify: the worker side of a password
// reset with the wired notify service (BE-015 store, preference
// "security"): the e-mail is rendered, recorded once and sent to the
// account's address, and the token in it is the stored one.
func TestWire_IdentityEmailGoesThroughNotify(t *testing.T) {
	t.Parallel()
	e := newActorEnv(t)
	u := e.user(t, nil)
	now := e.clk.Now().UTC().Truncate(time.Microsecond)
	id := uuid.NewV7()
	b, err := testDeriver().DerivedToken("identity.email_token", "test1", id[:])
	if err != nil {
		t.Fatal(err)
	}
	raw := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
	if err := e.store.InsertEmailToken(t.Context(), identity.EmailToken{ID: id, UserID: u.ID, Purpose: identity.PurposePasswordReset,
		KeyID: "test1", TokenHash: token.Hash(raw), ExpiresAt: now.Add(30 * time.Minute), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	env, err := domainevent.New(domainevent.IdentityEmailTokenIssued{TokenID: id, UserID: u.ID, Purpose: string(identity.PurposePasswordReset)}, "email_token", id)
	if err != nil {
		t.Fatal(err)
	}
	env.ID, env.OccurredAt = uuid.NewV7(), now
	payload, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	sub := e.c.infra.subs.For(domainevent.TopicIdentityEmailTokenIssued)[0]
	for range 2 { // at-least-once delivery: the second run is deduplicated
		if err := sub.HandleTask(workerContext(t), payload); err != nil {
			t.Fatalf("%s: %v", sub.Name, err)
		}
	}
	sent := e.c.notify.transport.(*notify.MemTransport).Sent()
	if len(sent) != 1 || sent[0].To != u.Email || !strings.Contains(sent[0].Text, raw) ||
		!strings.Contains(sent[0].Text, "https://app.koro.test/auth/reset-password#token="+raw) {
		t.Fatalf("sent %d e-mails (to %q)", len(sent), func() string {
			if len(sent) == 0 {
				return ""
			}
			return sent[0].To
		}())
	}
}
