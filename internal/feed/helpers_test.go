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
	"koro/internal/consent"
	"koro/internal/directory"
	"koro/internal/entitlements"
	"koro/internal/events"
	"koro/internal/feed"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/orgs"
	"koro/internal/outreach"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/outbox"
	"koro/internal/platform/problem"
	"koro/internal/platform/queue"
	"koro/internal/platform/ratelimit"
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

func (f *fakeOrgs) ActiveMemberIDs(_ context.Context, orgID uuid.UUID) ([]uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []uuid.UUID
	for u, ms := range f.members {
		for _, m := range ms {
			if m.OrgID == orgID {
				out = append(out, u)
				break
			}
		}
	}
	return out, nil
}

func (f *fakeOrgs) HasRole(_ context.Context, orgID, userID uuid.UUID, roles ...authz.MemberRole) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.members[userID] {
		for _, r := range roles {
			if m.OrgID == orgID && m.Role == r {
				return true, nil
			}
		}
	}
	return false, nil
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
	deleted  []uuid.UUID
}

func (f *fakeFiles) Delete(_ context.Context, _ authz.Actor, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.files, id)
	delete(f.attached, id)
	f.deleted = append(f.deleted, id)
	return nil
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

// fakeClubs is directory's sensitive club set (feed's Clubs) and its
// universities (outreach's Catalog of the real targeting).
type fakeClubs struct {
	mu        sync.Mutex
	sensitive map[uuid.UUID]bool
	unis      map[uuid.UUID]directory.University
}

func (f *fakeClubs) GetUniversity(_ context.Context, id uuid.UUID) (directory.University, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.unis[id]
	if !ok {
		return directory.University{}, directory.ErrNotFound
	}
	return u, nil
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

// fakeAudience is students' count_segment over profiles (outreach's
// Audience of the real targeting): a profile counts when it is eligible and
// its keys match every dimension of the spec; a sensitive club's key never
// counts; below 10 there is no number.
type fakeAudience struct {
	mu        sync.Mutex
	profiles  [][]string
	sensitive map[string]bool
}

func (f *fakeAudience) add(n int, keys ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for range n {
		f.profiles = append(f.profiles, keys)
	}
}

func (f *fakeAudience) Count(_ context.Context, spec students.AudienceSpec) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	dims, keys := map[string]bool{}, map[string]bool{}
	for _, k := range spec.Keys {
		dim, _, _ := strings.Cut(k, ":")
		dims[dim] = true
		if !f.sensitive[k] {
			keys[k] = true
		}
	}
	n := 0
	for _, p := range f.profiles {
		matched := map[string]bool{}
		for _, k := range p {
			if keys[k] {
				dim, _, _ := strings.Cut(k, ":")
				matched[dim] = true
			}
		}
		if len(matched) > 0 && len(matched) == len(dims) {
			n++
		}
	}
	if n < 10 {
		return 0, students.ErrAudienceTooSmall
	}
	return n, nil
}

// fakeSender records the notices.
type fakeSender struct {
	mu   sync.Mutex
	sent []notify.Message
	keys map[string]bool
}

func (f *fakeSender) Send(_ context.Context, m notify.Message) (uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.keys == nil {
		f.keys = map[string]bool{}
	}
	if f.keys[m.DedupeKey] {
		return uuid.NewV7(), nil
	}
	f.keys[m.DedupeKey] = true
	f.sent = append(f.sent, m)
	return uuid.NewV7(), nil
}

// to returns the recipients of tmpl.
func (f *fakeSender) to(tmpl string) []uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []uuid.UUID
	for _, m := range f.sent {
		if m.Template == tmpl {
			out = append(out, *m.UserID)
		}
	}
	return out
}

// dispatchPeers are the outreach dependencies of the dispatch (BE-035)
// the feed's tests never reach: the recipient walk, the club members, the
// accounts and the consents. The feed only schedules and withdraws.
type dispatchPeers struct{}

func (dispatchPeers) Stream(context.Context, students.AudienceSpec, func(uuid.UUID) error) error {
	return nil
}
func (dispatchPeers) ActiveMemberIDs(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}
func (dispatchPeers) GetMany(context.Context, []uuid.UUID) (map[uuid.UUID]identity.UserSummary, error) {
	return map[uuid.UUID]identity.UserSummary{}, nil
}
func (dispatchPeers) Contact(context.Context, uuid.UUID) (identity.UserContact, error) {
	return identity.UserContact{}, identity.ErrNotFound
}
func (dispatchPeers) FilterActive(context.Context, []uuid.UUID, consent.Purpose) ([]uuid.UUID, error) {
	return nil, nil
}

// latePosts is outreach's view of the feed, bound once the feed is built
// (cmd/koro does the same).
type latePosts struct{ svc *feed.Service }

func (p *latePosts) DispatchPost(ctx context.Context, id uuid.UUID) (outreach.PostInfo, error) {
	s, err := (*p.svc).Get(ctx, id)
	if err != nil {
		return outreach.PostInfo{}, err
	}
	return s.DispatchInfo(), nil
}

// fakeStaff lists the moderators and platform admins in effect.
type fakeStaff struct {
	moderators, admins []uuid.UUID
}

func (f *fakeStaff) Moderators(context.Context) ([]uuid.UUID, error)     { return f.moderators, nil }
func (f *fakeStaff) PlatformAdmins(context.Context) ([]uuid.UUID, error) { return f.admins, nil }

type env struct {
	clk      *clock.Fake
	mem      *feed.MemStore
	out      *outbox.Mem
	queue    *queue.Mem
	audit    *auditLog
	orgs     *fakeOrgs
	viewers  *fakeViewers
	events   *fakeEvents
	files    *fakeFiles
	clubs    *fakeClubs
	audience *fakeAudience
	ents     entitlements.Service
	sender   *fakeSender
	staff    *fakeStaff
	svc      feed.Service
	mod      feed.Module

	dispatches *outreach.MemStore // outreach's store (BE-035: the dispatch of targeted posts)
	targeting  outreach.Service
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
		clk: clk, mem: feed.NewMemStore(clk.Now), out: outbox.NewMem(clk), queue: queue.NewMem(), audit: &auditLog{},
		orgs: newFakeOrgs(), viewers: &fakeViewers{keys: map[uuid.UUID][]string{}},
		events: &fakeEvents{events: map[uuid.UUID]events.EventSummary{}},
		files:  &fakeFiles{files: map[uuid.UUID]files.FileMeta{}, attached: map[uuid.UUID]uuid.UUID{}},
		clubs: &fakeClubs{sensitive: map[uuid.UUID]bool{}, unis: map[uuid.UUID]directory.University{
			uniA: {ID: uniA, OrgID: orgID("uni_a"), Name: "Üniversite A", IsActive: true},
		}},
		audience: &fakeAudience{sensitive: map[string]bool{}},
		sender:   &fakeSender{},
		staff:    &fakeStaff{moderators: []uuid.UUID{orgID("moderator")}, admins: []uuid.UUID{orgID("platform_admin")}},
	}
	tx := db.NopTx{}
	idem := idempotency.New(idempotency.Deps{Tx: tx, Store: idempotency.NewMemStore(), Clock: clk, Log: obs.Discard()})
	ents, err := entitlements.New(entitlements.Deps{Store: entitlements.NewMemStore(clk.Now), Tx: tx, Audit: e.audit,
		Idempotency: idem, Clock: clk, Log: obs.Discard()})
	if err != nil {
		t.Fatal(err)
	}
	e.ents = ents.Service
	posts := &latePosts{svc: &e.svc}
	e.dispatches = outreach.NewMemStore(clk.Now)
	targeting, err := outreach.New(outreach.Deps{Store: e.dispatches, Tx: tx, Audit: e.audit,
		Audience: e.audience, Catalog: e.clubs, Orgs: e.orgs, Limiter: ratelimit.NewMem(clk), Idempotency: idem,
		Clock: clk, Log: obs.Discard(), Outbox: e.out, Entitlements: e.ents, Recipients: dispatchPeers{},
		Members: dispatchPeers{}, Users: dispatchPeers{}, Consents: dispatchPeers{}, Sender: e.sender, Posts: posts,
		LinkBaseURL: "https://koro.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	e.targeting = targeting.Service
	d := feed.Deps{
		Store: e.mem, Tx: tx, Outbox: e.out, Queue: e.queue, Audit: e.audit, Orgs: e.orgs, Idempotency: idem, Clock: clk,
		Log: obs.Discard(), Viewers: e.viewers, Events: e.events, Files: e.files, Clubs: e.clubs,
		Targeting: targeting.Service, Entitlements: e.ents, Sender: e.sender, Staff: e.staff,
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

// uniA is the university of uni_a (students' uni: keys).
var uniA = authztest.UniversityID("uni_a")

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
