package discovery_test

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/discovery"
	"koro/internal/entitlements"
	"koro/internal/orgs"
	"koro/internal/platform/crypto"
	"koro/internal/platform/db"
	"koro/internal/platform/httpx"
	"koro/internal/platform/obs"
	"koro/internal/students"
	"koro/internal/talent"
)

// student is one level 2 candidate of the fake students module.
type student struct {
	uni          uuid.UUID
	skills       []uuid.UUID
	languages    []string
	discoverable bool // every read-time check of students passes
}

// studentsFake is the level 2 contract with students' semantics: OR inside
// a filter, AND across filters, fewer than 10 → BelowThreshold, hits per
// requested skill and language, a match set in user ID order.
type studentsFake struct {
	mu       sync.Mutex
	students map[uuid.UUID]*student
	calls    map[string]int
	lastMost int
}

func (f *studentsFake) count(name string) {
	f.calls[name]++
}

func (f *studentsFake) callCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

func (f *studentsFake) IsDiscoverable(_ context.Context, id uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.count("IsDiscoverable")
	s, ok := f.students[id]
	return ok && s.discoverable, nil
}

func (f *studentsFake) DiscoveryMatches(_ context.Context, q students.DiscoveryQuery, most int) (students.DiscoveryMatchSet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.count("DiscoveryMatches")
	f.lastMost = most
	var out []students.DiscoveryMatch
	for _, id := range f.sorted() {
		s := f.students[id]
		if !s.discoverable || (len(q.UniversityIDs) > 0 && !slices.Contains(q.UniversityIDs, s.uni)) {
			continue
		}
		m := students.DiscoveryMatch{UserID: id}
		for _, k := range q.SkillIDs {
			if slices.Contains(s.skills, k) {
				m.SkillHits++
			}
		}
		for _, l := range q.LanguageCodes {
			if slices.Contains(s.languages, l) {
				m.LanguageHits++
			}
		}
		if (len(q.SkillIDs) > 0 && m.SkillHits == 0) || (len(q.LanguageCodes) > 0 && m.LanguageHits == 0) {
			continue
		}
		out = append(out, m)
	}
	if len(out) < students.KThreshold {
		return students.DiscoveryMatchSet{Matches: []students.DiscoveryMatch{}, BelowThreshold: true}, nil
	}
	return students.DiscoveryMatchSet{Matches: out}, nil
}

func (f *studentsFake) AnonProfiles(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]students.AnonProfile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.count("AnonProfiles")
	if len(ids) > 100 {
		return nil, errors.New("more than 100")
	}
	out := map[uuid.UUID]students.AnonProfile{}
	for _, id := range ids {
		if s, ok := f.students[id]; ok && s.discoverable {
			out[id] = students.AnonProfile{UserID: id}
		}
	}
	return out, nil
}

func (f *studentsFake) DiscoveryCandidatesAfter(_ context.Context, after uuid.UUID, limit int) ([]uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.count("DiscoveryCandidatesAfter")
	var out []uuid.UUID
	for _, id := range f.sorted() {
		if bytes.Compare(id[:], after[:]) > 0 && f.students[id].discoverable {
			out = append(out, id)
		}
	}
	return out[:min(len(out), limit)], nil
}

func (f *studentsFake) sorted() []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(f.students))
	for id := range f.students {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	return ids
}

// rankingFake is talent's band read; it must be called outside a
// transaction (talent opens its own, app_talent). Without mults no config
// version is in force (BE-099).
type rankingFake struct {
	mu    sync.Mutex
	bands map[uuid.UUID]talent.Band
	calls int
	inTx  bool

	mults     talent.BandMultipliers // nil: no config version in force
	multErr   error
	multCalls int
}

func (r *rankingFake) BandMultipliers(ctx context.Context) (talent.BandMultipliers, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.multCalls++
	r.inTx = r.inTx || db.InTx(ctx)
	if r.multErr != nil {
		return nil, false, r.multErr
	}
	return r.mults, r.mults != nil, nil
}

func (r *rankingFake) setMults(m talent.BandMultipliers) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mults = m
}

func (r *rankingFake) Bands(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]talent.Band, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.inTx = r.inTx || db.InTx(ctx)
	out := map[uuid.UUID]talent.Band{}
	for _, id := range ids {
		if b, ok := r.bands[id]; ok {
			out[id] = b
		}
	}
	return out, nil
}

type entitlementsFake struct {
	mu       sync.Mutex
	entitled map[uuid.UUID]bool
}

func (e *entitlementsFake) Available(_ context.Context, org uuid.UUID, f entitlements.Feature) (entitlements.Availability, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if f != entitlements.FeatureDiscoveryAccess {
		return entitlements.Availability{}, errors.New("unexpected feature " + string(f))
	}
	a := entitlements.Availability{Feature: f}
	if e.entitled[org] {
		until := t0.AddDate(0, 1, 0)
		a.AccessUntil = &until
	}
	return a, nil
}

type orgsFake struct {
	mu   sync.Mutex
	orgs map[uuid.UUID]orgs.OrgSummary
}

func (o *orgsFake) Get(_ context.Context, id uuid.UUID) (orgs.OrgSummary, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	s, ok := o.orgs[id]
	if !ok {
		return orgs.OrgSummary{}, orgs.ErrNotFound
	}
	return s, nil
}

// auditFake records the events; every one must be written in a
// transaction.
type auditFake struct {
	mu     sync.Mutex
	events []audit.Event
	outTx  int
	fail   error
}

func (a *auditFake) Log(ctx context.Context, e audit.Event) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !db.InTx(ctx) {
		a.outTx++
	}
	if a.fail != nil {
		return a.fail
	}
	a.events = append(a.events, e)
	return nil
}

func (a *auditFake) of(action authz.Action) []audit.Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []audit.Event
	for _, e := range a.events {
		if e.Action == string(action) {
			out = append(out, e)
		}
	}
	return out
}

type env struct {
	students *studentsFake
	ranking  *rankingFake
	ents     *entitlementsFake
	orgs     *orgsFake
	audit    *auditFake
	ps       *discovery.Pseudonyms
	svc      discovery.Service

	companyA, companyB, club uuid.UUID
	uniA, uniB               uuid.UUID
}

// testKeys is the token keyring of the tests: k2 is active, k1 the key
// before the rotation.
var testKeys = []crypto.Key{
	{ID: "k2", Secret: []byte("discovery-test-token-key-two-discovery-test")},
	{ID: "k1", Secret: []byte("discovery-test-token-key-one-discovery-test")},
}

func testDeriver(t *testing.T, keys ...crypto.Key) *crypto.Deriver {
	t.Helper()
	d, err := crypto.NewDeriver(keys)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func testPseudonyms(t *testing.T) *discovery.Pseudonyms {
	t.Helper()
	ps, err := discovery.NewPseudonyms(testDeriver(t, testKeys...), []string{"k2", "k1"})
	if err != nil {
		t.Fatal(err)
	}
	return ps
}

// newEnv builds discovery over fakes: company_a and company_b (the matrix
// fixtures' IDs, so the personas are their members) are active companies
// with discovery_access; club_a is a club.
func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{
		students: &studentsFake{students: map[uuid.UUID]*student{}, calls: map[string]int{}},
		ranking:  &rankingFake{bands: map[uuid.UUID]talent.Band{}},
		ents:     &entitlementsFake{entitled: map[uuid.UUID]bool{}},
		orgs:     &orgsFake{orgs: map[uuid.UUID]orgs.OrgSummary{}},
		audit:    &auditFake{},
		ps:       testPseudonyms(t),
		companyA: authztest.ID("company_a"), companyB: authztest.ID("company_b"), club: authztest.ID("club_a"),
		uniA: uuid.NewV7(), uniB: uuid.NewV7(),
	}
	for _, id := range []uuid.UUID{e.companyA, e.companyB} {
		e.orgs.orgs[id] = orgs.OrgSummary{ID: id, Kind: orgs.KindCompany, Status: orgs.StatusActive, Name: "Şirket"}
		e.ents.entitled[id] = true
	}
	e.orgs.orgs[e.club] = orgs.OrgSummary{ID: e.club, Kind: orgs.KindClub, Status: orgs.StatusActive, Name: "Kulüp"}
	mod, err := discovery.New(discovery.Deps{
		Students: e.students, Ranking: e.ranking, Entitlements: e.ents, Orgs: e.orgs, Audit: e.audit,
		Tx: db.NopTx{}, Pseudonyms: e.ps, Log: obs.Discard(),
	})
	if err != nil {
		t.Fatal(err)
	}
	e.svc = mod.Service
	return e
}

// add creates n discoverable students at uni with the given skills and
// languages and returns their IDs.
func (e *env) add(n int, uni uuid.UUID, skills []uuid.UUID, languages ...string) []uuid.UUID {
	e.students.mu.Lock()
	defer e.students.mu.Unlock()
	ids := make([]uuid.UUID, n)
	for i := range ids {
		ids[i] = uuid.NewV7()
		e.students.students[ids[i]] = &student{uni: uni, skills: slices.Clone(skills), languages: slices.Clone(languages), discoverable: true}
	}
	return ids
}

func (e *env) setDiscoverable(id uuid.UUID, on bool) {
	e.students.mu.Lock()
	defer e.students.mu.Unlock()
	e.students.students[id].discoverable = on
}

// userOf maps the anon IDs of a page back to the students (through the
// pseudonyms of company org).
func (e *env) usersOf(t *testing.T, org uuid.UUID, p discovery.Page) []uuid.UUID {
	t.Helper()
	all := e.students.sorted()
	out := make([]uuid.UUID, 0, len(p.Items))
	for _, it := range p.Items {
		found := false
		for _, id := range all {
			if ok, _ := e.ps.Matches(it.AnonID, org, id); ok {
				out, found = append(out, id), true
				break
			}
		}
		if !found {
			t.Fatalf("anon_id %q matches no student", it.AnonID)
		}
	}
	return out
}

func persona(t *testing.T, name string) authz.Actor {
	t.Helper()
	p, ok := authztest.Get(name)
	if !ok {
		t.Fatalf("unknown persona %s", name)
	}
	return p.Actor
}

// newHTTP serves the discovery operations of e with the persona resolver.
func newHTTP(t *testing.T, e *env) *httpx.App { return newHTTPMetrics(t, e, nil) }

// newHTTPMetrics is newHTTP with the Prometheus registry m (nil: off).
func newHTTPMetrics(t *testing.T, e *env, m *obs.Metrics) *httpx.App {
	t.Helper()
	app, err := httpx.New(httpx.Deps{Log: obs.Discard(), Metrics: m, Actors: authztest.NewResolver(authztest.Personas()...)})
	if err != nil {
		t.Fatal(err)
	}
	discovery.Register(app.V1, e.svc)
	return app
}

func get(t *testing.T, app *httpx.App, persona, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), "GET", path, nil)
	if p, _ := authztest.Get(persona); p.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.Token)
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	return rec
}
