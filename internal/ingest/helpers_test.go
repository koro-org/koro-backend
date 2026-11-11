package ingest_test

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"uuid"

	"koro/internal/audit/audittest"
	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/directory"
	"koro/internal/events"
	"koro/internal/ingest"
	"koro/internal/notify"
	"koro/internal/orgs"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
)

// env is the ingest module over its in-memory store, the real orgs and
// directory modules (in-memory) and fakes for the web, the drafts and the
// notices. No network, no model.
type env struct {
	clk     *clock.Fake
	store   *ingest.MemStore
	fetch   *ingest.MemFetcher
	ext     *scriptedExtractor
	audit   *audittest.Recorder
	orgs    orgs.Service
	orgsMem *orgs.MemStore
	dir     directory.Service
	dirMem  *directory.MemStore
	domains *mailDomains
	drafts  *fakeDrafts
	sender  *fakeSender
	svc     ingest.Service
	mod     ingest.Module
}

// mailDomains is directory with extra e-mail domains: the fixtures' club
// addresses are example.org (koro-backend is public), which directory's
// .edu.tr rule never registers.
type mailDomains struct {
	directory.Service
	mu    sync.Mutex
	extra map[string]uuid.UUID
}

func (m *mailDomains) UniversityByEmailDomain(ctx context.Context, domain string) (uuid.UUID, bool, error) {
	m.mu.Lock()
	id, ok := m.extra[domain]
	m.mu.Unlock()
	if ok {
		return id, true, nil
	}
	return m.Service.UniversityByEmailDomain(ctx, domain)
}

// scriptedExtractor runs the fake extractor unless a script is set, and
// counts its calls.
type scriptedExtractor struct {
	mu     sync.Mutex
	calls  int
	docs   [][]ingest.Document
	script func(req ingest.ExtractRequest) (ingest.Extraction, error)
}

func (x *scriptedExtractor) Name() string { return "scripted" }

func (x *scriptedExtractor) Extract(ctx context.Context, req ingest.ExtractRequest) (ingest.Extraction, error) {
	x.mu.Lock()
	x.calls++
	x.docs = append(x.docs, req.Docs)
	script := x.script
	x.mu.Unlock()
	if script != nil {
		return script(req)
	}
	return ingest.FakeExtractor{}.Extract(ctx, req)
}

func (x *scriptedExtractor) count() int {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.calls
}

type fakeDrafts struct {
	mu     sync.Mutex
	inputs []events.EventInput
	actors []authz.Actor
	fail   error
}

func (f *fakeDrafts) CreateEvent(_ context.Context, a authz.Actor, orgID uuid.UUID, in events.EventInput) (events.EventView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return events.EventView{}, f.fail
	}
	f.inputs, f.actors = append(f.inputs, in), append(f.actors, a)
	return events.EventView{Event: events.Event{ID: uuid.NewV7(), OrgID: orgID, Title: in.Title}}, nil
}

type fakeSender struct {
	mu   sync.Mutex
	msgs []notify.Message
}

func (f *fakeSender) Send(_ context.Context, m notify.Message) (uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = append(f.msgs, m)
	return uuid.NewV7(), nil
}

func newEnv(t *testing.T, cfg ingest.Config) *env {
	t.Helper()
	clk := clock.NewFake(t0)
	log := obs.Discard()
	e := &env{clk: clk, store: ingest.NewMemStore(clk.Now), fetch: ingest.NewMemFetcher(), ext: &scriptedExtractor{},
		audit: &audittest.Recorder{}, orgsMem: orgs.NewMemStore(clk.Now), drafts: &fakeDrafts{}, sender: &fakeSender{}}
	tx := db.NopTx{}
	out := outbox.NewMem(clk)
	idem := idempotency.New(idempotency.Deps{Tx: tx, Store: idempotency.NewMemStore(), Clock: clk, Log: log})
	om, err := orgs.New(orgs.Deps{Store: e.orgsMem, Tx: tx, Outbox: out, Audit: e.audit, Idempotency: idem, Clock: clk, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	e.orgs = om.Service
	e.dirMem = directory.NewMemStore(clk.Now, func(ctx context.Context, id uuid.UUID) (directory.OrgRef, bool) {
		o, err := e.orgsMem.GetOrg(ctx, id)
		if err != nil {
			return directory.OrgRef{}, false
		}
		return directory.OrgRef{Name: o.Name, Slug: o.Slug, Status: string(o.Status)}, true
	})
	dm, err := directory.New(directory.Deps{Store: e.dirMem, Tx: tx, Outbox: out, Queue: queue.NewMem(), Audit: e.audit,
		Orgs: om.Service, Idempotency: idem, Clock: clk, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	e.dir = dm.Service
	e.domains = &mailDomains{Service: dm.Service, extra: map[string]uuid.UUID{}}
	if cfg.MinConfidence == 0 {
		cfg.MinConfidence = 0.5
	}
	cfg.FetchEnabled = true
	mod, err := ingest.New(ingest.Deps{
		Store: e.store, Tx: tx, Directory: e.domains, Orgs: e.orgs, Fetcher: e.fetch, Extractor: e.ext, Audit: e.audit,
		Clock: clk, Log: log, Config: cfg, Idempotency: idem, Drafts: e.drafts, Sender: e.sender,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.mod, e.svc = mod, mod.Service
	return e
}

// university imports a university; mailDomain (example.org form) is the
// domain its club addresses use in the fixtures.
func (e *env) university(t *testing.T, code, name, mailDomain string) directory.University {
	t.Helper()
	if _, err := e.dir.ImportReference(t.Context(), directory.ReferenceData{
		Universities: []directory.RefUniversity{{YokCode: code, Name: name, City: "İstanbul", Type: directory.UniversityState}},
	}); err != nil {
		t.Fatalf("import university: %v", err)
	}
	u, err := e.dirMem.UniversityByYokCode(t.Context(), code)
	if err != nil {
		t.Fatal(err)
	}
	e.domains.mu.Lock()
	e.domains.extra[mailDomain] = u.ID
	e.domains.mu.Unlock()
	return u
}

// fixture serves testdata/name at url.
func (e *env) fixture(t *testing.T, url, name, contentType string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	e.fetch.Set(url, ingest.Page{ContentType: contentType, Body: b})
}

// run makes every source due and runs one tick, the clock moved past the
// host gap first.
func (e *env) run(t *testing.T) ingest.TickResult {
	t.Helper()
	e.clk.Advance(61 * 1e9)
	r, err := e.svc.RunDue(t.Context())
	if err != nil {
		t.Fatalf("RunDue: %v", err)
	}
	return r
}

func admin(t *testing.T) authz.Actor {
	t.Helper()
	p, ok := authztest.Get("platform_admin")
	if !ok {
		t.Fatal("no platform_admin persona")
	}
	return p.Actor
}

// clubAdmin is a verified adult student holding club_admin of org.
func clubAdmin(org uuid.UUID) authz.Actor {
	return authz.Actor{Kind: authz.KindUser, UserID: uuid.NewV7(), SessionID: uuid.NewV7(), StudentStatus: authz.StudentVerified,
		Memberships: []authz.Membership{{OrgID: org, OrgKind: authz.OrgClub, Role: authz.MemberClubAdmin}}}
}

func member(org uuid.UUID) authz.Actor {
	return authz.Actor{Kind: authz.KindUser, UserID: uuid.NewV7(), SessionID: uuid.NewV7(), StudentStatus: authz.StudentVerified,
		Memberships: []authz.Membership{{OrgID: org, OrgKind: authz.OrgClub, Role: authz.MemberMember}}}
}
