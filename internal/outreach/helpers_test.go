package outreach_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/consent"
	"koro/internal/directory"
	"koro/internal/entitlements"
	"koro/internal/identity"
	"koro/internal/orgs"
	"koro/internal/outreach"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/httpx"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/problem"
	"koro/internal/platform/ratelimit"
	"koro/internal/students"
)

// t0 is the test clock's start (microsecond precision, like PostgreSQL).
var t0 = time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)

// Org and university fixtures (authztest IDs, as in the matrix).
var (
	companyA      = authztest.ID("company_a")
	companyB      = authztest.ID("company_b")
	clubA         = authztest.ID("club_a")
	clubB         = authztest.ID("club_b")
	clubSensitive = authztest.ID("club_sensitive")
	uniOrgA       = authztest.ID("uni_a")
	uniA          = authztest.UniversityID("uni_a")
	uniB          = authztest.UniversityID("uni_b")
	platformOrg   = authztest.ID("platform_org")
)

// ---- fakes

// profile is one student as count_segment sees it.
type profile struct {
	id       uuid.UUID
	keys     []string
	eligible bool // audience_eligible: verified, adult, active
}

// fakeAudience models students.Audience.Count over profiles with the
// semantics of count_segment (02 §9.1): the dimensions come from the spec's
// keys, a sensitive club's key is dropped without dropping its dimension,
// only eligible profiles count, and below 10 there is no number.
type fakeAudience struct {
	mu        sync.Mutex
	profiles  []profile
	sensitive map[string]bool // club keys students never counts
	calls     int
	err       error
	streams   int // Stream calls (BE-035)
}

// add adds n profiles holding keys and returns their user IDs.
func (f *fakeAudience) add(n int, eligible bool, keys ...string) []uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]uuid.UUID, n)
	for i := range n {
		ids[i] = uuid.NewV7()
		f.profiles = append(f.profiles, profile{id: ids[i], keys: keys, eligible: eligible})
	}
	return ids
}

func (f *fakeAudience) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.profiles = nil
}

func (f *fakeAudience) Count(_ context.Context, spec students.AudienceSpec) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return 0, f.err
	}
	ids := f.match(spec)
	if len(ids) < 10 {
		return 0, students.ErrAudienceTooSmall
	}
	return len(ids), nil
}

// Stream implements outreach.Recipients like students: k ≥ 10 first (fn
// never called below), then every eligible matching profile in user_id
// order. (students also drops minors against identity; the fake leaves
// that to outreach's own check, which the tests exercise.)
func (f *fakeAudience) Stream(ctx context.Context, spec students.AudienceSpec, fn func(uuid.UUID) error) error {
	f.mu.Lock()
	f.streams++
	ids := f.match(spec)
	f.mu.Unlock()
	if len(ids) < 10 {
		return students.ErrAudienceTooSmall
	}
	slices.SortFunc(ids, uuid.UUID.Compare)
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(id); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeAudience) streamCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.streams
}

// match returns the eligible profiles spec targets (count_segment).
func (f *fakeAudience) match(spec students.AudienceSpec) []uuid.UUID {
	dims, keys := map[string]bool{}, map[string]bool{}
	for _, k := range spec.Keys {
		dim, _, _ := strings.Cut(k, ":")
		dims[dim] = true
		if !f.sensitive[k] {
			keys[k] = true
		}
	}
	var out []uuid.UUID
	for _, p := range f.profiles {
		if !p.eligible {
			continue
		}
		matched := map[string]bool{}
		for _, k := range p.keys {
			if keys[k] {
				dim, _, _ := strings.Cut(k, ":")
				matched[dim] = true
			}
		}
		if len(matched) > 0 && len(matched) == len(dims) {
			out = append(out, p.id)
		}
	}
	return out
}

func (f *fakeAudience) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeCatalog is directory: the matrix universities and the sensitive club.
type fakeCatalog struct {
	mu        sync.Mutex
	unis      map[uuid.UUID]directory.University
	sensitive map[uuid.UUID]bool
}

func (c *fakeCatalog) SensitiveOrgIDs(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[uuid.UUID]bool{}
	for _, id := range ids {
		if c.sensitive[id] {
			out[id] = true
		}
	}
	return out, nil
}

func (c *fakeCatalog) GetUniversity(_ context.Context, id uuid.UUID) (directory.University, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	u, ok := c.unis[id]
	if !ok {
		return directory.University{}, directory.ErrNotFound
	}
	return u, nil
}

func (c *fakeCatalog) markSensitive(id uuid.UUID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sensitive[id] = true
}

// fakeOrgs is orgs: every org fixture, active unless set otherwise.
type fakeOrgs struct {
	mu   sync.Mutex
	rows map[uuid.UUID]orgs.OrgSummary
}

func (o *fakeOrgs) Get(_ context.Context, id uuid.UUID) (orgs.OrgSummary, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	s, ok := o.rows[id]
	if !ok {
		return orgs.OrgSummary{}, orgs.ErrNotFound
	}
	return s, nil
}

func (o *fakeOrgs) set(id uuid.UUID, kind orgs.Kind, st orgs.Status) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.rows[id] = orgs.OrgSummary{ID: id, Kind: kind, Name: "Org", Status: st}
}

// auditLog records the events an outreach service writes.
type auditLog struct {
	mu     sync.Mutex
	events []audit.Event
}

func (l *auditLog) Log(_ context.Context, e audit.Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
	return nil
}

// actionsOf returns the recorded events of action.
func (l *auditLog) actionsOf(action string) []audit.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []audit.Event
	for _, e := range l.events {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

// all returns every recorded event.
func (l *auditLog) all() []audit.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.events)
}

// last returns the last recorded event.
func (l *auditLog) last() audit.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.events) == 0 {
		return audit.Event{}
	}
	return l.events[len(l.events)-1]
}

func (l *auditLog) actions() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.events))
	for i, e := range l.events {
		out[i] = e.Action
	}
	return out
}

// failingLimiter simulates an unavailable Redis.
type failingLimiter struct{}

func (failingLimiter) Allow(context.Context, ratelimit.Rule, string) (ratelimit.Result, error) {
	return ratelimit.Result{}, ratelimit.ErrUnavailable
}

// ---- env

// env is an outreach module over a store (in memory by default) and fakes.
type env struct {
	clk      *clock.Fake
	store    outreach.Store
	audience *fakeAudience
	catalog  *fakeCatalog
	orgs     *fakeOrgs
	audit    *auditLog
	limiter  outreach.Limiter
	mod      outreach.Module
	svc      outreach.Service

	// The dispatch (BE-035).
	out       *outbox.Mem
	subs      *outbox.Registry
	ents      entitlements.Service
	users     *fakeUsers
	consents  *fakeConsents
	members   *fakeMembers
	sender    *fakeSender
	posts     *fakePosts
	delivered map[uuid.UUID]bool // outbox events handed to the worker
}

type envOption func(*env, *outreach.Deps)

func withDailyLimit(n int) envOption {
	return func(_ *env, d *outreach.Deps) { d.DailyCountLimit = n }
}

func withLimiter(l outreach.Limiter) envOption {
	return func(e *env, d *outreach.Deps) { e.limiter, d.Limiter = l, l }
}

func newEnv(t *testing.T, opts ...envOption) *env {
	t.Helper()
	clk := clock.NewFake(t0)
	return newEnvWith(t, clk, outreach.NewMemStore(clk.Now), db.NopTx{}, idempotency.NewMemStore(), opts...)
}

func newEnvWith(t *testing.T, clk *clock.Fake, store outreach.Store, tx db.TxManager, keys idempotency.Store, opts ...envOption) *env {
	t.Helper()
	e := &env{
		clk: clk, store: store, audit: &auditLog{},
		audience: &fakeAudience{sensitive: map[string]bool{"club:" + clubSensitive.String(): true}},
		catalog: &fakeCatalog{
			unis: map[uuid.UUID]directory.University{
				uniA: {ID: uniA, OrgID: uniOrgA, Name: "Üniversite A", IsActive: true},
				uniB: {ID: uniB, OrgID: authztest.ID("uni_b"), Name: "Üniversite B", IsActive: true},
			},
			sensitive: map[uuid.UUID]bool{clubSensitive: true},
		},
		orgs:    &fakeOrgs{rows: map[uuid.UUID]orgs.OrgSummary{}},
		limiter: ratelimit.NewMem(clk),
		out:     outbox.NewMem(clk), subs: outbox.NewRegistry(),
		users:     &fakeUsers{users: map[uuid.UUID]identity.UserSummary{}},
		consents:  &fakeConsents{grants: map[consent.Purpose]map[uuid.UUID]bool{}},
		members:   &fakeMembers{members: map[uuid.UUID][]uuid.UUID{}},
		sender:    &fakeSender{byKey: map[string]uuid.UUID{}},
		posts:     &fakePosts{posts: map[uuid.UUID]outreach.PostInfo{}},
		delivered: map[uuid.UUID]bool{},
	}
	for id, kind := range map[uuid.UUID]orgs.Kind{
		companyA: orgs.KindCompany, companyB: orgs.KindCompany, clubA: orgs.KindClub, clubB: orgs.KindClub,
		clubSensitive: orgs.KindClub, uniOrgA: orgs.KindUniversity, authztest.ID("uni_b"): orgs.KindUniversity,
		platformOrg: orgs.KindPlatform,
	} {
		e.orgs.set(id, kind, orgs.StatusActive)
	}
	idem := idempotency.New(idempotency.Deps{Tx: tx, Store: keys, Clock: clk, Log: obs.Discard()})
	ents, err := entitlements.New(entitlements.Deps{Store: entitlements.NewMemStore(clk.Now), Tx: tx, Audit: e.audit,
		Idempotency: idem, Clock: clk, Log: obs.Discard()})
	if err != nil {
		t.Fatal(err)
	}
	e.ents = ents.Service
	d := outreach.Deps{
		Store: store, Tx: tx, Audit: e.audit, Audience: e.audience, Catalog: e.catalog, Orgs: e.orgs,
		Limiter: e.limiter, Clock: clk, Log: obs.Discard(), Idempotency: idem,
		Outbox: e.out, Entitlements: e.ents, Recipients: e.audience, Members: e.members, Users: e.users,
		Consents: e.consents, Sender: e.sender, Posts: e.posts, LinkBaseURL: "https://koro.example.test",
		UnsubscribeURL: testUnsubscribe,
	}
	for _, o := range opts {
		o(e, &d)
	}
	mod, err := outreach.New(d)
	if err != nil {
		t.Fatalf("outreach.New: %v", err)
	}
	e.mod, e.svc = mod, mod.Service
	mod.Subscribe(e.subs)
	if err := e.subs.Err(); err != nil {
		t.Fatal(err)
	}
	return e
}

// recount runs outreach.recount_segments.
func (e *env) recount(t *testing.T) {
	t.Helper()
	for _, j := range e.mod.Jobs() {
		if j.Name == outreach.RecountSegmentsJobName {
			if err := j.Handle(t.Context()); err != nil {
				t.Fatalf("recount: %v", err)
			}
			return
		}
	}
	t.Fatal("no recount job")
}

func actor(t *testing.T, name string) authz.Actor {
	t.Helper()
	p, ok := authztest.Get(name)
	if !ok {
		t.Fatalf("unknown persona %s", name)
	}
	return p.Actor
}

// Audience keys.
func uniKey(id uuid.UUID) string  { return "uni:" + id.String() }
func deptKey(id uuid.UUID) string { return "dept:" + id.String() }
func clubKey(id uuid.UUID) string { return "club:" + id.String() }
func yearKey(y int) string        { return "year:" + strconv.Itoa(y) }

func ptr[T any](v T) *T { return &v }

func wantErr(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

// problemOf returns the status and code of a problem error.
func problemOf(err error) (int, string) {
	if p, ok := errors.AsType[*problem.Error](err); ok {
		return p.Status(), p.Code()
	}
	return 0, ""
}

// ---- HTTP

// newHTTP serves the outreach operations of e with the persona resolver,
// the rate limit middleware over its own in-memory limiter and the
// Idempotency-Key middleware (as cmd/koro does).
func newHTTP(t *testing.T, e *env) *httpx.App {
	t.Helper()
	app, err := httpx.New(httpx.Deps{Log: obs.Discard(), Actors: authztest.NewResolver(append(authztest.Personas(), authztest.Helpers()...)...)})
	if err != nil {
		t.Fatal(err)
	}
	app.API.UseMiddleware(ratelimit.Middleware(app.API, ratelimit.NewMem(e.clk), obs.Discard()))
	app.API.UseMiddleware(idempotency.Middleware(app.API))
	outreach.Register(app.V1, e.svc)
	return app
}

func call(t *testing.T, app *httpx.App, persona, method, path string, body any, hdr map[string]string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequestWithContext(t.Context(), method, path, &buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if p, _ := authztest.Get(persona); p.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.Token)
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func countPath(org uuid.UUID) string    { return "/api/v1/orgs/" + org.String() + "/audience-counts" }
func segmentsPath(org uuid.UUID) string { return "/api/v1/orgs/" + org.String() + "/segments" }

// defBody is a JSON definition.
func defBody(d outreach.Definition) map[string]any {
	out := map[string]any{}
	strs := func(ids []uuid.UUID) []string {
		s := make([]string, len(ids))
		for i, id := range ids {
			s[i] = id.String()
		}
		return s
	}
	if len(d.Uni) > 0 {
		out["uni"] = strs(d.Uni)
	}
	if len(d.Dept) > 0 {
		out["dept"] = strs(d.Dept)
	}
	if len(d.Club) > 0 {
		out["club"] = strs(d.Club)
	}
	if len(d.Year) > 0 {
		out["year"] = d.Year
	}
	return out
}
