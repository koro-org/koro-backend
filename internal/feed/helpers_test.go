package feed_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/events"
	"koro/internal/feed"
	"koro/internal/files"
	"koro/internal/orgs"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/problem"
	"koro/internal/students"
)

// t0 is the test clock's start (microsecond precision, like PostgreSQL).
var t0 = time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)

// auditLog records the audit events.
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

func (l *auditLog) of(action authz.Action) []audit.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []audit.Event
	for _, e := range l.events {
		if e.Action == string(action) {
			out = append(out, e)
		}
	}
	return out
}

// fakeOrgs is the org directory of the authztest orgs (all active) with
// the active memberships set per user.
type fakeOrgs struct {
	mu      sync.Mutex
	orgs    map[uuid.UUID]orgs.OrgSummary
	members map[uuid.UUID][]authz.Membership
}

func newFakeOrgs() *fakeOrgs {
	f := &fakeOrgs{orgs: map[uuid.UUID]orgs.OrgSummary{}, members: map[uuid.UUID][]authz.Membership{}}
	for _, o := range authztest.Orgs() {
		id := authztest.ID(o.Name)
		f.orgs[id] = orgs.OrgSummary{ID: id, Kind: orgs.Kind(o.Kind), Name: "Org " + o.Name,
			Slug: strings.ReplaceAll(o.Name, "_", "-"), Status: orgs.StatusActive}
	}
	return f
}

func (f *fakeOrgs) Get(_ context.Context, id uuid.UUID) (orgs.OrgSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.orgs[id]
	if !ok {
		return orgs.OrgSummary{}, orgs.ErrNotFound
	}
	return o, nil
}

func (f *fakeOrgs) GetMany(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]orgs.OrgSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[uuid.UUID]orgs.OrgSummary{}
	for _, id := range ids {
		if o, ok := f.orgs[id]; ok {
			out[id] = o
		}
	}
	return out, nil
}

func (f *fakeOrgs) MembershipsOf(_ context.Context, userID uuid.UUID) ([]authz.Membership, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]authz.Membership(nil), f.members[userID]...), nil
}

// setMembers replaces the active memberships of userID.
func (f *fakeOrgs) setMembers(userID uuid.UUID, ms ...authz.Membership) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.members[userID] = ms
}

func (f *fakeOrgs) setStatus(id uuid.UUID, st orgs.Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.orgs[id]
	o.Status = st
	f.orgs[id] = o
}

// fakeViewers is students.ViewerKeys: 'all' plus the keys set per user.
type fakeViewers struct {
	mu   sync.Mutex
	keys map[uuid.UUID][]string
}

func (v *fakeViewers) ViewerKeys(_ context.Context, id uuid.UUID) (students.ViewerKeys, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return students.ViewerKeys{Keys: append([]string{"all"}, v.keys[id]...)}, nil
}

// fakeEvents holds the events that may be shared.
type fakeEvents struct {
	mu     sync.Mutex
	events map[uuid.UUID]events.EventSummary
}

func (f *fakeEvents) Get(_ context.Context, id uuid.UUID) (events.EventSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.events[id]
	if !ok {
		return events.EventSummary{}, events.ErrNotFound
	}
	return e, nil
}

func (f *fakeEvents) put(e events.EventSummary) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events[e.ID] = e
}

// fakeFiles holds the uploads; Attach records the owner.
type fakeFiles struct {
	mu       sync.Mutex
	files    map[uuid.UUID]files.FileMeta
	attached map[uuid.UUID]uuid.UUID // file → post
}

func (f *fakeFiles) Get(_ context.Context, id uuid.UUID) (files.FileMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.files[id]
	if !ok {
		return files.FileMeta{}, files.ErrNotFound
	}
	return m, nil
}

func (f *fakeFiles) Attach(_ context.Context, _ authz.Actor, id uuid.UUID, o files.Owner) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if prev, ok := f.attached[id]; ok && prev != o.EntityID {
		return files.ErrAlreadyAttached
	}
	f.attached[id] = o.EntityID
	return nil
}

func (f *fakeFiles) PresignGet(_ context.Context, id uuid.UUID, _ time.Duration) (files.Download, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.files[id]
	if !ok || m.ScanStatus != files.ScanClean {
		return files.Download{}, files.ErrNotClean
	}
	return files.Download{URL: "https://files.example.test/" + id.String()}, nil
}

// upload adds a post_media upload of org.
func (f *fakeFiles) upload(org uuid.UUID, scan files.ScanStatus) uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := uuid.NewV7()
	f.files[id] = files.FileMeta{ID: id, Purpose: files.PurposePostMedia, OrgID: &org, Status: files.StatusActive, ScanStatus: scan}
	return id
}

func (f *fakeFiles) setScan(id uuid.UUID, scan files.ScanStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.files[id]
	m.ScanStatus = scan
	f.files[id] = m
}

// fakeClubs is directory's sensitive club set.
type fakeClubs struct {
	mu        sync.Mutex
	sensitive map[uuid.UUID]bool
}

func (f *fakeClubs) SensitiveOrgIDs(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[uuid.UUID]bool{}
	for _, id := range ids {
		if f.sensitive[id] {
			out[id] = true
		}
	}
	return out, nil
}

func (f *fakeClubs) set(id uuid.UUID, sensitive bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sensitive[id] = sensitive
}

type env struct {
	clk     *clock.Fake
	mem     *feed.MemStore
	out     *outbox.Mem
	audit   *auditLog
	orgs    *fakeOrgs
	viewers *fakeViewers
	events  *fakeEvents
	files   *fakeFiles
	clubs   *fakeClubs
	svc     feed.Service
	mod     feed.Module
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvWith(t, nil)
}

// newEnvWith is newEnv with the dependencies adjusted by tweak.
func newEnvWith(t *testing.T, tweak func(*feed.Deps)) *env {
	t.Helper()
	clk := clock.NewFake(t0)
	e := &env{
		clk: clk, mem: feed.NewMemStore(clk.Now), out: outbox.NewMem(clk), audit: &auditLog{}, orgs: newFakeOrgs(),
		viewers: &fakeViewers{keys: map[uuid.UUID][]string{}}, events: &fakeEvents{events: map[uuid.UUID]events.EventSummary{}},
		files: &fakeFiles{files: map[uuid.UUID]files.FileMeta{}, attached: map[uuid.UUID]uuid.UUID{}},
		clubs: &fakeClubs{sensitive: map[uuid.UUID]bool{}},
	}
	tx := db.NopTx{}
	idem := idempotency.New(idempotency.Deps{Tx: tx, Store: idempotency.NewMemStore(), Clock: clk, Log: obs.Discard()})
	d := feed.Deps{
		Store: e.mem, Tx: tx, Outbox: e.out, Audit: e.audit, Orgs: e.orgs, Idempotency: idem, Clock: clk,
		Log: obs.Discard(), Viewers: e.viewers, Events: e.events, Files: e.files, Clubs: e.clubs,
	}
	if tweak != nil {
		tweak(&d)
	}
	mod, err := feed.New(d)
	if err != nil {
		t.Fatal(err)
	}
	e.mod, e.svc = mod, mod.Service
	return e
}

func persona(t *testing.T, name string) authz.Actor {
	t.Helper()
	p, ok := authztest.Get(name)
	if !ok {
		t.Fatalf("unknown persona %s", name)
	}
	return p.Actor
}

func orgID(name string) uuid.UUID { return authztest.ID(name) }

// doc is a one-paragraph Tiptap document.
func doc(text string) []byte {
	return []byte(`{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"` + text + `"}]}]}`)
}

// textInput is a valid public text post.
func textInput(text string) feed.PostInput {
	return feed.PostInput{Kind: feed.KindText, Title: "Duyuru", Body: doc(text)}
}

// create makes a draft as actor in org.
func (e *env) create(t *testing.T, actor, org string, in feed.PostInput) feed.PostView {
	t.Helper()
	v, err := e.svc.CreatePost(t.Context(), persona(t, actor), orgID(org), in)
	if err != nil {
		t.Fatalf("CreatePost(%s in %s): %v", actor, org, err)
	}
	return v
}

// trust makes org trusted (its posts are published at once).
func (e *env) trust(org string) {
	e.mem.PutSettings(feed.OrgSettings{OrgID: orgID(org), TrustLevel: feed.TrustTrusted})
}

// published creates and publishes a post of org by actor (the org is
// trusted first) and advances the clock so publication times differ.
func (e *env) published(t *testing.T, actor, org string, in feed.PostInput) feed.PostView {
	t.Helper()
	e.trust(org)
	v := e.create(t, actor, org, in)
	p, err := e.svc.PublishPost(t.Context(), persona(t, actor), v.Post.ID)
	if err != nil {
		t.Fatalf("PublishPost: %v", err)
	}
	if p.Post.Status != feed.StatusPublished {
		t.Fatalf("status = %s", p.Post.Status)
	}
	e.clk.Advance(time.Second)
	return p
}

// feedIDs reads the whole feed of actor page by page (limit per page).
func (e *env) feedIDs(t *testing.T, a authz.Actor, q feed.FeedQuery) []uuid.UUID {
	t.Helper()
	var ids []uuid.UUID
	for range 100 {
		p, err := e.svc.GetFeed(t.Context(), a, q)
		if err != nil {
			t.Fatalf("GetFeed: %v", err)
		}
		for _, v := range p.Items {
			ids = append(ids, v.Post.ID)
		}
		if p.Next == nil {
			return ids
		}
		q.After = p.Next
	}
	t.Fatal("feed did not end")
	return nil
}

func wantCode(t *testing.T, err error, status int, code string) {
	t.Helper()
	p, ok := errors.AsType[*problem.Error](err)
	if !ok || p.Status() != status || p.Code() != code {
		t.Fatalf("error = %v, want %d %s", err, status, code)
	}
}

func wantNotFound(t *testing.T, err error) { t.Helper(); wantCode(t, err, 404, "not_found") }

func wantForbidden(t *testing.T, err error) { t.Helper(); wantCode(t, err, 403, "forbidden") }

// wantField fails unless err is a 422 validation error on loc.
func wantField(t *testing.T, err error, loc string) {
	t.Helper()
	p, ok := errors.AsType[*problem.Error](err)
	if !ok || p.Status() != 422 {
		t.Fatalf("error = %v, want 422 on %s", err, loc)
	}
	for _, f := range p.Fields() {
		if f.Location == loc {
			return
		}
	}
	t.Fatalf("error fields = %+v, want %s", p.Fields(), loc)
}

func contains(ids []uuid.UUID, id uuid.UUID) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}
