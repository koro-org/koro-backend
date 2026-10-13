package catalog_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/catalog"
	"koro/internal/directory"
	"koro/internal/events"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/orgs"
	"koro/internal/parties"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/httpx"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/outbox"
	"koro/internal/platform/problem"
	"koro/internal/platform/queue"
)

// t0 is the test clock's start (microsecond precision, like PostgreSQL).
var t0 = time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)

func org(name string) uuid.UUID { return authztest.ID(name) }

func ptr[T any](v T) *T { return &v }

// auditLog records the events a catalog service writes.
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

func (l *auditLog) of(act authz.Action) []audit.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []audit.Event
	for _, e := range l.events {
		if e.Action == string(act) {
			out = append(out, e)
		}
	}
	return out
}

// fakeOrgs is orgs.OrgDirectory over the matrix orgs (authztest.Orgs).
type fakeOrgs struct{ m map[uuid.UUID]orgs.OrgSummary }

func newFakeOrgs() *fakeOrgs {
	f := &fakeOrgs{m: map[uuid.UUID]orgs.OrgSummary{}}
	for _, o := range authztest.Orgs() {
		id := authztest.ID(o.Name)
		f.m[id] = orgs.OrgSummary{ID: id, Kind: orgs.Kind(o.Kind), Name: "Org " + o.Name, Status: orgs.StatusActive, Slug: o.Name}
	}
	return f
}

func (f *fakeOrgs) Get(_ context.Context, id uuid.UUID) (orgs.OrgSummary, error) {
	o, ok := f.m[id]
	if !ok {
		return orgs.OrgSummary{}, orgs.ErrNotFound
	}
	return o, nil
}

func (f *fakeOrgs) GetMany(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]orgs.OrgSummary, error) {
	out := map[uuid.UUID]orgs.OrgSummary{}
	for _, id := range ids {
		if o, ok := f.m[id]; ok {
			out[id] = o
		}
	}
	return out, nil
}

func (f *fakeOrgs) ActiveMemberIDs(_ context.Context, orgID uuid.UUID) ([]uuid.UUID, error) {
	var out []uuid.UUID
	for _, p := range append(authztest.Personas(), authztest.Helpers()...) {
		if p.Actor.MemberOf(orgID) {
			out = append(out, p.User())
		}
	}
	return out, nil
}

func (f *fakeOrgs) HasRole(_ context.Context, orgID, userID uuid.UUID, roles ...authz.MemberRole) (bool, error) {
	for _, p := range append(authztest.Personas(), authztest.Helpers()...) {
		if p.User() == userID && p.Actor.HasOrgRole(orgID, roles...) {
			return true, nil
		}
	}
	return false, nil
}

// fakeDirectory knows the clubs and universities of authztest (the
// universities row of uni_x is authztest.UniversityID("uni_x")).
type fakeDirectory struct{}

func (fakeDirectory) GetClub(_ context.Context, orgID uuid.UUID) (directory.Club, error) {
	for _, o := range authztest.Orgs() {
		if o.Kind == authz.OrgClub && authztest.ID(o.Name) == orgID {
			return directory.Club{OrgID: orgID, UniversityID: authztest.UniversityID(o.University)}, nil
		}
	}
	return directory.Club{}, directory.ErrNotFound
}

func (fakeDirectory) GetUniversity(_ context.Context, id uuid.UUID) (directory.University, error) {
	for _, o := range authztest.Orgs() {
		if o.Kind == authz.OrgUniversity && authztest.UniversityID(o.Name) == id {
			return directory.University{ID: id, OrgID: authztest.ID(o.Name), IsActive: true}, nil
		}
	}
	return directory.University{}, directory.ErrNotFound
}

func (d fakeDirectory) UniversityOrgs(ctx context.Context, universityID uuid.UUID) ([]uuid.UUID, error) {
	u, err := d.GetUniversity(ctx, universityID)
	if errors.Is(err, directory.ErrNotFound) {
		return []uuid.UUID{}, nil // an unknown university has no orgs
	}
	out := []uuid.UUID{u.OrgID}
	for _, o := range authztest.Orgs() {
		if o.Kind == authz.OrgClub && authztest.UniversityID(o.University) == universityID {
			out = append(out, authztest.ID(o.Name))
		}
	}
	return out, nil
}

// fakeEvents holds event summaries by ID.
type fakeEvents struct {
	mu sync.Mutex
	m  map[uuid.UUID]events.EventSummary
}

func (f *fakeEvents) add(orgName string, st events.Status) uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := uuid.NewV7()
	f.m[id] = events.EventSummary{ID: id, OrgID: org(orgName), Title: "Bahar Şenliği", Status: st, StartsAt: t0.Add(30 * 24 * time.Hour)}
	return id
}

func (f *fakeEvents) set(id uuid.UUID, st events.Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.m[id]
	e.Status = st
	f.m[id] = e
}

func (f *fakeEvents) Get(_ context.Context, id uuid.UUID) (events.EventSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.m[id]
	if !ok {
		return events.EventSummary{}, events.ErrNotFound
	}
	return e, nil
}

// fakePayees holds payees and which of them have an approved account.
type fakePayees struct {
	mu       sync.Mutex
	m        map[uuid.UUID]parties.Payee
	approved map[uuid.UUID]bool
}

// add registers a payee of the org: active or not, with or without an
// approved bank account.
func (f *fakePayees) add(orgName string, st parties.PayeeStatus, approvedAccount bool) uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := uuid.NewV7()
	f.m[id] = parties.Payee{ID: id, OrgID: org(orgName), Status: st, IsDefault: true, ValidFrom: t0.Add(-time.Hour)}
	f.approved[id] = approvedAccount
	return id
}

func (f *fakePayees) setApproved(id uuid.UUID, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.approved[id] = ok
}

func (f *fakePayees) Get(_ context.Context, id uuid.UUID) (parties.Payee, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.m[id]
	if !ok {
		return parties.Payee{}, parties.ErrNotFound
	}
	return p, nil
}

func (f *fakePayees) DefaultFor(_ context.Context, orgID uuid.UUID) (parties.Payee, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.m {
		if p.OrgID == orgID && p.IsDefault && p.Status == parties.PayeeActive {
			return p, true, nil
		}
	}
	return parties.Payee{}, false, nil
}

func (f *fakePayees) ApprovedAccount(_ context.Context, id uuid.UUID) (parties.BankAccount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.approved[id] {
		return parties.BankAccount{}, parties.ErrNoApprovedAccount
	}
	return parties.BankAccount{ID: uuid.NewV7(), PayeeID: id, Status: parties.AccountApproved}, nil
}

func (f *fakePayees) IsActive(_ context.Context, id uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.m[id]
	if !ok {
		return false, parties.ErrNotFound
	}
	return p.Status == parties.PayeeActive, nil
}

// reauthPassword is the proof fakeReauth accepts.
const reauthPassword = "test-reauth-password"

type fakeReauth struct {
	mu    sync.Mutex
	calls []uuid.UUID
}

func (f *fakeReauth) Verify(_ context.Context, userID uuid.UUID, p identity.ReauthProof) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, userID)
	f.mu.Unlock()
	switch {
	case p.Password == "" && p.TOTPCode == "":
		return "", identity.ErrReauthRequired
	case p.Password != reauthPassword:
		return "", identity.ErrReauthFailed
	}
	return "password", nil
}

func proof() identity.ReauthProof { return identity.ReauthProof{Password: reauthPassword} }

// fakeSender records the notices (deduplicated by key).
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
	if !f.keys[m.DedupeKey] {
		f.keys[m.DedupeKey] = true
		f.sent = append(f.sent, m)
	}
	return uuid.NewV7(), nil
}

func (f *fakeSender) all() []notify.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]notify.Message(nil), f.sent...)
}

// fakeUsers resolves every persona to an address of its name.
type fakeUsers struct{}

func (fakeUsers) Contact(_ context.Context, id uuid.UUID) (identity.UserContact, error) {
	for _, p := range append(authztest.Personas(), authztest.Helpers()...) {
		if p.User() == id && p.Actor.IsUser() {
			return identity.UserContact{ID: id, Email: p.Name + "@koro.test", FirstName: p.Name, Locale: "tr"}, nil
		}
	}
	return identity.UserContact{}, identity.ErrNotFound
}

// fakeStaff: platform_admin and platform_admin_2; finance_admin and finance_admin_2.
type fakeStaff struct{}

func (fakeStaff) PlatformAdmins(context.Context) ([]uuid.UUID, error) {
	return []uuid.UUID{authztest.ID("platform_admin"), authztest.ID("platform_admin_2")}, nil
}

func (fakeStaff) FinanceAdmins(context.Context) ([]uuid.UUID, error) {
	return []uuid.UUID{authztest.ID("finance_admin"), authztest.ID("finance_admin_2")}, nil
}

// fakeHeld is the per-buyer count of orders (BE-040) of the tests.
type fakeHeld struct {
	mu sync.Mutex
	m  map[[2]uuid.UUID]int
}

func (f *fakeHeld) set(offering, buyer uuid.UUID, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[[2]uuid.UUID{offering, buyer}] = n
}

func (f *fakeHeld) held(_ context.Context, offering, buyer uuid.UUID) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.m[[2]uuid.UUID{offering, buyer}], nil
}

// env is a catalog module over the in-memory store.
type env struct {
	clk    *clock.Fake
	store  *catalog.MemStore
	audit  *auditLog
	orgs   *fakeOrgs
	events *fakeEvents
	payees *fakePayees
	held   *fakeHeld
	outbox *outbox.Mem
	queue  *queue.Mem
	reauth *fakeReauth
	sender *fakeSender
	logs   *bytes.Buffer
	svc    catalog.Service
	mod    catalog.Module

	// a sellable service, the payee of club_a (active, approved account)
	booth, quota uuid.UUID
	payeeA       uuid.UUID
}

func newEnv(t *testing.T) *env {
	t.Helper()
	clk := clock.NewFake(t0)
	e := &env{clk: clk, store: catalog.NewMemStore(clk.Now), audit: &auditLog{}, orgs: newFakeOrgs(),
		events: &fakeEvents{m: map[uuid.UUID]events.EventSummary{}},
		payees: &fakePayees{m: map[uuid.UUID]parties.Payee{}, approved: map[uuid.UUID]bool{}},
		held:   &fakeHeld{m: map[[2]uuid.UUID]int{}}, outbox: outbox.NewMem(clk), queue: queue.NewMem(),
		reauth: &fakeReauth{}, sender: &fakeSender{}, logs: &bytes.Buffer{}}
	tx := db.NopTx{}
	mod, err := catalog.New(catalog.Deps{
		Store: e.store, Tx: tx, Audit: e.audit, Orgs: e.orgs, Outbox: e.outbox, Queue: e.queue, Clock: clk,
		Log:         slog.New(slog.DiscardHandler),
		Idempotency: idempotency.New(idempotency.Deps{Tx: tx, Store: idempotency.NewMemStore(), Clock: clk, Log: slog.New(slog.DiscardHandler)}),
		Payees:      e.payees, Directory: fakeDirectory{}, Events: e.events, Reauth: e.reauth, BuyerHeld: e.held.held,
		Sender: e.sender, Users: fakeUsers{}, Staff: fakeStaff{},
	})
	if err != nil {
		t.Fatalf("catalog.New: %v", err)
	}
	e.mod, e.svc = mod, mod.Service
	e.booth = e.service(t, "booth_stand", catalog.CategoryBooth, catalog.FulfillOrgBound, true)
	e.quota = e.service(t, "targeted_post_5", catalog.CategoryTargetedPostQuota, catalog.FulfillPlatformNative, true)
	e.payeeA = e.payees.add("club_a", parties.PayeeActive, true)
	return e
}

// service writes a service as finance_admin (reviewed when reviewed).
func (e *env) service(t *testing.T, code string, cat catalog.Category, ft catalog.FulfillmentType, reviewed bool) uuid.UUID {
	t.Helper()
	a := actor(t, "finance_admin")
	in := catalog.ServiceInput{Code: code, NameTR: "Hizmet " + code, Category: cat, FulfillmentType: ft, AccountingReviewed: reviewed,
		AccountingReviewedRef: "MM-2026-01"}
	if ft == catalog.FulfillPlatformNative {
		in.EntitlementTemplate = map[string]any{"feature": "targeted_post", "kind": "quota", "quantity_per_unit": float64(5), "valid_days": float64(90)}
	}
	v, err := e.svc.AdminCreateService(ctxOf(t, a), a, in)
	if err != nil {
		t.Fatalf("AdminCreateService: %v", err)
	}
	return v.ID
}

// defaultPolicy writes the default policy (model A, 10 % from the seller)
// valid from now, as finance_admin.
func (e *env) defaultPolicy(t *testing.T) catalog.CommercePolicy {
	t.Helper()
	a := actor(t, "finance_admin")
	p, err := e.svc.AdminCreatePolicy(ctxOf(t, a), a, catalog.DefaultPolicy(1000), proof())
	if err != nil {
		t.Fatalf("AdminCreatePolicy: %v", err)
	}
	return p
}

// offering creates a draft offering of orgName as by.
func (e *env) offering(t *testing.T, by, orgName string, mut func(*catalog.OfferingInput)) catalog.OfferingView {
	t.Helper()
	in := catalog.OfferingInput{ServiceID: e.booth, Title: "Bahar Şenliği Stant Paketi", UnitPriceAmount: 1_000_000,
		Deliverables: []catalog.Deliverable{{Key: "booth_3x3", Label: "3x3 stant alanı"}, {Key: "logo_on_banner", Label: "Afişte logo"}}}
	if mut != nil {
		mut(&in)
	}
	a := actor(t, by)
	v, err := e.svc.CreateOffering(ctxOf(t, a), a, org(orgName), in)
	if err != nil {
		t.Fatalf("CreateOffering: %v", err)
	}
	return v
}

// active takes a draft through submission (by its creator) and approval
// (platform_admin_2).
func (e *env) active(t *testing.T, by string, id uuid.UUID) catalog.OfferingView {
	t.Helper()
	a := actor(t, by)
	if _, err := e.svc.SubmitOffering(ctxOf(t, a), a, id); err != nil {
		t.Fatalf("SubmitOffering: %v", err)
	}
	pad := actor(t, "platform_admin_2")
	v, err := e.svc.AdminDecideOffering(ctxOf(t, pad), pad, id, catalog.Decision{Approve: true})
	if err != nil {
		t.Fatalf("AdminDecideOffering: %v", err)
	}
	return v
}

func actor(t *testing.T, name string) authz.Actor {
	t.Helper()
	p, ok := authztest.Get(name)
	if !ok {
		t.Fatalf("unknown persona %s", name)
	}
	return p.Actor
}

func ctxOf(t *testing.T, a authz.Actor) context.Context { return authz.WithActor(t.Context(), a) }

func wantErr(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func problemOf(err error) (int, string) {
	if p, ok := errors.AsType[*problem.Error](err); ok {
		return p.Status(), p.Code()
	}
	return 0, ""
}

func wantProblem(t *testing.T, err error, status int, code string) {
	t.Helper()
	if s, c := problemOf(err); s != status || c != code {
		t.Fatalf("error = %v (%d %s), want %d %s", err, s, c, status, code)
	}
}

// ---- HTTP

func newHTTP(t *testing.T, e *env) *httpx.App {
	t.Helper()
	app, err := httpx.New(httpx.Deps{Log: slog.New(slog.DiscardHandler),
		Actors: authztest.NewResolver(append(authztest.Personas(), authztest.Helpers()...)...)})
	if err != nil {
		t.Fatal(err)
	}
	app.API.UseMiddleware(idempotency.Middleware(app.API))
	catalog.Register(app.V1, e.svc)
	return app
}

func call(t *testing.T, app *httpx.App, persona, method, path string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	return callH(t, app, persona, method, path, body, nil)
}

// callH is call with request headers.
func callH(t *testing.T, app *httpx.App, persona, method, path string, body any, hdr map[string]string) (*httptest.ResponseRecorder, map[string]any) {
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

func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }
