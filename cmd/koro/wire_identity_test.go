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

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"

	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/platform/civil"
	"koro/internal/platform/clock"
	"koro/internal/platform/config"
	"koro/internal/platform/crypto"
	"koro/internal/platform/crypto/rewrap"
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
	w.store = identity.NewMemStore(clock.Real().Now) // created_at/updated_at only: no test depends on them
	w.tx = db.NopTx{}
	w.deriver = testDeriver()
	w.hasher = testHasher()
	w.cfg = &config.Identity{LinkBaseURL: "https://app.koro.test"}
	w.secrets = testEnvelope()
	w.memChallenges = true
}

// testEnvelope seals TOTP secrets in tests (fixed local KEK).
var testEnvelope = sync.OnceValue(func() *crypto.Envelope {
	kek, err := crypto.NewLocalKEK([]crypto.Key{{ID: "test1", Secret: []byte("identity-test-kek-32-bytes-long!")}})
	if err != nil {
		panic(err)
	}
	env, err := crypto.NewEnvelope(kek)
	if err != nil {
		panic(err)
	}
	return env
})

// TestWire_IdentityTasksAndPrivacy pins what identity registers (01 §6.2,
// §7): the e-mail token subscription, the notice tasks, the cleanup and
// student verification jobs and the KVKK provider.
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
	want := []string{"identity.sessions_cleanup 30 3 * * * low", "identity.email_tokens_cleanup 35 3 * * * low",
		"identity.edu_expire 30 0 * * * default", "identity.edu_renewal_reminders 0 10 * * * default"}
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
	for _, name := range []string{identity.SendEmailTokenTaskName, identity.SendSecurityNoticeTaskName, identity.SendRoleAlertTaskName,
		identity.SessionsCleanupJobName, identity.EmailTokensCleanupJobName, identity.SendEduNoticeTaskName,
		identity.EduExpireJobName, identity.EduRenewalRemindersJobName} {
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
	store := identity.NewMemStore(clock.Real().Now)
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
		Persona: identity.PersonaStudent, Locale: identity.LocaleTR,
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

// TestWire_AdminMFAOverHTTP runs BE-016 end to end through the real
// ActorResolver: an admin without MFA is refused on the role path
// (authz.mfa_required), enrolls and confirms TOTP (the session steps up),
// then signs in again in two steps (03 §6.4).
func TestWire_AdminMFAOverHTTP(t *testing.T) {
	t.Parallel()
	e := newActorEnv(t)
	u := e.user(t, nil)
	now := e.clk.Now().UTC().Truncate(time.Microsecond)
	if err := e.store.InsertRoleAssignment(t.Context(), identity.RoleAssignment{ID: uuid.NewV7(), UserID: u.ID, Role: authz.PlatformAdmin,
		Reason: "bootstrap", Status: identity.RoleActive, RequestedAt: now, GrantedAt: &now}); err != nil {
		t.Fatal(err)
	}
	app, err := httpx.New(httpx.Deps{Log: obs.Discard(), Actors: e.c.actors})
	if err != nil {
		t.Fatal(err)
	}
	e.c.registerRoutes(app.V1)
	do := func(method, path, tok, body string) (int, map[string]any) {
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
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	login := `{"email":"` + u.Email + `","password":"` + matrixReauthPassword + `"}`
	code, body := do(http.MethodPost, "/api/v1/auth/sessions", "", login)
	if code != http.StatusOK || body["token"] == nil {
		t.Fatalf("login = %d %v", code, body)
	}
	tok := body["token"].(string)
	if code, body = do(http.MethodGet, "/api/v1/admin/platform-roles", tok, ""); code != http.StatusForbidden || body["code"] != "authz.mfa_required" {
		t.Fatalf("admin call without MFA = %d %v", code, body)
	}
	pw := `"reauth":{"password":"` + matrixReauthPassword + `"}`
	if code, body = do(http.MethodPost, "/api/v1/me/mfa/totp", tok, "{}"); code != http.StatusForbidden || body["code"] != "identity.reauth_required" {
		t.Fatalf("enroll without the password (06 H-46) = %d %v", code, body)
	}
	code, body = do(http.MethodPost, "/api/v1/me/mfa/totp", tok, "{"+pw+"}")
	if code != http.StatusOK || body["secret_base32"] == nil {
		t.Fatalf("enroll = %d %v", code, body)
	}
	secret := body["secret_base32"].(string)
	gen := func() string {
		c, err := totp.GenerateCodeCustom(secret, e.clk.Now(), totp.ValidateOpts{Period: 30, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	code, body = do(http.MethodPost, "/api/v1/me/mfa/totp/confirm", tok, `{"code":"`+gen()+`",`+pw+`}`)
	if codes, _ := body["recovery_codes"].([]any); code != http.StatusOK || len(codes) != 10 {
		t.Fatalf("confirm = %d %v", code, body)
	}
	if code, body = do(http.MethodGet, "/api/v1/admin/platform-roles", tok, ""); code != http.StatusOK {
		t.Fatalf("admin call after the step-up = %d %v", code, body)
	}
	// A new sign-in takes two steps; the password step opens nothing.
	code, body = do(http.MethodPost, "/api/v1/auth/sessions", "", login)
	if code != http.StatusOK || body["mfa_required"] != true || body["token"] != nil || body["challenge_id"] == nil {
		t.Fatalf("password step = %d %v", code, body)
	}
	challenge := body["challenge_id"].(string)
	e.c.infra.clock.(interface{ Advance(time.Duration) }).Advance(30 * time.Second)
	code, body = do(http.MethodPost, "/api/v1/auth/sessions/mfa", "", `{"challenge_id":"`+challenge+`","totp_code":"`+gen()+`"}`)
	if code != http.StatusOK || body["token"] == nil {
		t.Fatalf("second step = %d %v", code, body)
	}
	if code, _ = do(http.MethodGet, "/api/v1/admin/platform-roles", body["token"].(string), ""); code != http.StatusOK {
		t.Fatalf("admin call with the MFA session = %d", code)
	}
	// TOTP cannot be turned off while the role is held (k1).
	if code, body = do(http.MethodPost, "/api/v1/me/mfa/totp/disable", tok, `{"reauth":{"totp_code":"000000"}}`); code != http.StatusConflict ||
		body["code"] != "identity.mfa_required_by_role" {
		t.Fatalf("disable = %d %v", code, body)
	}
}

// TestIdentityTemplates_MFAAndRoleAlertsRender: every BE-016 notice kind
// renders its own sentence in both locales (no fallback text).
func TestIdentityTemplates_MFAAndRoleAlertsRender(t *testing.T) {
	t.Parallel()
	tpl, err := notify.EmbeddedTemplates()
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]string{
		"identity.mfa_notice": {identity.NoticeMFAEnabled, identity.NoticeMFADisabled, identity.NoticeRecoveryCodeUsed,
			identity.NoticeRecoveryCodesRegenerated, identity.NoticeMFAReset},
		"identity.role_alert": {identity.AlertRoleRequested, identity.AlertRoleGranted, identity.AlertRoleRejected,
			identity.AlertRoleRevokeRequested, identity.AlertRoleRevoked},
	}
	for id, kinds := range cases {
		for _, loc := range []string{"tr", "en"} {
			fallback, err := tpl.Render(id, loc, notify.RenderInput{Data: map[string]any{"name": "Ada", "kind": "?", "role": "moderator", "link": "https://x.test"}})
			if err != nil {
				t.Fatal(err)
			}
			seen := map[string]bool{}
			for _, k := range kinds {
				r, err := tpl.Render(id, loc, notify.RenderInput{Data: map[string]any{"name": "Ada", "kind": k, "role": "finance_admin", "link": "https://x.test"}})
				if err != nil {
					t.Fatalf("%s %s %s: %v", id, loc, k, err)
				}
				if r.Text == fallback.Text || seen[r.Text] || !strings.Contains(r.Text, "Ada") {
					t.Fatalf("%s %s %s: no own text: %q", id, loc, k, r.Text)
				}
				seen[r.Text] = true
			}
		}
	}
}

// TestWire_IdentityUsesSharedEnvelope (T-185): with only the local KEK
// configured, identity seals TOTP secrets with the shared envelope of
// platform/crypto and declares mfa_totp.secret_enc for crypto.rewrap.
func TestWire_IdentityUsesSharedEnvelope(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	c.identity.secrets = nil
	c.infra.platform.Crypto = config.Crypto{KEKs: config.NewSecret(testKEKs)}
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	env, err := c.keys.envelope()
	if err != nil {
		t.Fatal(err)
	}
	if c.identity.secrets != identity.SecretBox(env) {
		t.Fatal("identity does not use the shared envelope")
	}
	if !slices.ContainsFunc(c.keys.columns, func(col rewrap.Column) bool {
		return col.Table == "mfa_totp" && col.Column == "secret_enc" && col.Key == "user_id" &&
			col.PurposePrefix == "identity.mfa_secret/" && col.PurposeColumn == "user_id"
	}) {
		t.Fatalf("mfa_totp.secret_enc not declared for rewrap: %+v", c.keys.columns)
	}
	// Without any key the identity build fails (no TOTP secret sealed with nothing).
	none := newTestContainer(t)
	none.identity.secrets = nil
	if err := none.build(); err == nil {
		t.Fatal("identity built without a key-encryption key")
	}
}
